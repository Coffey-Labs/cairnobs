package dashboards

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// store is the narrow interface Handler depends on -- *Store (store.go)
// is the production implementation; tests use a fake, same pattern as
// queryapi's SQLRunner/SearchClient.
type store interface {
	CreateDashboard(ctx context.Context, d *Dashboard) error
	ListDashboards(ctx context.Context) ([]Dashboard, error)
	GetDashboard(ctx context.Context, id string) (*Dashboard, error)
	UpdateDashboard(ctx context.Context, d *Dashboard) error
	DeleteDashboard(ctx context.Context, id string) error
	AddPanel(ctx context.Context, dashboardID string, p *Panel) error
	UpdatePanel(ctx context.Context, p *Panel) error
	DeletePanel(ctx context.Context, dashboardID, panelID string) error
	ImportDashboard(ctx context.Context, d *Dashboard) (*Dashboard, error)
}

type Handler struct {
	logger *slog.Logger
	store  store
}

func NewHandler(logger *slog.Logger, store store) *Handler {
	return &Handler{logger: logger, store: store}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /dashboards", h.handleCreate)
	mux.HandleFunc("GET /dashboards", h.handleList)
	mux.HandleFunc("POST /dashboards/import", h.handleImport)
	mux.HandleFunc("GET /dashboards/{id}", h.handleGet)
	mux.HandleFunc("PUT /dashboards/{id}", h.handleUpdate)
	mux.HandleFunc("DELETE /dashboards/{id}", h.handleDelete)
	mux.HandleFunc("GET /dashboards/{id}/export", h.handleExport)
	mux.HandleFunc("POST /dashboards/{id}/panels", h.handleAddPanel)
	mux.HandleFunc("PUT /dashboards/{id}/panels/{panelId}", h.handleUpdatePanel)
	mux.HandleFunc("DELETE /dashboards/{id}/panels/{panelId}", h.handleDeletePanel)
}

const maxBodyBytes = 1 << 20 // 1 MiB, same cap as queryapi

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var d Dashboard
	if !decodeJSON(w, r, &d) {
		return
	}
	if d.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	if err := h.store.CreateDashboard(r.Context(), &d); err != nil {
		h.logger.Error("creating dashboard", "error", err)
		writeError(w, http.StatusInternalServerError, "creating dashboard failed")
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListDashboards(r.Context())
	if err != nil {
		h.logger.Error("listing dashboards", "error", err)
		writeError(w, http.StatusInternalServerError, "listing dashboards failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	d, err := h.store.GetDashboard(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeStoreErr(w, err, "fetching dashboard")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	// Export is the same document GET /dashboards/{id} returns -- the
	// import endpoint below consumes exactly this shape, and so does
	// `sentryctl dashboards apply`, so there's one JSON contract used
	// from every call site rather than a bespoke export format.
	h.handleGet(w, r)
}

func (h *Handler) handleImport(w http.ResponseWriter, r *http.Request) {
	var d Dashboard
	if !decodeJSON(w, r, &d) {
		return
	}
	if d.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	imported, err := h.store.ImportDashboard(r.Context(), &d)
	if err != nil {
		h.logger.Error("importing dashboard", "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, imported)
}

func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var d Dashboard
	if !decodeJSON(w, r, &d) {
		return
	}
	if d.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	d.ID = r.PathValue("id")
	if err := h.store.UpdateDashboard(r.Context(), &d); err != nil {
		h.writeStoreErr(w, err, "updating dashboard")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteDashboard(r.Context(), r.PathValue("id")); err != nil {
		h.writeStoreErr(w, err, "deleting dashboard")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleAddPanel(w http.ResponseWriter, r *http.Request) {
	var p Panel
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := validatePanel(&p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.AddPanel(r.Context(), r.PathValue("id"), &p); err != nil {
		h.logger.Error("adding panel", "error", err)
		writeError(w, http.StatusInternalServerError, "adding panel failed")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) handleUpdatePanel(w http.ResponseWriter, r *http.Request) {
	var p Panel
	if !decodeJSON(w, r, &p) {
		return
	}
	if err := validatePanel(&p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.ID = r.PathValue("panelId")
	p.DashboardID = r.PathValue("id")
	if err := h.store.UpdatePanel(r.Context(), &p); err != nil {
		h.writeStoreErr(w, err, "updating panel")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) handleDeletePanel(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeletePanel(r.Context(), r.PathValue("id"), r.PathValue("panelId")); err != nil {
		h.writeStoreErr(w, err, "deleting panel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	h.logger.Error(action, "error", err)
	writeError(w, http.StatusInternalServerError, action+" failed")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}
