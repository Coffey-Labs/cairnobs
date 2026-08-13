// Package queryapi is the Phase 0 query API: a single crude POST /query
// endpoint that takes a raw SQL string, allowlists it to a single SELECT
// statement, and proxies it to ClickHouse. This is a deliberate
// simplification of the pinned "gRPC + REST gateway" control-plane
// pattern (see CLAUDE.md's tech stack table): a plain net/http REST
// handler, not a gRPC service transcoded through grpc-gateway. That
// machinery (proto definitions, googleapis annotations, gateway codegen)
// buys nothing for one crude placeholder endpoint that Phase 2 replaces
// outright with the real SPL-like query layer. Revisit gRPC+gateway when
// /api grows a second real endpoint.
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
	queryTimeout  time.Duration
	allowedOrigin string
}

func NewHandler(logger *slog.Logger, exec queryExecutor, queryTimeout time.Duration, allowedOrigin string) *Handler {
	return &Handler{logger: logger, exec: exec, queryTimeout: queryTimeout, allowedOrigin: allowedOrigin}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /query", h.handleQuery)
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

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		h.logger.Error("encoding response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}
