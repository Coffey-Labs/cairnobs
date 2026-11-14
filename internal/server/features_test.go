package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// The monitoring side shows only what's set up; Configure says where each
// setting lives.
func TestFeaturesAndSettingsSummary(t *testing.T) {
	srv := stateServer(t, t.TempDir(), true)
	h := srv.Handler()
	c := devCookie(t, h)

	var ov struct{ Features map[string]bool }
	if err := json.Unmarshal(get(h, "/api/v1/overview", c).Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]bool{"maps": false, "mail": false, "cloudflare": false, "containers": false, "checks": false, "reports": true} {
		if ov.Features[k] != want {
			t.Errorf("features[%s] = %v, want %v", k, ov.Features[k], want)
		}
	}

	// A host that reports Docker turns Containers on; a check turns Checks on.
	now := time.Now()
	srv.store.Put("host1", report.Report{V: report.Version, Time: now, Containers: &report.Containers{}}, now)
	if rec := sendCookie(h, "PUT", "/api/v1/settings/checks/site", c, `{"name":"Site","kind":"http","target":"https://example.org/"}`); rec.Code != 200 {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	if f := srv.features(); !f["containers"] || !f["checks"] {
		t.Fatalf("after: %v", f)
	}

	var sum struct {
		Database bool
		Hosts    int
		Sections map[string]struct {
			Source string
			Count  int
		}
		Integrations map[string]string
	}
	rec := get(h, "/api/v1/settings", c)
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
	}
	if !sum.Database || sum.Sections["checks"].Source != "ui" || sum.Sections["checks"].Count != 1 || sum.Integrations["cloudflare"] != "" {
		t.Fatalf("summary: %s", rec.Body)
	}
	// Admins only.
	if rec := get(h, "/api/v1/settings", nil); rec.Code != 401 {
		t.Fatalf("signed out: %d", rec.Code)
	}
}
