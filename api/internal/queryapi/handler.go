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

	"github.com/sentry/sentry/api/internal/querylang/executor"
	"github.com/sentry/sentry/api/internal/querylang/planner"
)

type Handler struct {
	logger       *slog.Logger
	sqlRunner    executor.SQLRunner
	search       executor.SearchClient
	queryTimeout time.Duration
}

func NewHandler(logger *slog.Logger, sqlRunner executor.SQLRunner, search executor.SearchClient, queryTimeout time.Duration) *Handler {
	return &Handler{logger: logger, sqlRunner: sqlRunner, search: search, queryTimeout: queryTimeout}
}

// RegisterRoutes adds this handler's routes onto a shared mux. Phase 3
// introduced a second handler package (internal/dashboards), so CORS is
// now applied once, by main.go, around the fully-assembled mux rather
// than by each handler wrapping itself individually -- see
// httpserver.WithCORS.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /query", h.handleQuery)
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

	result, err := executor.Execute(ctx, plan, h.sqlRunner, h.search)
	if err != nil {
		h.logger.Error("query execution failed", "query", req.Query, "error", err)
		writeError(w, http.StatusBadGateway, "query failed: "+err.Error())
		return
	}

	writeJSON(w, queryResponse{Columns: result.Columns, Rows: result.Rows})
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
