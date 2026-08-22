// Package httpapi is alerting's REST surface: rule CRUD, notification
// target CRUD, and a read-only delivery log -- mirrors
// api/internal/dashboards' Handler/Store shape (narrow interfaces,
// pgx-backed production implementation, fakes in tests).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cairnobs/cairnobs/alerting/internal/notifystore"
	"github.com/cairnobs/cairnobs/alerting/internal/rulestore"
)

type ruleStore interface {
	Create(ctx context.Context, r *rulestore.Rule) error
	List(ctx context.Context) ([]rulestore.RuleWithState, error)
	Get(ctx context.Context, id string) (*rulestore.RuleWithState, error)
	Delete(ctx context.Context, id string) error
}

type targetStore interface {
	Create(ctx context.Context, t *notifystore.Target) error
	List(ctx context.Context) ([]notifystore.Target, error)
	Get(ctx context.Context, id string) (*notifystore.Target, error)
	Delete(ctx context.Context, id string) error
}

type deliveryReader interface {
	ListForRule(ctx context.Context, ruleID string, limit int) ([]rulestore.DeliveryLogEntry, error)
}

type Handler struct {
	logger     *slog.Logger
	rules      ruleStore
	targets    targetStore
	deliveries deliveryReader
}

func NewHandler(logger *slog.Logger, rules ruleStore, targets targetStore, deliveries deliveryReader) *Handler {
	return &Handler{logger: logger, rules: rules, targets: targets, deliveries: deliveries}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.handleHealthz)

	mux.HandleFunc("POST /rules", h.handleCreateRule)
	mux.HandleFunc("GET /rules", h.handleListRules)
	mux.HandleFunc("GET /rules/{id}", h.handleGetRule)
	mux.HandleFunc("DELETE /rules/{id}", h.handleDeleteRule)
	mux.HandleFunc("GET /rules/{id}/deliveries", h.handleListDeliveries)

	mux.HandleFunc("POST /targets", h.handleCreateTarget)
	mux.HandleFunc("GET /targets", h.handleListTargets)
	mux.HandleFunc("GET /targets/{id}", h.handleGetTarget)
	mux.HandleFunc("DELETE /targets/{id}", h.handleDeleteTarget)
}

const maxBodyBytes = 1 << 20 // 1 MiB, same cap as api/internal/queryapi and dashboards

func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// createRuleRequest mirrors rulestore.Rule except Enabled is a pointer:
// a plain bool can't distinguish "omitted" from "explicitly false" on
// decode, and Go's zero value for bool is false -- without this, a
// create request that simply doesn't mention "enabled" would silently
// create a rule the evaluator's claim query never picks up (found by
// actually creating a rule through this endpoint and checking the
// response). Omitted means enabled; only an explicit "enabled": false
// creates a disabled rule.
type createRuleRequest struct {
	rulestore.Rule
	Enabled *bool `json:"enabled"`
}

func (h *Handler) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req createRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rule := req.Rule
	rule.Enabled = req.Enabled == nil || *req.Enabled
	if err := validateRule(&rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.rules.Create(r.Context(), &rule); err != nil {
		h.logger.Error("creating rule", "error", err)
		writeError(w, http.StatusInternalServerError, "creating rule failed")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (h *Handler) handleListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.rules.List(r.Context())
	if err != nil {
		h.logger.Error("listing rules", "error", err)
		writeError(w, http.StatusInternalServerError, "listing rules failed")
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func (h *Handler) handleGetRule(w http.ResponseWriter, r *http.Request) {
	rule, err := h.rules.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeStoreErr(w, err, "fetching rule")
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *Handler) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	if err := h.rules.Delete(r.Context(), r.PathValue("id")); err != nil {
		h.writeStoreErr(w, err, "deleting rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	entries, err := h.deliveries.ListForRule(r.Context(), r.PathValue("id"), 100)
	if err != nil {
		h.logger.Error("listing deliveries", "rule_id", r.PathValue("id"), "error", err)
		writeError(w, http.StatusInternalServerError, "listing deliveries failed")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *Handler) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var target notifystore.Target
	if !decodeJSON(w, r, &target) {
		return
	}
	if target.Name == "" {
		writeError(w, http.StatusBadRequest, "name must not be empty")
		return
	}
	if !notifystore.ValidKind(target.Kind) {
		writeError(w, http.StatusBadRequest, "kind must be one of webhook, slack, pagerduty")
		return
	}
	if target.WebhookURL == "" {
		writeError(w, http.StatusBadRequest, "webhook_url must not be empty")
		return
	}
	if err := notifystore.ValidateWebhookURL(target.WebhookURL); err != nil {
		writeError(w, http.StatusBadRequest, "webhook_url: "+err.Error())
		return
	}
	if err := h.targets.Create(r.Context(), &target); err != nil {
		h.logger.Error("creating notification target", "error", err)
		writeError(w, http.StatusInternalServerError, "creating notification target failed")
		return
	}
	writeJSON(w, http.StatusCreated, target)
}

func (h *Handler) handleListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.targets.List(r.Context())
	if err != nil {
		h.logger.Error("listing notification targets", "error", err)
		writeError(w, http.StatusInternalServerError, "listing notification targets failed")
		return
	}
	writeJSON(w, http.StatusOK, targets)
}

func (h *Handler) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	target, err := h.targets.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeStoreErr(w, err, "fetching notification target")
		return
	}
	writeJSON(w, http.StatusOK, target)
}

func (h *Handler) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	if err := h.targets.Delete(r.Context(), r.PathValue("id")); err != nil {
		h.writeStoreErr(w, err, "deleting notification target")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateRule enforces the shape the evaluator assumes:
// condition_type-appropriate fields present, a sane interval floor.
// Mirrors the DB CHECK constraints so bad input is rejected with a
// clear message here rather than surfacing as an opaque constraint
// violation from Postgres.
func validateRule(r *rulestore.Rule) error {
	if r.Name == "" {
		return errBadRequest("name must not be empty")
	}
	if r.Query == "" {
		return errBadRequest("query must not be empty")
	}
	if r.NotificationTargetID == "" {
		return errBadRequest("notification_target_id must not be empty")
	}
	if r.EvalIntervalSeconds < 30 {
		return errBadRequest("eval_interval_seconds must be at least 30")
	}
	switch r.ConditionType {
	case rulestore.ConditionThreshold:
		if r.Comparator == nil || !rulestore.ValidComparator(*r.Comparator) {
			return errBadRequest("threshold rules require a valid comparator (gt, gte, lt, lte, eq, ne)")
		}
		if r.ThresholdValue == nil {
			return errBadRequest("threshold rules require threshold_value")
		}
	case rulestore.ConditionAbsence:
		// No comparator/threshold_value needed -- absence is "the query
		// returned zero rows in its own earliest=/latest= window."
	default:
		return errBadRequest("condition_type must be \"threshold\" or \"absence\"")
	}
	return nil
}

type badRequestError string

func (e badRequestError) Error() string { return string(e) }
func errBadRequest(msg string) error    { return badRequestError(msg) }

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, rulestore.ErrNotFound) || errors.Is(err, notifystore.ErrNotFound) {
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
