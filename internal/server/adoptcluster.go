package server

// Adopting in a cluster (docs/cluster-watcher.md, What pages, and who
// decides). Out of the box only the cluster and its nodes page; a
// workload's problems are warnings on the pages. An admin adopts whole
// namespaces, or single controllers and VMs, on Configure → Clusters → a
// cluster, and those page. Teams can do the same from the cluster with the
// obs.cairnobs.org/alert annotation.
//
// Which wins: a setting in Cairn OBS for the workload itself, then the
// cluster's annotation (the workload's, its controller's or its
// namespace's, as the agent found it), then a setting in Cairn OBS for its
// namespace. So an admin always has the last word on one workload, and a
// team's annotation is respected unless an admin set that very workload.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// adoption is one cluster's adopted namespaces and workloads.
type adoption struct {
	ns       map[string]string
	wl       map[string]ExpectedWorkload
	list     []ExpectedWorkload // in the file's order, for what's missing
	platform bool               // OpenShift: its own namespaces are off unless set
}

func adoptionFor(e *Expected, cluster string) adoption {
	if e == nil {
		return adoption{}
	}
	ec, ok := e.Clusters[cluster]
	if !ok {
		return adoption{}
	}
	a := adoption{ns: ec.Namespaces, wl: make(map[string]ExpectedWorkload, len(ec.Workloads)), list: ec.Workloads}
	for _, w := range ec.Workloads {
		a.wl[w.Name] = w
	}
	return a
}

// recKey is a workload's name as adopting knows it, namespace/kind/name:
// "shop/deployment/web", "vms/vm/db1". A pod is its controller's.
func recKey(r report.Workload) string {
	switch {
	case r.Kind == report.KindController:
		return r.Namespace + "/" + r.Name
	case r.Kind == report.KindPod && r.Owner != "":
		return r.Namespace + "/" + r.Owner
	}
	return r.Namespace + "/" + r.Kind + "/" + r.Name
}

// alert is the alert setting in force for a record; annotated is what the
// agent found on the cluster.
func (a adoption) alert(r report.Workload, annotated string) string {
	if r.Namespace == "" { // the cluster and its nodes: the annotation only
		return annotated
	}
	return a.alertKey(r.Namespace, recKey(r), annotated)
}

func (a adoption) alertKey(ns, key, annotated string) string {
	if w, ok := a.wl[key]; ok {
		if w.Alert == "" {
			return report.AlertPage
		}
		return w.Alert
	}
	if annotated != "" {
		return annotated
	}
	if v, ok := a.ns[ns]; ok {
		return v
	}
	if a.platform && platformNamespace(ns) {
		return report.AlertOff // judged through the ClusterOperators
	}
	return ""
}

// louder is the setting that alerts more: page, then the default or warn,
// then off.
func louder(a, b string) string {
	rank := func(s string) int {
		switch s {
		case report.AlertPage:
			return 2
		case report.AlertOff:
			return 0
		}
		return 1
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// adoptNamespace is a namespace as the adopt page lists it.
type adoptNamespace struct {
	Name        string `json:"name"`
	Controllers int    `json:"controllers"`
	VMs         int    `json:"vms"`
	Problems    int    `json:"problems"`
	Alert       string `json:"alert"` // set in Cairn OBS; "" for none
}

// adoptWorkload is a controller or VM as the adopt page lists it.
type adoptWorkload struct {
	Name      string            `json:"name"` // namespace/kind/name
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Label     string            `json:"label"` // "deployment/web", "vm/db1"
	State     string            `json:"state"`
	Word      string            `json:"word"`
	Severity  string            `json:"severity"`
	Annotated string            `json:"annotated"` // from the cluster
	Set       *ExpectedWorkload `json:"set"`       // in Cairn OBS, or null
	Effective string            `json:"effective"` // page, warn or off: what's in force
}

func (srv *Server) clusterExpected() *Expected {
	if srv.expectedSource() == sourceFile {
		return srv.currentExpected()
	}
	srv.expMu.Lock()
	defer srv.expMu.Unlock()
	return srv.expected
}

func effective(a string) string {
	if a == "" {
		return report.AlertWarn
	}
	return a
}

// handleClusterAdopt lists one cluster's namespaces, and its controllers
// and VMs a page at a time (?ns= and ?q= narrow them), with what's adopted.
func (srv *Server) handleClusterAdopt(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("cluster")
	if !slices.Contains(srv.clusterAgents(), cluster) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No cluster called " + cluster + "."})
		return
	}
	qv := r.URL.Query()
	nsWant, q := qv.Get("ns"), strings.ToLower(strings.TrimSpace(qv.Get("q")))
	offset, limit := page(r, 50, 500)
	ec := srv.clusterExpected().Clusters[cluster]
	ad := adoptionFor(&Expected{Clusters: map[string]ExpectedCluster{cluster: ec}}, cluster)
	nss := map[string]*adoptNamespace{}
	for n, a := range ec.Namespaces {
		nss[n] = &adoptNamespace{Name: n, Alert: a}
	}
	rows := []adoptWorkload{}
	present := map[string]bool{}
	listed := false
	for _, s := range srv.workloadSnapshot() {
		if s.name != cluster {
			continue
		}
		listed = !s.listed.IsZero()
		ad.platform = isOpenShift(s.recs)
		for _, rec := range s.recs {
			if rec.Namespace == "" {
				continue
			}
			n := nss[rec.Namespace]
			if n == nil {
				n = &adoptNamespace{Name: rec.Namespace}
				nss[rec.Namespace] = n
			}
			if rec.Kind != report.KindController && rec.Kind != report.KindVM {
				continue
			}
			key := recKey(rec)
			present[key] = true
			sev := stateSeverity(rec.State)
			if rec.Kind == report.KindVM {
				n.VMs++
			} else {
				n.Controllers++
			}
			if sev == "critical" || sev == "warning" {
				n.Problems++
			}
			if (nsWant != "" && rec.Namespace != nsWant) || (q != "" && !strings.Contains(strings.ToLower(key), q)) {
				continue
			}
			row := adoptWorkload{Name: key, Kind: rec.Kind, Namespace: rec.Namespace, Label: strings.TrimPrefix(key, rec.Namespace+"/"),
				State: rec.State, Word: rec.Word, Severity: sev, Annotated: rec.Alert, Effective: effective(ad.alertKey(rec.Namespace, key, rec.Alert))}
			if x, ok := ad.wl[key]; ok {
				row.Set = &x
			}
			rows = append(rows, row)
		}
	}
	// Adopted first, then problems, then by name.
	slices.SortFunc(rows, func(a, b adoptWorkload) int {
		return cmp.Or(cmp.Compare(boolRank(a.Set == nil), boolRank(b.Set == nil)), cmp.Compare(rank(a.Severity), rank(b.Severity)), strings.Compare(a.Name, b.Name))
	})
	namespaces := slices.SortedFunc(maps.Values(nss), func(a, b *adoptNamespace) int { return strings.Compare(a.Name, b.Name) })
	missing := []ExpectedWorkload{}
	if listed {
		for _, x := range ec.Workloads {
			if !present[x.Name] {
				missing = append(missing, x)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": srv.expectedSource(), "file": srv.cfg.Expected, "cluster": cluster,
		"namespaces": namespaces, "workloads": window(rows, offset, limit), "total": len(rows), "offset": offset, "limit": limit,
		"missing": missing, "adopted": len(ec.Workloads)})
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// adoptForm changes one thing: a namespace, or a workload
// (namespace/kind/name). An empty alert takes it back to the default.
type adoptForm struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Alert     string `json:"alert"`
	Stopped   bool   `json:"stopped"`
}

func (srv *Server) handleClusterAdoptPut(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.expectedSource(), "adopted namespaces and workloads", srv.cfg.Expected) {
		return
	}
	cluster := r.PathValue("cluster")
	if !slices.Contains(srv.clusterAgents(), cluster) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No cluster called " + cluster + "."})
		return
	}
	var f adoptForm
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&f); err != nil || (f.Namespace == "") == (f.Workload == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: one namespace or one workload"})
		return
	}
	srv.expMu.Lock()
	defer srv.expMu.Unlock()
	cur := srv.expected
	if cur == nil {
		cur = &Expected{}
	}
	old := cur.Clusters[cluster]
	ec := ExpectedCluster{Namespaces: maps.Clone(old.Namespaces), Workloads: slices.Clone(old.Workloads), Subsystems: slices.Clone(old.Subsystems)}
	what, did := f.Namespace, ""
	if f.Namespace != "" {
		if ec.Namespaces == nil {
			ec.Namespaces = map[string]string{}
		}
		if f.Alert == "" {
			delete(ec.Namespaces, f.Namespace)
			did = "set back to the default"
		} else {
			ec.Namespaces[f.Namespace] = f.Alert
			did = f.Alert
		}
	} else {
		what = f.Workload
		ec.Workloads = slices.DeleteFunc(ec.Workloads, func(x ExpectedWorkload) bool { return x.Name == f.Workload })
		if f.Alert == "" {
			did = "set back to the default"
		} else {
			ec.Workloads = append(ec.Workloads, ExpectedWorkload{Name: f.Workload, Alert: f.Alert, Stopped: f.Stopped})
			slices.SortFunc(ec.Workloads, func(a, b ExpectedWorkload) int { return strings.Compare(a.Name, b.Name) })
			did = f.Alert
			if f.Stopped {
				did += ", stopped on purpose"
			}
		}
	}
	if err := ec.check(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	next := &Expected{Hosts: cur.Hosts, CIJobs: cur.CIJobs, Clusters: maps.Clone(cur.Clusters)}
	if next.Clusters == nil {
		next.Clusters = map[string]ExpectedCluster{}
	}
	if len(ec.Namespaces) == 0 {
		ec.Namespaces = nil
	}
	if len(ec.Workloads) == 0 {
		ec.Workloads = nil
	}
	if ec.Namespaces == nil && ec.Workloads == nil && len(ec.Subsystems) == 0 {
		delete(next.Clusters, cluster)
	} else {
		next.Clusters[cluster] = ec
	}
	if len(next.Clusters) == 0 {
		next.Clusters = nil
	}
	if err := srv.putExpected(next, srv.who(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.audit(srv.who(r), "set alerting in cluster "+cluster, fmt.Sprintf("%s: %s", what, did))
	writeJSON(w, http.StatusOK, ec)
}
