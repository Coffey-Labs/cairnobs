package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// fleetServer is a server with n hosts, signed in through dev sign-in.
// Hosts node00 to node04 report, node03 busiest, each with a restarting
// container (critical); the rest are waiting.
func fleetServer(t *testing.T, n int) (get func(path string, into any)) {
	t.Helper()
	cfg := &Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Hour}
	toks := map[string]string{}
	for i := range n {
		name := fmt.Sprintf("node%02d", i)
		toks[name] = NewAgentToken()
		cfg.Agents = append(cfg.Agents, AgentConfig{Name: name, TokenSHA256: HashToken(toks[name])})
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	for i := range 5 {
		name := fmt.Sprintf("node%02d", i)
		r := report.Report{V: report.Version, CPU: float64(10 + i), OS: "Debian 13",
			Containers: &report.Containers{List: []report.Container{
				{ID: name + "a", Name: "web", Image: "nginx", State: "running", Status: "Up"},
				{ID: name + "b", Name: "db", Image: "postgres", Project: "shop", State: "restarting", Status: "Restarting"},
			}}}
		if i == 3 {
			r.CPU, r.OS = 97, "Rocky 10"
		}
		body, _ := json.Marshal(r)
		if rec := post(h, "/api/v1/agent/report", toks[name], body, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("report %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	return func(path string, into any) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatal(err)
		}
	}
}

type overviewBody struct {
	Hosts []HostView   `json:"hosts"`
	Fleet FleetSummary `json:"fleet"`
}

// A small fleet's overview has every host; a big one's has the most in
// need, and totals for all of them.
func TestOverviewSizedForTheFleet(t *testing.T) {
	var o overviewBody
	fleetServer(t, 6)("/api/v1/overview", &o)
	if len(o.Hosts) != 6 || o.Fleet.Hosts != 6 || o.Fleet.Reporting != 5 {
		t.Fatalf("small fleet: %d hosts, fleet %+v", len(o.Hosts), o.Fleet)
	}
	o = overviewBody{}
	fleetServer(t, 40)("/api/v1/overview", &o)
	if len(o.Hosts) != overviewHosts || o.Fleet.Hosts != 40 || o.Fleet.Reporting != 5 {
		t.Fatalf("big fleet: %d hosts, fleet %+v", len(o.Hosts), o.Fleet)
	}
	if o.Hosts[0].Status == "good" {
		t.Fatalf("the reporting hosts came before the waiting ones: %+v", o.Hosts[0])
	}
	if o.Fleet.Busiest == nil || o.Fleet.Busiest.Host != "node03" || o.Fleet.Busiest.CPU != 97 {
		t.Fatalf("busiest %+v", o.Fleet.Busiest)
	}
}

func TestHostsListFiltersAndPages(t *testing.T) {
	get := fleetServer(t, 40)
	type list struct {
		Hosts  []HostRow      `json:"hosts"`
		Total  int            `json:"total"`
		Counts map[string]int `json:"counts"`
	}
	var l list
	get("/api/v1/hosts?limit=10&offset=30", &l)
	// The five reporting hosts have a restarting container: critical, so first.
	if len(l.Hosts) != 10 || l.Total != 40 || l.Counts["waiting"] != 35 || l.Counts["critical"] != 5 || l.Hosts[9].Name != "node39" {
		t.Fatalf("page 4: %d rows, total %d, counts %v, last %+v", len(l.Hosts), l.Total, l.Counts, l.Hosts[len(l.Hosts)-1])
	}
	l = list{}
	get("/api/v1/hosts?limit=5", &l)
	if l.Hosts[0].Name != "node00" || l.Hosts[4].Name != "node04" || l.Hosts[0].Status != "critical" {
		t.Fatalf("most in need first: %+v", l.Hosts)
	}
	l = list{}
	get("/api/v1/hosts?q=rocky", &l)
	if l.Total != 1 || l.Hosts[0].Name != "node03" || l.Counts["all"] != 1 {
		t.Fatalf("search by OS: %+v", l)
	}
	l = list{}
	get("/api/v1/hosts?status=waiting&offset=100", &l)
	if l.Total != 35 || len(l.Hosts) != 0 || l.Hosts == nil {
		t.Fatalf("past the end: total %d, %v", l.Total, l.Hosts)
	}
	l = list{}
	get("/api/v1/hosts?limit=100000", &l)
	if len(l.Hosts) != 40 {
		t.Fatalf("%d rows", len(l.Hosts))
	}
}

func TestContainersListFiltersAndPages(t *testing.T) {
	get := fleetServer(t, 8)
	type list struct {
		Containers []ContainerRow   `json:"containers"`
		Total      int              `json:"total"`
		Counts     map[string]int   `json:"counts"`
		Hosts      []string         `json:"hosts"`
		Summary    ContainerSummary `json:"summary"`
	}
	var l list
	get("/api/v1/containers", &l)
	if l.Total != 10 || len(l.Hosts) != 5 || l.Summary.Total != 10 || l.Counts["problems"] != 5 {
		t.Fatalf("all: total %d, hosts %v, summary %+v, counts %v", l.Total, l.Hosts, l.Summary, l.Counts)
	}
	if l.Containers[0].Name != "db" || l.Containers[5].Name != "web" {
		t.Fatalf("problems aren't first: %s, %s", l.Containers[0].Name, l.Containers[5].Name)
	}
	l = list{}
	get("/api/v1/containers?host=node02&show=problems", &l)
	if l.Total != 1 || l.Containers[0].Host != "node02" || l.Counts["all"] != 2 || l.Summary.Total != 10 {
		t.Fatalf("one host's problems: %+v", l)
	}
	l = list{}
	get("/api/v1/containers?q=SHOP&limit=2&offset=2", &l)
	if l.Total != 5 || len(l.Containers) != 2 || l.Containers[0].Project != "shop" {
		t.Fatalf("search, page 2: %+v", l)
	}
}

func TestAgentsListPages(t *testing.T) {
	get := fleetServer(t, 40)
	var l struct {
		Agents []agentView `json:"agents"`
		Total  int         `json:"total"`
	}
	get("/api/v1/agents?limit=15&offset=30", &l)
	if l.Total != 40 || len(l.Agents) != 10 || l.Agents[0].Name != "node30" {
		t.Fatalf("page: total %d, %d rows, first %+v", l.Total, len(l.Agents), l.Agents)
	}
	l.Agents = nil
	get("/api/v1/agents?q=NODE03", &l)
	if l.Total != 1 || l.Agents[0].Status != "critical" || l.Agents[0].Word == "" {
		t.Fatalf("search: %+v", l)
	}
}
