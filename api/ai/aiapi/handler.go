// Package aiapi is Track A's HTTP surface: complete, explain, fix, and
// optimize, each a thin wrapper around api/ai/router dispatching to
// whichever provider.Provider is configured for that operation. Mirrors
// queryapi's shape deliberately (same auth wrapper, same request-size
// cap, same error-response shape) since this is the same kind of
// endpoint -- a JSON-in, JSON-out operation gated by the same RoleViewer
// requirement /query uses, nothing AI-specific about the transport.
//
// What this package does NOT do: execute a query. Every operation here
// returns text (a suggestion, an explanation, a fix) for the client to
// review -- running anything still goes through the unchanged POST
// /query endpoint (queryapi.Handler), never through here. See
// /docs/phase-7-ai-design.md for why that split is load-bearing, not
// incidental.
package aiapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/api/ai/costguard"
	"github.com/cairnobs/cairnobs/api/ai/provider"
	"github.com/cairnobs/cairnobs/api/ai/router"
	"github.com/cairnobs/cairnobs/api/authz"
	"github.com/cairnobs/cairnobs/api/internal/querylang/ir"
	"github.com/cairnobs/cairnobs/api/internal/querylang/planner"
)

// SchemaContextSource resolves the calling tenant's grounding data.
// Core wires a tenant-agnostic adapter around one grounding.Service;
// enterprise-api wires one around groundingregistry that reads the
// tenant from request context -- same "interface in core, tenant-aware
// implementation supplied by whoever constructs the handler" shape as
// queryapi.AuditLogger and dashboards.PermissionStore.
type SchemaContextSource interface {
	SchemaContext(ctx context.Context) provider.SchemaContext
}

// InteractionLogger records a translate/fix/optimize suggestion's
// accept-or-dismiss outcome into the Phase 4 audit trail (task 12) --
// same nil-by-default, fail-open shape as queryapi.AuditLogger: a
// single-tenant deployment with no enterprise/ configured just doesn't
// log these, same as it doesn't log query executions today.
// enterprise/internal/audit supplies the real implementation, writing
// into the same append-only audit_log table query executions use
// (a new event_type, not a new table -- see
// metadata/migrations/0036_add_ai_interaction_event_type.sql).
//
// Deliberately not wired into Complete (ghost-text): that operation
// fires on every keystroke pause, and logging each one at the same
// weight as a deliberate Fix/Optimize/Translate review would drown the
// signal task 12 actually wants (real accept/reject decisions) in
// high-frequency noise. Not wired into Explain either -- it produces no
// suggestion to accept or reject, so "accepted vs. rejected" doesn't
// apply to it. Both are named scope boundaries, not oversights.
type InteractionLogger interface {
	LogInteraction(ctx context.Context, entry InteractionEntry) error
}

type InteractionEntry struct {
	// Operation is "translate", "fix", or "optimize" -- the three flows
	// that produce a suggestion a user explicitly accepts or dismisses.
	Operation string
	Input     string
	Output    string
	// Confidence is empty for fix/optimize (provider.Confidence only
	// applies to Translate/Fix results in a way the frontend surfaces
	// today -- Optimize's phrasing has no confidence concept).
	Confidence string
	// Accepted is false for a dismissed suggestion; Output/FinalQuery
	// still carry what was offered, since a rejected suggestion is
	// itself useful signal (task 12: "useful data for improving
	// grounding/prompting later").
	Accepted bool
	// Edited is only meaningful when Accepted -- did the user change
	// the suggested text before using it. False, not omitted, when
	// Accepted is false (there's nothing to have edited).
	Edited     bool
	FinalQuery string
}

// completeTimeout is deliberately tight -- task 5's "low enough latency
// to feel responsive" requirement. A slow or hung provider must not
// stall the query bar; the frontend's fallback to deterministic
// autocomplete (Phase 2/5) kicks in on any error, including a timeout,
// so a short timeout here fails fast toward that fallback rather than
// making the user wait to find out AI completion isn't going to work
// this time.
const completeTimeout = 1500 * time.Millisecond

// Explain/Fix/Optimize are user-initiated (a button press, not
// as-you-type), so a more generous budget is the right tradeoff --
// correctness/quality over latency here, unlike Complete.
const operationTimeout = 15 * time.Second

type Handler struct {
	logger       *slog.Logger
	router       *router.Router
	schema       SchemaContextSource
	authz        authz.Authorizer
	interactions InteractionLogger
}

// interactions may be nil -- see InteractionLogger's doc comment.
func NewHandler(logger *slog.Logger, r *router.Router, schema SchemaContextSource, authorizer authz.Authorizer, interactions InteractionLogger) *Handler {
	return &Handler{logger: logger, router: r, schema: schema, authz: authorizer, interactions: interactions}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /ai/complete", authz.RequireRoleOrService(h.authz, authz.RoleViewer, h.handleComplete))
	mux.HandleFunc("POST /ai/explain", authz.RequireRoleOrService(h.authz, authz.RoleViewer, h.handleExplain))
	mux.HandleFunc("POST /ai/fix", authz.RequireRoleOrService(h.authz, authz.RoleViewer, h.handleFix))
	mux.HandleFunc("POST /ai/optimize", authz.RequireRoleOrService(h.authz, authz.RoleViewer, h.handleOptimize))
	mux.HandleFunc("POST /ai/translate", authz.RequireRoleOrService(h.authz, authz.RoleViewer, h.handleTranslate))
	mux.HandleFunc("POST /ai/log-interaction", authz.RequireRoleOrService(h.authz, authz.RoleViewer, h.handleLogInteraction))
}

const maxBodyBytes = 1 << 20 // 1 MiB, same cap queryapi uses -- these bodies are smaller still

type errorResponse struct {
	Error string `json:"error"`
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

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// ---- complete ----

type completeRequest struct {
	QueryPrefix string `json:"queryPrefix"`
	Language    string `json:"language"`
}

type completeResponse struct {
	Suggestion string `json:"suggestion"`
}

func (h *Handler) handleComplete(w http.ResponseWriter, r *http.Request) {
	var req completeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.QueryPrefix) == "" {
		writeJSON(w, completeResponse{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), completeTimeout)
	defer cancel()

	result, err := h.router.For(router.OpComplete).Complete(ctx, provider.CompleteRequest{
		QueryPrefix: req.QueryPrefix,
		Language:    orDefault(req.Language, "spl"),
		Schema:      h.schema.SchemaContext(r.Context()),
	})
	if err != nil {
		// Complete's whole point is to degrade gracefully -- a failed or
		// slow completion is not worth a scary error response the query
		// bar has to handle specially. An empty suggestion is exactly
		// what "no good completion available right now" looks like to
		// the frontend's fallback logic (task 5).
		h.logger.Warn("ai complete failed", "error", err)
		writeJSON(w, completeResponse{})
		return
	}
	writeJSON(w, completeResponse{Suggestion: result.Suggestion})
}

// ---- explain ----

type explainRequest struct {
	Query          string `json:"query"`
	Language       string `json:"language"`
	OriginalIntent string `json:"originalIntent"`
}

type explainResponse struct {
	Explanation string `json:"explanation"`
}

func (h *Handler) handleExplain(w http.ResponseWriter, r *http.Request) {
	var req explainRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query must not be empty")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()

	result, err := h.router.For(router.OpExplain).Explain(ctx, provider.ExplainRequest{
		Query:          req.Query,
		Language:       orDefault(req.Language, "spl"),
		OriginalIntent: req.OriginalIntent,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "explain failed: "+err.Error())
		return
	}
	writeJSON(w, explainResponse{Explanation: result.Explanation})
}

// ---- fix ----

type fixRequest struct {
	Query          string `json:"query"`
	Language       string `json:"language"`
	ParseError     string `json:"parseError"`
	ExecutionError string `json:"executionError"`
}

type fixResponse struct {
	SuggestedQuery string `json:"suggestedQuery"`
	Explanation    string `json:"explanation"`
	Confidence     string `json:"confidence"`
	// Blocked mirrors costguard's assessment on the *suggested* query --
	// task 4's stricter AI-track treatment: a reject-level suggestion is
	// still shown (so the user understands what was tried and why it's
	// not being offered outright) but the frontend must not present a
	// plain accept-and-run action for it. CostWarnings is empty unless
	// Blocked, or the suggestion has a lesser (warn-level) concern worth
	// surfacing.
	Blocked      bool     `json:"blocked"`
	CostWarnings []string `json:"costWarnings,omitempty"`
}

func (h *Handler) handleFix(w http.ResponseWriter, r *http.Request) {
	var req fixRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query must not be empty")
		return
	}
	if req.ParseError == "" && req.ExecutionError == "" {
		writeError(w, http.StatusBadRequest, "parseError or executionError must be set")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()

	lang := orDefault(req.Language, "spl")
	result, err := h.router.For(router.OpFix).Fix(ctx, provider.FixRequest{
		Query:          req.Query,
		Language:       lang,
		ParseError:     req.ParseError,
		ExecutionError: req.ExecutionError,
		Schema:         h.schema.SchemaContext(r.Context()),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "fix failed: "+err.Error())
		return
	}

	resp := fixResponse{
		SuggestedQuery: result.SuggestedQuery,
		Explanation:    result.Explanation,
		Confidence:     string(result.Confidence),
	}
	if resp.SuggestedQuery != "" {
		if plan, err := planner.Compile(resp.SuggestedQuery, planner.Language(lang), time.Now()); err == nil {
			if assessment := costguard.Assess(plan); assessment.Level != costguard.LevelOK {
				resp.CostWarnings = assessment.Reasons
				resp.Blocked = assessment.Level == costguard.LevelReject
			}
		}
		// A suggested query that itself fails to compile is left
		// unassessed rather than treated as an error -- an unusual
		// outcome (the model produced something that doesn't parse) the
		// frontend can still show as a suggestion text, just without a
		// cost assessment attached to it.
	}
	writeJSON(w, resp)
}

// ---- optimize ----

type optimizeRequest struct {
	Query    string `json:"query"`
	Language string `json:"language"`
}

type optimizeResponse struct {
	// Findings is always populated when costguard has anything to say --
	// rule-based, instant, no model call needed to produce this part.
	Findings []string `json:"findings"`
	// Phrased is the AI-phrased version of Findings (task 8: "AI layer
	// used mainly to phrase the suggestion clearly"). Empty if the
	// provider is unavailable or fails -- graceful degradation, same as
	// Complete: the raw Findings are still useful on their own, this is
	// an enhancement layered on top, not a dependency.
	Phrased string `json:"phrased"`
	// SuggestedQuery is a mechanical rewrite, not model-generated --
	// only populated for the one case this package can safely rewrite
	// unambiguously (a missing time range; see suggestFix below). Other
	// findings (an overly large time range, an unfiltered free-text
	// search) get text-only guidance, honestly, rather than a guessed
	// rewrite.
	SuggestedQuery string `json:"suggestedQuery,omitempty"`
}

func (h *Handler) handleOptimize(w http.ResponseWriter, r *http.Request) {
	var req optimizeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query must not be empty")
		return
	}

	lang := orDefault(req.Language, "spl")
	plan, err := planner.Compile(req.Query, planner.Language(lang), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "query does not compile: "+err.Error())
		return
	}

	assessment := costguard.Assess(plan)
	resp := optimizeResponse{Findings: assessment.Reasons}
	if assessment.Level == costguard.LevelOK {
		writeJSON(w, resp)
		return
	}

	resp.SuggestedQuery = suggestMechanicalFix(req.Query, plan)

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()
	if result, err := h.router.For(router.OpExplain).Explain(ctx, provider.ExplainRequest{
		Query:        req.Query,
		Language:     lang,
		RuleFindings: assessment.Reasons,
	}); err != nil {
		h.logger.Warn("ai optimize phrasing failed", "error", err)
	} else {
		resp.Phrased = result.Explanation
	}
	writeJSON(w, resp)
}

// ---- translate (Track B, task 9) ----

type translateRequest struct {
	NLQuery string `json:"nlQuery"`
}

type translateResponse struct {
	Query               string `json:"query"`
	Confidence          string `json:"confidence"`
	LowConfidenceReason string `json:"lowConfidenceReason,omitempty"`
	// Compiles is false when Query is non-empty but doesn't actually
	// parse as pipe syntax -- a real, honest outcome (the model
	// produced something invalid), not folded into "low confidence"
	// since a model can be confident and still wrong about syntax.
	// CompileError is set only then.
	Compiles     bool   `json:"compiles"`
	CompileError string `json:"compileError,omitempty"`
	// Blocked/CostWarnings mirror handleFix's same-named fields exactly
	// -- task 9's explicit requirement that translation results run
	// through the shared cost guard "before returning it," same
	// treatment as an AI-suggested fix gets, not a lesser one.
	Blocked      bool     `json:"blocked"`
	CostWarnings []string `json:"costWarnings,omitempty"`
}

func (h *Handler) handleTranslate(w http.ResponseWriter, r *http.Request) {
	var req translateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.NLQuery) == "" {
		writeError(w, http.StatusBadRequest, "nlQuery must not be empty")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operationTimeout)
	defer cancel()

	result, err := h.router.For(router.OpTranslate).Translate(ctx, provider.TranslateRequest{
		NLQuery: req.NLQuery,
		Schema:  h.schema.SchemaContext(r.Context()),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "translation failed: "+err.Error())
		return
	}

	resp := translateResponse{
		Query:               result.Query,
		Confidence:          string(result.Confidence),
		LowConfidenceReason: result.LowConfidenceReason,
	}
	if resp.Query != "" {
		// Always pipe syntax -- provider.TranslateResult's own doc
		// comment requires this (task 9: "prefer this over raw SQL...
		// narrower, safer surface"), so this always compiles as SPL,
		// never auto-detected/SQL.
		plan, compileErr := planner.Compile(resp.Query, planner.SPL, time.Now())
		if compileErr != nil {
			resp.Compiles = false
			resp.CompileError = compileErr.Error()
		} else {
			resp.Compiles = true
			if assessment := costguard.Assess(plan); assessment.Level != costguard.LevelOK {
				resp.CostWarnings = assessment.Reasons
				resp.Blocked = assessment.Level == costguard.LevelReject
			}
		}
	}
	writeJSON(w, resp)
}

// suggestMechanicalFix handles exactly one case: no time bound at all.
// Prepending "earliest=-1h " is always syntactically safe (another
// AND'd base-search filter term, same as any other) and semantically
// the single most common real fix for this specific finding -- not
// attempted for any other finding (a too-large span, an unindexed
// free-text pattern), which don't have one unambiguous correct rewrite.
// Checked against the plan directly (not against costguard's Reasons
// text), so this stays correct even if that phrasing changes later.
func suggestMechanicalFix(originalQuery string, plan *ir.Plan) string {
	if plan.RawSQL != "" {
		return "" // no safe generic rewrite for arbitrary SQL
	}
	hasTimeBound := plan.TimeRange != nil && (!plan.TimeRange.From.IsZero() || !plan.TimeRange.To.IsZero())
	if hasTimeBound {
		return ""
	}
	return "earliest=-1h " + originalQuery
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---- log-interaction (task 12) ----

type logInteractionRequest struct {
	Operation  string `json:"operation"`
	Input      string `json:"input"`
	Output     string `json:"output"`
	Confidence string `json:"confidence"`
	Accepted   bool   `json:"accepted"`
	Edited     bool   `json:"edited"`
	FinalQuery string `json:"finalQuery"`
}

var validInteractionOps = map[string]bool{"translate": true, "fix": true, "optimize": true}

// handleLogInteraction is called by the frontend at the moment a user
// takes a terminal action on a suggestion (accept-and-use or dismiss) --
// see InteractionLogger's doc comment for why this is a single
// frontend-reported event rather than a backend-correlated
// generation-plus-outcome pair. Fail-open, same posture
// queryapi.Handler.logAudit uses: a write failure here is logged
// server-side and otherwise ignored, never surfaced as an error to a
// user who just clicked a button -- audit-trail completeness is a real
// requirement, but it shouldn't be able to break the query bar.
func (h *Handler) handleLogInteraction(w http.ResponseWriter, r *http.Request) {
	var req logInteractionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !validInteractionOps[req.Operation] {
		writeError(w, http.StatusBadRequest, `operation must be one of "translate", "fix", "optimize"`)
		return
	}

	if h.interactions != nil {
		err := h.interactions.LogInteraction(r.Context(), InteractionEntry{
			Operation:  req.Operation,
			Input:      req.Input,
			Output:     req.Output,
			Confidence: req.Confidence,
			Accepted:   req.Accepted,
			Edited:     req.Edited,
			FinalQuery: req.FinalQuery,
		})
		if err != nil {
			h.logger.Error("ai interaction audit log write failed", "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
