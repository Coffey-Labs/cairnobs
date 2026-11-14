package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

const checksYAML = `checks:
  - id: site
    name: example.org
    http: https://example.org/
    contains: Coffey Labs
  - id: mx
    name: mail.example.org SMTP
    tcp: mail.example.org:25
    contains: ESMTP
    every: 2m
  - id: ping-host1
    name: host1 over the private network
    ping: 10.0.0.1
    from: [host2, host3]
    severity: warning
`

// checkServer has three agents and the checks above; it returns their tokens.
func checkServer(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "checks.yaml")
	_ = os.WriteFile(p, []byte(checksYAML), 0o644)
	toks := map[string]string{"host1": NewAgentToken(), "host2": NewAgentToken(), "host3": NewAgentToken()}
	cfg := &Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute, Checks: p}
	for _, h := range []string{"host1", "host2", "host3"} {
		cfg.Agents = append(cfg.Agents, AgentConfig{Name: h, TokenSHA256: HashToken(toks[h])})
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return srv, toks
}

func results(now time.Time, rs ...report.CheckResult) report.Report {
	for i := range rs {
		rs[i].At = now
	}
	return report.Report{V: report.Version, Checks: rs}
}

func viewOf(t *testing.T, views []CheckView, id string) CheckView {
	t.Helper()
	for _, v := range views {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no view for %s", id)
	return CheckView{}
}

func TestChecksNeedTwoHostsToAgree(t *testing.T) {
	srv, _ := checkServer(t)
	now := time.Now()
	soon := now.Add(10 * 24 * time.Hour)
	srv.store.Put("host1", results(now, report.CheckResult{ID: "site", OK: false, Error: "timed out"}, report.CheckResult{ID: "mx", OK: true}), now)
	srv.store.Put("host2", results(now, report.CheckResult{ID: "site", OK: true, CertExpires: soon}, report.CheckResult{ID: "mx", OK: false, Error: "connection refused"}, report.CheckResult{ID: "ping-host1", OK: false, Error: "no reply"}), now)
	srv.store.Put("host3", results(now, report.CheckResult{ID: "site", OK: true}, report.CheckResult{ID: "mx", OK: false, Error: "connection refused"}, report.CheckResult{ID: "ping-host1", OK: false, Error: "no reply"}), now)

	views, conds := srv.judgeChecks(now)
	if v := viewOf(t, views, "site"); v.Status != "warning" || v.Word != "Failing from host1 only" || v.CertDays == nil || *v.CertDays != 10 {
		t.Fatalf("one host failing: %+v", v)
	}
	if v := viewOf(t, views, "mx"); v.Status != "critical" || v.Detail != "Failing from host2, host3: connection refused" {
		t.Fatalf("two hosts failing: %+v", v)
	}
	if v := viewOf(t, views, "ping-host1"); v.Status != "warning" || v.Word != "Down" || len(v.From) != 2 {
		t.Fatalf("ping from its two hosts: %+v", v)
	}
	if got, want := keys(conds), "warning:cert/site critical:check/mx warning:check/ping-host1"; got != want {
		t.Fatalf("conditions\n got  %s\n want %s", got, want)
	}

	// Results from a host that went silent don't count.
	later := now.Add(2 * time.Minute)
	srv.store.Put("host2", results(later, report.CheckResult{ID: "mx", OK: true}), later)
	srv.store.Put("host1", results(later, report.CheckResult{ID: "mx", OK: true}), later)
	views, _ = srv.judgeChecks(later)
	if v := viewOf(t, views, "mx"); v.Status != "good" || len(v.Results) != 2 {
		t.Fatalf("host3 silent: %+v", v)
	}
}

func TestAgentGetsItsChecks(t *testing.T) {
	srv, toks := checkServer(t)
	h := srv.AgentHandler()
	body, _ := json.Marshal(report.Report{V: report.Version})
	rec := post(h, "/api/v1/agent/report", toks["host1"], body, nil)
	ver := rec.Header().Get("X-Beacon-Checks")
	if rec.Code != http.StatusNoContent || len(ver) != 16 {
		t.Fatalf("report answer: %d %q", rec.Code, ver)
	}
	list := func(tok string) (string, []string) {
		req := httptest.NewRequest("GET", "/api/v1/agent/checks", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out struct {
			Version string `json:"version"`
			Checks  []struct {
				ID       string `json:"id"`
				Every    int64  `json:"every"`
				Severity string `json:"severity"`
			} `json:"checks"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var ids []string
		for _, c := range out.Checks {
			ids = append(ids, c.ID)
		}
		if strings.Contains(rec.Body.String(), "severity") || strings.Contains(rec.Body.String(), "from") {
			t.Errorf("judging fields went to the agent: %s", rec.Body)
		}
		return out.Version, ids
	}
	if v, ids := list(toks["host1"]); v != ver || strings.Join(ids, ",") != "site,mx" {
		t.Fatalf("host1: %s %v", v, ids)
	}
	if _, ids := list(toks["host2"]); strings.Join(ids, ",") != "site,mx,ping-host1" {
		t.Fatalf("host2: %v", ids)
	}
	if v, _ := list("not-a-token"); v != "" {
		t.Fatal("unknown token got checks")
	}
}

func TestBadChecksFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "checks.yaml")
	for _, bad := range []string{
		"checks:\n  - id: a\n    http: https://a.test/\n  - id: a\n    ping: a.test\n",
		"checks:\n  - id: a\n    ping: a.test\n    from: [host9]\n",
		"checks:\n  - id: a\n    ping: a.test\n    colour: red\n",
	} {
		_ = os.WriteFile(p, []byte(bad), 0o644)
		if _, err := LoadChecks(p, []AgentConfig{{Name: "host1"}}); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

func TestChecksExampleLoads(t *testing.T) {
	c, err := LoadChecks("../../config/checks.example.yaml", []AgentConfig{{Name: "host1"}, {Name: "host2"}, {Name: "host3"}})
	if err != nil || len(c.Checks) != 5 {
		t.Fatalf("example: %v", err)
	}
}

func TestUnitsExpectedAndJudged(t *testing.T) {
	dir := t.TempDir()
	exp := filepath.Join(dir, "expected.yaml")
	_ = os.WriteFile(exp, []byte("hosts:\n  host1:\n    units: [patroni, etcd, garage.service, inbuxa]\n"), 0o644)
	tok := NewAgentToken()
	srv, err := New(&Config{Listen: "127.0.0.1:0", StaleAfter: time.Minute, Expected: exp,
		Agents: []AgentConfig{{Name: "host1", TokenSHA256: HashToken(tok)}}})
	if err != nil {
		t.Fatal(err)
	}
	// The agent learns which units to watch.
	work, ver := srv.workFor("host1")
	if ver == "" || strings.Join(work.Units, ",") != "patroni.service,etcd.service,garage.service,inbuxa.service" {
		t.Fatalf("work %+v %q", work, ver)
	}
	now := time.Now()
	srv.store.Put("host1", report.Report{V: report.Version, Units: []report.Unit{
		{Name: "patroni.service", Load: "loaded", Active: "active", Sub: "running"},
		{Name: "etcd.service", Load: "loaded", Active: "failed", Sub: "failed"},
		{Name: "garage.service", Load: "not-found", Active: "inactive", Sub: "dead"},
	}}, now)
	hosts, conds := srv.view(now)
	words := []string{}
	for _, u := range hosts[0].Units {
		words = append(words, u.Name+"="+u.Word)
	}
	if got := strings.Join(words, " "); got != "patroni.service=Running etcd.service=Failed garage.service=Not installed inbuxa.service=Not reported yet" {
		t.Fatalf("units: %s", got)
	}
	if got := keys(conds); got != "critical:unit/host1/etcd.service critical:unit/host1/garage.service" {
		t.Fatalf("conditions: %s", got)
	}
}
