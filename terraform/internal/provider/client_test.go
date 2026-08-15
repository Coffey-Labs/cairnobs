package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateDashboardSendsExpectedRequest is the same "real
// httptest.Server, real HTTP round trip" pattern
// cli/cmd/sentryctl's own tests use against the same api/dashboards
// endpoints -- this client has no fake/mock mode, so its tests exercise
// real request construction and real response parsing throughout.
func TestCreateDashboardSendsExpectedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/dashboards" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		var body dashboard
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Name != "My Dashboard" {
			t.Errorf("request body Name = %q, want My Dashboard", body.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(dashboard{
			ID: "dash-1", TenantID: "acme", Name: body.Name,
			DefaultEarliest: "-1h", DefaultLatest: "now",
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "test-token")
	out, err := c.createDashboard(context.Background(), &dashboard{Name: "My Dashboard"})
	if err != nil {
		t.Fatalf("createDashboard: %v", err)
	}
	if out.ID != "dash-1" || out.TenantID != "acme" || out.DefaultEarliest != "-1h" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestGetDashboardNotFoundIsRecognizable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "dashboard not found"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getDashboard(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !isNotFound(err) {
		t.Fatalf("isNotFound(%v) = false, want true", err)
	}
}

func TestGetDashboardServerErrorIsNotNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getDashboard(context.Background(), "dash-1")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if isNotFound(err) {
		t.Fatal("isNotFound must be false for a 500 -- only a real 404 means \"this resource is gone\"")
	}
}

func TestUpdateDashboardSendsToCorrectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/dashboards/dash-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dashboard{ID: "dash-1", Name: "Renamed"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.updateDashboard(context.Background(), "dash-1", &dashboard{Name: "Renamed"})
	if err != nil {
		t.Fatalf("updateDashboard: %v", err)
	}
	if out.Name != "Renamed" {
		t.Fatalf("Name = %q, want Renamed", out.Name)
	}
}

func TestDeleteDashboardSendsToCorrectPath(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete || r.URL.Path != "/dashboards/dash-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	if err := c.deleteDashboard(context.Background(), "dash-1"); err != nil {
		t.Fatalf("deleteDashboard: %v", err)
	}
	if !called {
		t.Fatal("expected the server to receive a DELETE request")
	}
}

func TestDoOmitsAuthorizationHeaderWhenNoTokenConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	if err := c.do(context.Background(), http.MethodGet, "/dashboards", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
}

func TestApiErrorSurfacesPlainTextBodyWhenNotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getDashboard(context.Background(), "dash-1")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v, want it to surface the plain-text body", err)
	}
}

func TestCreateRuleSendsExpectedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rules" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body rule
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Name != "High Error Rate" || body.ConditionType != "threshold" {
			t.Errorf("unexpected request body: %+v", body)
		}
		if body.Comparator == nil || *body.Comparator != "gt" {
			t.Errorf("Comparator = %v, want gt", body.Comparator)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		comparator := "gt"
		threshold := 5.0
		_ = json.NewEncoder(w).Encode(rule{
			ID: "rule-1", TenantID: "acme", Name: body.Name,
			ConditionType: "threshold", Comparator: &comparator, ThresholdValue: &threshold,
			EvalIntervalSeconds: 60, NotificationTargetID: "target-1",
		})
	}))
	defer srv.Close()

	comparator := "gt"
	threshold := 5.0
	c := newClient(srv.URL, "")
	out, err := c.createRule(context.Background(), &rule{
		Name: "High Error Rate", Query: "status>=500 | stats count", ConditionType: "threshold",
		Comparator: &comparator, ThresholdValue: &threshold,
		EvalIntervalSeconds: 60, NotificationTargetID: "target-1",
	})
	if err != nil {
		t.Fatalf("createRule: %v", err)
	}
	if out.ID != "rule-1" || out.EvalIntervalSeconds != 60 {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestGetRuleParsesFlattenedRuleWithStateResponse(t *testing.T) {
	// alerting/internal/httpapi's GET /rules/{id} returns
	// rulestore.RuleWithState -- Rule's fields promoted to the top
	// level via anonymous embedding, plus a "state" object this
	// client's rule type deliberately has no field for (see client.go's
	// doc comment). This test proves that extra "state" key doesn't
	// break parsing the fields this provider does care about.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "rule-1", "tenant_id": "acme", "name": "High Error Rate",
			"condition_type": "threshold", "comparator": "gt", "threshold_value": 5,
			"eval_interval_seconds": 60, "notification_target_id": "target-1", "enabled": true,
			"state": {"rule_id": "rule-1", "state": "ok", "last_eval_status": "ok", "consecutive_errors": 0}
		}`))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.getRule(context.Background(), "rule-1")
	if err != nil {
		t.Fatalf("getRule: %v", err)
	}
	if out.Name != "High Error Rate" || out.Comparator == nil || *out.Comparator != "gt" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestDeleteRuleSendsToCorrectPath(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete || r.URL.Path != "/rules/rule-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	if err := c.deleteRule(context.Background(), "rule-1"); err != nil {
		t.Fatalf("deleteRule: %v", err)
	}
	if !called {
		t.Fatal("expected the server to receive a DELETE request")
	}
}

func TestCreateNotificationTargetSendsExpectedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/targets" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body notificationTarget
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Name != "Ops Webhook" || body.Kind != "webhook" {
			t.Errorf("unexpected request body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(notificationTarget{
			ID: "target-1", TenantID: "acme", Name: body.Name, Kind: body.Kind, WebhookURL: body.WebhookURL,
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.createNotificationTarget(context.Background(), &notificationTarget{
		Name: "Ops Webhook", Kind: "webhook", WebhookURL: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("createNotificationTarget: %v", err)
	}
	if out.ID != "target-1" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestGetNotificationTargetReturnsSecretUnredacted(t *testing.T) {
	// Documents real, existing alerting behavior (notifystore's Get
	// query selects the secret column with no redaction) -- this test
	// exists so a future change to alerting's redaction posture would
	// be caught here too, not just discovered by surprise.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(notificationTarget{ID: "target-1", Name: "Ops Webhook", Kind: "webhook", Secret: strPtr("shh")})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.getNotificationTarget(context.Background(), "target-1")
	if err != nil {
		t.Fatalf("getNotificationTarget: %v", err)
	}
	if out.Secret == nil || *out.Secret != "shh" {
		t.Fatalf("Secret = %v, want it echoed back unredacted", out.Secret)
	}
}

func TestDeleteNotificationTargetSendsToCorrectPath(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete || r.URL.Path != "/targets/target-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	if err := c.deleteNotificationTarget(context.Background(), "target-1"); err != nil {
		t.Fatalf("deleteNotificationTarget: %v", err)
	}
	if !called {
		t.Fatal("expected the server to receive a DELETE request")
	}
}

func strPtr(s string) *string { return &s }

func TestCreatePanelSendsExpectedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/dashboards/dash-1/panels" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body panel
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Query != "status>=500 | stats count" || body.VizType != "line" {
			t.Errorf("unexpected request body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(panel{ID: "panel-1", DashboardID: "dash-1", Query: body.Query, VizType: body.VizType})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.createPanel(context.Background(), "dash-1", &panel{Query: "status>=500 | stats count", VizType: "line"})
	if err != nil {
		t.Fatalf("createPanel: %v", err)
	}
	if out.ID != "panel-1" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestUpdatePanelSendsToCorrectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/dashboards/dash-1/panels/panel-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(panel{ID: "panel-1", Title: "Renamed"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.updatePanel(context.Background(), "dash-1", "panel-1", &panel{Title: "Renamed"})
	if err != nil {
		t.Fatalf("updatePanel: %v", err)
	}
	if out.Title != "Renamed" {
		t.Fatalf("Title = %q, want Renamed", out.Title)
	}
}

func TestDeletePanelSendsToCorrectPath(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete || r.URL.Path != "/dashboards/dash-1/panels/panel-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	if err := c.deletePanel(context.Background(), "dash-1", "panel-1"); err != nil {
		t.Fatalf("deletePanel: %v", err)
	}
	if !called {
		t.Fatal("expected the server to receive a DELETE request")
	}
}

func TestGetPanelFindsPanelWithinParentDashboard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/dashboards/dash-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dashboard{
			ID: "dash-1",
			Panels: []panel{
				{ID: "panel-1", Title: "First"},
				{ID: "panel-2", Title: "Second"},
			},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	out, err := c.getPanel(context.Background(), "dash-1", "panel-2")
	if err != nil {
		t.Fatalf("getPanel: %v", err)
	}
	if out.Title != "Second" {
		t.Fatalf("Title = %q, want Second", out.Title)
	}
}

func TestGetPanelNotFoundWhenPanelMissingFromDashboard(t *testing.T) {
	// Same "not found" shape as a real 404 -- proves isNotFound works
	// for a panel absent from an otherwise-real dashboard response, not
	// just for a literal 404 status code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dashboard{ID: "dash-1", Panels: []panel{{ID: "panel-1"}}})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getPanel(context.Background(), "dash-1", "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for a panel not present on the dashboard")
	}
	if !isNotFound(err) {
		t.Fatalf("isNotFound(%v) = false, want true", err)
	}
}

func TestGetPanelPropagatesDashboardNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.getPanel(context.Background(), "does-not-exist", "panel-1")
	if err == nil || !isNotFound(err) {
		t.Fatalf("err = %v, want a recognizable not-found when the parent dashboard itself is gone", err)
	}
}
