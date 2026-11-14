package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

// Adopting in a cluster: a namespace pages, the cluster's annotation beats
// that, a setting for the workload itself beats both, and an adopted
// workload that isn't there, or a VM adopted as running that's stopped, is
// reported.
func TestAdoptingInACluster(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	now := time.Now()
	recs := smallCluster(now)
	for i := range recs { // every pod of web crashing: critical, before alert settings
		if recs[i].Kind == report.KindPod {
			recs[i].State, recs[i].Word = report.StateCrashloop, "Restarting (CrashLoopBackOff)"
		}
	}
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: recs})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	h := a.Handler()
	cookie := post(h, "/auth/dev", "", nil, map[string]string{"X-Beacon": "1"}).Result().Cookies()[0]
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.AddCookie(cookie)
		req.Header.Set("X-Beacon", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	adopt := func(f adoptForm) {
		t.Helper()
		if rec := do("PUT", "/api/v1/settings/clusters/prod/adopt", f); rec.Code != http.StatusOK {
			t.Fatalf("adopt %+v: %d %s", f, rec.Code, rec.Body)
		}
	}
	judge := func(want string) {
		t.Helper()
		if got := keysOf(a.workloadConditions(now)); got != want {
			t.Fatalf("conditions: %s, want %s", got, want)
		}
	}
	judge("warning:workload/prod/shop/deployment/web") // not adopted: a warning

	adopt(adoptForm{Namespace: "shop", Alert: "page"})
	judge("critical:workload/prod/shop/deployment/web")

	// The team says warn on the Deployment: that beats the namespace.
	recs[3].Alert = report.AlertWarn
	sendBatch(t, a, tok, report.Workloads{Records: recs[3:4]})
	judge("warning:workload/prod/shop/deployment/web")

	// An admin sets the Deployment itself: that beats the annotation.
	adopt(adoptForm{Workload: "shop/deployment/web", Alert: "page"})
	judge("critical:workload/prod/shop/deployment/web")
	adopt(adoptForm{Workload: "shop/deployment/web", Alert: "off"})
	judge("")

	// Adopted, and not there.
	adopt(adoptForm{Workload: "shop/statefulset/gone", Alert: "page"})
	judge("critical:workload/prod/shop/statefulset/gone")
	adopt(adoptForm{Workload: "shop/statefulset/gone"}) // taken off again
	judge("")

	// A VM adopted as running, stopped; then marked stopped on purpose.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{{Kind: report.KindVM, ID: "v1", Name: "db1", Namespace: "shop", Node: "worker-2",
		State: report.StateStopped, Word: "Stopped"}}})
	judge("") // stopped, and nobody said it should run
	adopt(adoptForm{Workload: "shop/vm/db1", Alert: "page"})
	judge("critical:vm/prod/shop/db1")
	adopt(adoptForm{Workload: "shop/vm/db1", Alert: "page", Stopped: true})
	judge("")

	// What can't be set.
	for _, f := range []adoptForm{
		{Workload: "shop/deployment/web", Alert: "page", Stopped: true}, // only a VM is stopped on purpose
		{Namespace: "shop", Alert: "loud"},
		{Workload: "deployment/web", Alert: "page"},
		{Namespace: "shop", Workload: "shop/vm/db1", Alert: "page"},
		{Namespace: "Shop!", Alert: "page"},
	} {
		if rec := do("PUT", "/api/v1/settings/clusters/prod/adopt", f); rec.Code != http.StatusBadRequest {
			t.Fatalf("%+v: %d %s", f, rec.Code, rec.Body)
		}
	}
	if rec := do("PUT", "/api/v1/settings/clusters/nowhere/adopt", adoptForm{Namespace: "shop", Alert: "page"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown cluster: %d", rec.Code)
	}

	// The page's view: namespaces with counts, adopted first, what's in force.
	adopt(adoptForm{Workload: "shop/deployment/old", Alert: "warn"})
	rec := do("GET", "/api/v1/settings/clusters/prod/adopt", nil)
	var v struct {
		Namespaces []adoptNamespace   `json:"namespaces"`
		Workloads  []adoptWorkload    `json:"workloads"`
		Missing    []ExpectedWorkload `json:"missing"`
		Total      int                `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(v.Namespaces) != 1 || v.Namespaces[0] != (adoptNamespace{Name: "shop", Controllers: 1, VMs: 1, Problems: 1, Alert: "page"}) {
		t.Fatalf("namespaces: %+v", v.Namespaces)
	}
	if v.Total != 2 || v.Workloads[0].Name != "shop/deployment/web" || v.Workloads[0].Effective != "off" || v.Workloads[0].Annotated != "warn" ||
		v.Workloads[1].Name != "shop/vm/db1" || !v.Workloads[1].Set.Stopped || v.Workloads[1].Label != "vm/db1" {
		t.Fatalf("workloads: %+v", v.Workloads)
	}
	if len(v.Missing) != 1 || v.Missing[0].Name != "shop/deployment/old" {
		t.Fatalf("missing: %+v", v.Missing)
	}

	// Setting a host's containers keeps what's adopted in clusters.
	if rec := do("PUT", "/api/v1/settings/expected/hosts/host1", hostExpectations{Running: []string{"web"}}); rec.Code != http.StatusOK {
		t.Fatalf("host: %d %s", rec.Code, rec.Body)
	}
	if ec := a.clusterExpected().Clusters["prod"]; ec.Namespaces["shop"] != "page" || len(ec.Workloads) != 3 {
		t.Fatalf("after a host's save: %+v", ec)
	}
	// And it's in expected.yaml, as exported.
	if rec := do("GET", "/api/v1/settings/expected/export", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "name: shop/vm/db1") {
		t.Fatalf("export: %d %s", rec.Code, rec.Body)
	}
}

// With expected.yaml in a file, a cluster's adopting is read from it, the
// page can't change it, and judging follows it.
func TestClusterAdoptingFromAFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "expected.yaml")
	_ = os.WriteFile(f, []byte("clusters:\n  prod:\n    namespaces:\n      shop: page\n"), 0o600)
	key := filepath.Join(dir, "state.key")
	_ = os.WriteFile(key, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o600)
	tok := NewAgentToken()
	srv, err := New(&Config{Listen: "127.0.0.1:0", DevSignin: true, StaleAfter: time.Minute, Expected: f,
		Database: statetest.Database(t, dir), DataDir: dir, StateKeyFile: key,
		Agents: []AgentConfig{{Name: "prod", TokenSHA256: HashToken(tok), Cluster: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := srv.Handler()
	c := devCookie(t, h)
	if rec := sendCookie(h, "PUT", "/api/v1/settings/clusters/prod/adopt", c, `{"namespace":"shop","alert":"off"}`); rec.Code != 409 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if body := get(h, "/api/v1/settings/clusters/prod/adopt", c).Body.String(); !strings.Contains(body, `"source":"file"`) || !strings.Contains(body, `"alert":"page"`) {
		t.Fatalf("get: %s", body)
	}
	now := time.Now()
	recs := smallCluster(now)
	for i := range recs {
		if recs[i].Kind == report.KindPod {
			recs[i].State, recs[i].Word = report.StateCrashloop, "Restarting (CrashLoopBackOff)"
		}
	}
	sendBatch(t, srv, tok, report.Workloads{Full: report.FullBegin, Records: recs})
	sendBatch(t, srv, tok, report.Workloads{Full: report.FullEnd})
	if got := keysOf(srv.workloadConditions(now)); got != "critical:workload/prod/shop/deployment/web" {
		t.Fatalf("conditions: %s", got)
	}
}
