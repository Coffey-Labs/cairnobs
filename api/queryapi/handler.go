// Package queryapi is Sentry's query API: a single POST /query endpoint
// accepting either the pipe syntax or raw SQL, compiled by
// querylang/planner and executed by querylang/executor. Replaces Phase
// 0/1's two separate placeholder endpoints (raw-SQL-only /query,
// free-text-only /search) -- see /docs/query-language-design.md.
//
// Still plain net/http, not the pinned gRPC+REST-gateway pattern, for
// the same reason as Phase 0/1: this is one endpoint, and the
// proto/annotations/codegen machinery doesn't buy much at that size.
// `/api` does speak gRPC internally (to /search) — this simplification
// is about the public-facing surface only.
package queryapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sentry/sentry/api/ai/costguard"
	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/internal/querylang/planner"
	"github.com/sentry/sentry/api/querylang/executor"
)

// AuditLogger is core's extension point for query audit logging --
// deliberately minimal and tenant-agnostic, since core has no concept of
// tenants (see /docs/phase-4-isolation-design.md: that mechanism lives
// entirely in enterprise/). enterprise/internal/audit implements this
// against the real hash-chained, append-only store; a nil AuditLogger
// (the default for a single-tenant deployment without enterprise/
// configured) means no audit logging happens and core behaves exactly
// as it did in Phases 0-3.
//
// Tenant/user identity is deliberately NOT a field on QueryAuditEntry --
// once Phase 4 task 5's auth middleware wraps this handler, it attaches
// that identity to the request's context.Context via
// enterprise/internal/tenant, and LogQuery's ctx parameter is the same
// context the request carried, so an enterprise-side implementation
// reads identity from ctx rather than this interface growing
// tenant-awareness. Per /docs/phase-4-isolation-design.md's audit
// section, this is a fail-open path: a LogQuery error is logged but
// never fails the HTTP response for a routine read query.
type AuditLogger interface {
	LogQuery(ctx context.Context, entry QueryAuditEntry) error
}

type QueryAuditEntry struct {
	Query    string
	Language string
	RowCount int
	Duration time.Duration
	Success  bool
	Error    string
}

type Handler struct {
	logger       *slog.Logger
	sqlRunner    executor.SQLRunner
	search       executor.SearchClient
	queryTimeout time.Duration
	audit        AuditLogger
	authorizer   authz.Authorizer
}

// audit and authorizer may both be nil -- see AuditLogger's doc comment
// and authz.RequireRoleOrService's nil-safety. /query allows RoleViewer
// (human sessions) or the alerting service identity (RoleService) --
// it's the one endpoint /alerting's evaluator legitimately calls, per
// /docs/phase-4-isolation-design.md's alerting service-identity design.
func NewHandler(logger *slog.Logger, sqlRunner executor.SQLRunner, search executor.SearchClient, queryTimeout time.Duration, audit AuditLogger, authorizer authz.Authorizer) *Handler {
	return &Handler{logger: logger, sqlRunner: sqlRunner, search: search, queryTimeout: queryTimeout, audit: audit, authorizer: authorizer}
}

// RegisterRoutes adds this handler's routes onto a shared mux. Phase 3
// introduced a second handler package (dashboards), so CORS is
// now applied once, by main.go, around the fully-assembled mux rather
// than by each handler wrapping itself individually -- see
// httpserver.WithCORS.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /query", authz.RequireRoleOrService(h.authorizer, authz.RoleViewer, h.handleQuery))
	mux.HandleFunc("GET /healthz", h.handleHealthz)
}

func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

type queryRequest struct {
	Query string `json:"query"`
	// Language overrides auto-detection ("" / omitted). "sql" or "spl" --
	// see planner.Language and /docs/query-language-design.md's
	// "Detection" section for why this exists: the rare case a pipe
	// query legitimately starts with the literal word "select".
	Language string `json:"language"`
}

type queryResponse struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// Warnings surfaces costguard's assessment (Phase 7 task 4) for
	// every query, hand-written or AI-suggested alike -- the same
	// guard, never a hard block here. AI-suggested queries get a
	// stricter treatment (a Reject-level assessment withholds the
	// suggestion entirely, see the ai package) before a query ever
	// reaches this handler; a hand-written query submitted directly
	// always runs regardless of what this says, matching every prior
	// phase's behavior -- this field is informational, not new
	// enforcement, a deliberate choice recorded in
	// /docs/phase-7-ai-design.md rather than a retrofit nobody decided
	// on. Omitted (not an empty array) when there's nothing to say, so
	// existing callers that don't look for this field see no shape
	// change at all.
	Warnings []string `json:"warnings,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// maxBodyBytes caps the request body: a query string has no legitimate
// reason to be larger than this.
const maxBodyBytes = 1 << 20 // 1 MiB

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query must not be empty")
		return
	}

	lang := planner.Language(req.Language)
	if lang != planner.Auto && lang != planner.SQL && lang != planner.SPL {
		writeError(w, http.StatusBadRequest, `language must be "sql", "spl", or omitted`)
		return
	}

	plan, err := planner.Compile(req.Query, lang, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.queryTimeout)
	defer cancel()

	start := time.Now()
	result, err := executor.Execute(ctx, plan, h.sqlRunner, h.search)
	duration := time.Since(start)

	if err != nil {
		h.logger.Error("query execution failed", "query", req.Query, "error", err)
		h.logAudit(r.Context(), req, 0, duration, err)
		writeError(w, http.StatusBadGateway, "query failed: "+err.Error())
		return
	}

	h.logAudit(r.Context(), req, len(result.Rows), duration, nil)
	resp := queryResponse{Columns: result.Columns, Rows: result.Rows}
	if assessment := costguard.Assess(plan); assessment.Level != costguard.LevelOK {
		resp.Warnings = []string{costguard.Summary(assessment)}
	}
	writeJSON(w, resp)
}

// logAudit is fail-open by design (see AuditLogger's doc comment): a
// write failure here is logged and otherwise ignored, never surfaced to
// the HTTP caller. Uses r.Context() (the original request context, not
// the query-execution one with its own deadline) so a slow/cancelled
// query's context.WithTimeout expiring doesn't also cancel the audit
// write for it.
func (h *Handler) logAudit(ctx context.Context, req queryRequest, rowCount int, duration time.Duration, execErr error) {
	if h.audit == nil {
		return
	}
	entry := QueryAuditEntry{
		Query: req.Query, Language: req.Language, RowCount: rowCount,
		Duration: duration, Success: execErr == nil,
	}
	if execErr != nil {
		entry.Error = execErr.Error()
	}
	if err := h.audit.LogQuery(ctx, entry); err != nil {
		h.logger.Error("audit log write failed", "error", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}
