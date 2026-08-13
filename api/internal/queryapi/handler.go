// Package queryapi is Sentry's query API: POST /query (Phase 0, a crude
// raw-SQL passthrough allowlisted to SELECT) and POST /search (Phase 1,
// free-text search via the search service, joined back against
// ClickHouse). This is a deliberate simplification of the pinned "gRPC +
// REST gateway" control-plane pattern (see CLAUDE.md's tech stack table):
// plain net/http REST handlers, not a gRPC service transcoded through
// grpc-gateway. That machinery (proto definitions, googleapis
// annotations, gateway codegen) doesn't buy much for two crude endpoints
// that Phase 2's real SPL-like query layer replaces outright. Revisit
// gRPC+gateway once /api's endpoint count and lifespan justify it.
package queryapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// queryExecutor is the narrow interface handleQuery depends on, so tests
// can substitute a fake without a real ClickHouse connection. *Executor
// satisfies it.
type queryExecutor interface {
	Execute(ctx context.Context, sql string) (*QueryResult, error)
}

type Handler struct {
	logger        *slog.Logger
	exec          queryExecutor
	search        searchClient
	queryTimeout  time.Duration
	allowedOrigin string
}

func NewHandler(logger *slog.Logger, exec queryExecutor, search searchClient, queryTimeout time.Duration, allowedOrigin string) *Handler {
	return &Handler{logger: logger, exec: exec, search: search, queryTimeout: queryTimeout, allowedOrigin: allowedOrigin}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /query", h.handleQuery)
	mux.HandleFunc("POST /search", h.handleSearch)
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	return h.withCORS(mux)
}

// withCORS is deliberately permissive by default (see CORSAllowedOrigin in
// internal/config) since Phase 0 has no auth and the SvelteKit dev server
// runs on a different origin. Tighten alongside adding real auth.
func (h *Handler) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", h.allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

type queryRequest struct {
	SQL string `json:"sql"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// maxBodyBytes caps the request body: a raw SQL string has no legitimate
// reason to be larger than this.
const maxBodyBytes = 1 << 20 // 1 MiB

func (h *Handler) handleQuery(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if err := validateSelectOnly(req.SQL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.queryTimeout)
	defer cancel()

	result, err := h.exec.Execute(ctx, req.SQL)
	if err != nil {
		h.logger.Error("query execution failed", "error", err)
		writeError(w, http.StatusBadGateway, "query failed: "+err.Error())
		return
	}

	writeJSON(w, result)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}
