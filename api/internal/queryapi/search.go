package queryapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// searchClient is the narrow interface handleSearch depends on, so tests
// can substitute a fake without a real search service. A small gRPC
// adapter in cmd/api satisfies this.
type searchClient interface {
	Search(ctx context.Context, query string, limit uint32) ([]string, error)
}

type searchRequest struct {
	Query string `json:"query"`
	Limit uint32 `json:"limit"`
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req searchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query must not be empty")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.queryTimeout)
	defer cancel()

	recordIDs, err := h.search.Search(ctx, req.Query, req.Limit)
	if err != nil {
		h.logger.Error("search failed", "error", err)
		writeError(w, http.StatusBadGateway, "search failed: "+err.Error())
		return
	}

	if len(recordIDs) == 0 {
		writeJSON(w, &QueryResult{Columns: []string{}, Rows: [][]any{}})
		return
	}

	sql, err := recordIDsQuery(recordIDs)
	if err != nil {
		h.logger.Error("building record_id query", "error", err)
		writeError(w, http.StatusBadGateway, "search returned unusable results")
		return
	}

	result, err := h.exec.Execute(ctx, sql)
	if err != nil {
		h.logger.Error("joining search results against clickhouse failed", "error", err)
		writeError(w, http.StatusBadGateway, "query failed: "+err.Error())
		return
	}

	writeJSON(w, result)
}

// recordIDsQuery builds a SELECT ... WHERE record_id IN (...) against the
// IDs the search service returned. Every ID is validated as a real UUID
// before being embedded in the query string -- record_ids come from an
// internal, trusted service (not raw user input), but a UUID that fails
// to parse can't contain SQL-breaking characters either way, so this is
// defense in depth, not a response to a specific threat.
func recordIDsQuery(recordIDs []string) (string, error) {
	quoted := make([]string, 0, len(recordIDs))
	for _, id := range recordIDs {
		if _, err := uuid.Parse(id); err != nil {
			continue // skip anything not a valid UUID rather than failing the whole query
		}
		quoted = append(quoted, "'"+id+"'")
	}
	if len(quoted) == 0 {
		return "", fmt.Errorf("no valid record_ids in search response")
	}
	return fmt.Sprintf(
		"SELECT * FROM logs WHERE record_id IN (%s) ORDER BY timestamp DESC",
		strings.Join(quoted, ","),
	), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
