package vault

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

func TestParseMetrics(t *testing.T) {
	m, err := parseMetrics(strings.NewReader(`# HELP vault_expire_num_leases Number of leases
# TYPE vault_expire_num_leases gauge
vault_expire_num_leases 1234
vault_expire_num_irrevocable_leases 2
vault_expire_leases_by_expiration{cluster="c",expiring="2026-11-14 07:00:00 +0000 UTC"} 4
vault_audit_log_request_failure 3
vault_audit_log_response_failure{x="a b"} 1 1700000000000
vault_audit_log_request_count 158
go_gc_duration_seconds{quantile="0"} 2.7e-05
`))
	if err != nil || m != (Metrics{Leases: 1234, Irrevocable: 2, HasLeases: true, AuditFailures: 4, HasAudit: true}) {
		t.Errorf("%+v %v", m, err)
	}
}

// An audit failure is critical while the count goes up and for
// auditHold after; a server restarting (the count lower) isn't one.
func TestAuditFailures(t *testing.T) {
	var failures atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/health":
			fmt.Fprint(w, `{"initialized":true,"sealed":false,"version":"1.21.4"}`)
		case "/v1/sys/leader":
			fmt.Fprint(w, `{"ha_enabled":false}`)
		case "/v1/sys/metrics":
			fmt.Fprintf(w, "vault_expire_num_leases 42\nvault_audit_log_request_failure %d\nvault_audit_log_response_failure 0\n", failures.Load())
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[]}`)
		}
	}))
	defer srv.Close()
	c, _ := newClient(Config{Name: "fake", URL: srv.URL, TokenFile: writeToken(t, "t")})
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	audit := func(min int) report.Workload {
		t.Helper()
		s, err := c.Read(context.Background(), at.Add(time.Duration(min)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if self := Records(s)[0]; self.Facts["leases"] != int64(42) || self.State != report.StateOK {
			t.Errorf("minute %d: the cluster %+v", min, self)
		}
		if u := s.Usage(); len(u) != 1 || *u[0].Leases != 42 {
			t.Errorf("minute %d: usage %+v", min, u)
		}
		for _, r := range Records(s) {
			if r.Name == AuditPart {
				return r
			}
		}
		t.Fatalf("minute %d: no audit record", min)
		return report.Workload{}
	}
	failures.Store(5) // failed before obs-beacond started: not counted
	if r := audit(0); r.State != report.StateOK {
		t.Errorf("first read: %+v", r)
	}
	failures.Store(8)
	if r := audit(1); r.State != report.StateDown || r.Word != "Writing to it failed 3 times, last at 12:01 UTC: Vault refuses what it can't audit" {
		t.Errorf("failing: %+v", r)
	}
	failures.Store(9)
	if r := audit(2); r.State != report.StateDown || !strings.HasPrefix(r.Word, "Writing to it failed 4 times, last at 12:02") {
		t.Errorf("failing more: %+v", r)
	}
	if r := audit(17); r.State != report.StateDown {
		t.Errorf("within the hold: %+v", r)
	}
	if r := audit(18); r.State != report.StateOK {
		t.Errorf("after the hold: %+v", r)
	}
	failures.Store(0) // restarted
	if r := audit(19); r.State != report.StateOK {
		t.Errorf("restarted: %+v", r)
	}
	failures.Store(1)
	if r := audit(20); r.State != report.StateDown || !strings.HasPrefix(r.Word, "Writing to it failed 1 time,") {
		t.Errorf("after a restart: %+v", r)
	}
}

// TestRealMetrics reads the dev servers' metrics with the monitoring
// token, with an audit device on.
func TestRealMetrics(t *testing.T) {
	addrs := os.Getenv("CAIRN_OBS_TEST_VAULT")
	if addrs == "" {
		t.Skip("CAIRN_OBS_TEST_VAULT not set")
	}
	for _, base := range strings.Split(addrs, ",") {
		t.Run(base, func(t *testing.T) {
			path := fmt.Sprintf("obs%d", time.Now().UnixNano()%1e9)
			// OpenBao 2.4 takes audit devices only from its config file:
			// there, no device and no audit record.
			rc, _ := newClient(Config{Name: "setup", URL: base, TokenFile: writeToken(t, "root-ci")})
			_, err := rc.call(context.Background(), http.MethodPut, "sys/audit/"+path, true, map[string]any{"type": "file", "options": map[string]any{"file_path": "discard"}}, nil)
			audited := err == nil
			if audited {
				t.Cleanup(func() { rootCall(t, base, "root-ci", http.MethodDelete, "sys/audit/"+path, nil, nil) })
			} else if !strings.Contains(err.Error(), "declarative") {
				t.Fatal(err)
			}
			rootCall(t, base, "root-ci", http.MethodPut, "sys/policies/acl/obs-monitor", map[string]any{"policy": policy}, nil)
			var made struct {
				Auth struct {
					ClientToken string `json:"client_token"`
				} `json:"auth"`
			}
			rootCall(t, base, "root-ci", http.MethodPost, "auth/token/create", map[string]any{"policies": []string{"obs-monitor"}, "ttl": "1h"}, &made)
			c, _ := newClient(Config{Name: "dev", URL: base, TokenFile: writeToken(t, made.Auth.ClientToken)})
			s, err := c.Read(context.Background(), time.Now())
			if err != nil || len(s.Problems) > 0 || s.Metrics == nil {
				t.Fatalf("%v %v %+v", err, s.Problems, s.Metrics)
			}
			// A new server counts its leases a while after it starts: 0
			// until then.
			if !s.Metrics.HasLeases || s.Metrics.Leases < 0 || audited && (!s.Metrics.HasAudit || s.Metrics.AuditFailures != 0) {
				t.Errorf("metrics %+v", s.Metrics)
			}
			var audit *report.Workload
			for _, r := range Records(s) {
				if r.Name == AuditPart {
					audit = &r
				}
			}
			if audited && (audit == nil || audit.State != report.StateOK) {
				t.Errorf("audit %+v", audit)
			}
		})
	}
}
