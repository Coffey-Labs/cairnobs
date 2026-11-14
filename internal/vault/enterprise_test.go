package vault

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A fake Vault Enterprise: replication and the license are read only when
// sys/health says they're there, and judged.
func TestEnterprise(t *testing.T) {
	now := time.Now().UTC()
	beat := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	var asked atomic.Int32
	enterprise, repl, license := true, "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/health":
			mode := "disabled"
			if repl != "" {
				mode = "primary"
			}
			fmt.Fprintf(w, `{"initialized":true,"sealed":false,"version":"1.21.4+ent","enterprise":%v,"replication_dr_mode":%q,"replication_performance_mode":"disabled"}`, enterprise, mode)
		case "/v1/sys/leader":
			fmt.Fprint(w, `{"ha_enabled":false}`)
		case "/v1/sys/replication/status":
			asked.Add(1)
			fmt.Fprint(w, repl)
		case "/v1/sys/license/status":
			asked.Add(1)
			fmt.Fprint(w, license)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[]}`)
		}
	}))
	defer srv.Close()
	c, _ := newClient(Config{Name: "ent", URL: srv.URL, TokenFile: writeToken(t, "t")})
	parts := func() map[string]report.Workload {
		t.Helper()
		s, err := c.Read(context.Background(), now)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Problems) > 0 {
			t.Errorf("problems %v", s.Problems)
		}
		out := map[string]report.Workload{}
		for _, r := range Records(s) {
			out[r.Name] = r
		}
		return out
	}

	// Open-source: nothing asked.
	enterprise = false
	if p := parts(); asked.Load() != 0 || len(p) != 2 {
		t.Errorf("open-source: asked %d, %v", asked.Load(), p)
	}

	enterprise = true
	license = fmt.Sprintf(`{"data":{"autoloading_used":true,"autoloaded":{"license_id":"L-1","expiration_time":%q,"termination_time":%q}}}`,
		now.Add(10*24*time.Hour+time.Hour).Format(time.RFC3339), now.Add(40*24*time.Hour).Format(time.RFC3339))
	repl = fmt.Sprintf(`{"data":{"dr":{"mode":"primary","state":"running","known_secondaries":["dr-1","dr-2"],"last_wal":291,
		"secondaries":[{"node_id":"dr-1","connection_state":"ready","last_heartbeat":%q},{"node_id":"dr-2","connection_state":"transient_failure","last_heartbeat":%q}]},
		"performance":{"mode":"disabled"}}}`, beat(3*time.Second), beat(10*time.Minute))
	p := parts()
	if l := p[LicensePart]; l.State != report.StateDegraded || !strings.HasPrefix(l.Word, "Expires in 10 days") || l.Facts["license_id"] != "L-1" {
		t.Errorf("license %+v", l)
	}
	if r := p["replication/dr"]; r.State != report.StateDegraded || r.Word != "Primary: dr-2 (transient failure)" {
		t.Errorf("dr %+v", r)
	}
	if _, ok := p["replication/performance"]; ok {
		t.Error("performance replication is off: no record")
	}
}

func TestReplicationStates(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	fresh, stale := now.Add(-5*time.Second).Format(time.RFC3339), now.Add(-10*time.Minute).Format(time.RFC3339)
	for _, c := range []struct {
		name  string
		r     Replication
		state string
		word  string
	}{
		{"streaming", Replication{Mode: "primary", State: "running", Secondaries: []Peer{{NodeID: "a", ConnectionState: "ready", LastHeartbeat: fresh}}},
			report.StateOK, "Primary, streaming to 1 secondary"},
		{"late heartbeat", Replication{Mode: "primary", Secondaries: []Peer{{NodeID: "a", ConnectionState: "ready", LastHeartbeat: stale}, {NodeID: "b", ConnectionState: "ready", LastHeartbeat: fresh}}},
			report.StateDegraded, "Primary: a (last heard 10m ago)"},
		{"none streaming", Replication{Mode: "primary", Secondaries: []Peer{{NodeID: "a", ConnectionState: "idle"}}},
			report.StateDown, "Primary: no secondary is streaming: a (idle)"},
		{"none connected", Replication{Mode: "primary", KnownSecondaries: []string{"a", "b"}},
			report.StateDown, "Primary: none of its 2 secondaries are connected"},
		{"new primary", Replication{Mode: "primary"}, report.StateOK, "Primary, with no secondaries yet"},
		{"secondary", Replication{Mode: "secondary", State: "stream-wals", Primaries: []Peer{{APIAddress: "https://p:8200", ConnectionStatus: "connected", LastHeartbeat: fresh}}},
			report.StateOK, "Secondary, streaming from its primary"},
		{"catching up", Replication{Mode: "secondary", State: "merkle-sync"}, report.StateDegraded, "Secondary, catching up with its primary (merkle-sync)"},
		{"cut off", Replication{Mode: "secondary", State: "idle", Primaries: []Peer{{APIAddress: "https://p:8200", ConnectionStatus: "disconnected"}}},
			report.StateDown, "Secondary, not connected to its primary: https://p:8200 (disconnected)"},
		{"corrupt", Replication{Mode: "secondary", State: "stream-wals", Corrupted: true}, report.StateDown, "Its Merkle tree is corrupt: reindex it"},
	} {
		if w := replicationRecord("dr", &c.r, now); w.State != c.state || w.Word != c.word {
			t.Errorf("%s: %s %q, want %s %q", c.name, w.State, w.Word, c.state, c.word)
		}
	}
}

func TestLicense(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for left, want := range map[time.Duration]string{
		90 * 24 * time.Hour: report.StateOK + " Expires in 90 days (2027-02-11)",
		6 * 24 * time.Hour:  report.StateDown + " Expires in 6 days (2026-11-19)",
		-3 * 24 * time.Hour: report.StateDown + " Expired 3 days ago (2026-11-10); Vault seals when it terminates, 2026-12-10",
		20 * 24 * time.Hour: report.StateDegraded + " Expires in 20 days (2026-12-03)",
	} {
		s := &Snapshot{At: now, License: &License{Expires: now.Add(left), Terminates: now.Add(27 * 24 * time.Hour)}}
		if r := licenseRecord(s); r.State+" "+r.Word != want {
			t.Errorf("%v: %s %q, want %q", left, r.State, r.Word, want)
		}
	}
}
