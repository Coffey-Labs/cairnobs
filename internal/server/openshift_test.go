package server

import (
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// An OpenShift cluster: operators are judged on their own and don't make
// the cluster unreachable, the platform's namespaces are left to them
// unless adopted, and an update softens what it's expected to cause.
func TestOpenShiftClusterJudged(t *testing.T) {
	a, _, _, tok := clusterCopies(t)
	now := time.Now()
	crash := func(id, ns string) report.Workload {
		return report.Workload{Kind: report.KindPod, ID: id, Name: id, Namespace: ns, Owner: "deployment/" + id, State: report.StateCrashloop,
			Word: "Restarting (CrashLoopBackOff)", Since: now.Add(-time.Hour)}
	}
	part := func(id, name, state, word string, facts map[string]any) report.Workload {
		return report.Workload{Kind: report.KindCluster, ID: id, Name: name, State: state, Word: word, Facts: facts}
	}
	recs := []report.Workload{
		{Kind: report.KindCluster, ID: report.ClusterSelf, Name: "prod", State: report.StateOK, Word: "API answering", Facts: map[string]any{"version": "v1.33.5"}},
		part("cv", "clusterversion/version", report.StateOK, "OKD 5.0.0-okd-scos.3", map[string]any{"version": "5.0.0-okd-scos.3", "distribution": "OKD"}),
		part("o1", "clusteroperator/dns", report.StateOK, "Available", nil),
		part("o2", "clusteroperator/ingress", report.StateDegraded, "Degraded: IngressDegraded", nil),
		part("o3", "clusteroperator/authentication", report.StateDown, "Not available: OAuthServerDown", nil),
		part("m1", "machineconfigpool/worker", report.StateOK, "Up to date (3 nodes)", nil),
		{Kind: report.KindNode, ID: "n1", Name: "worker-0", State: report.StateOK, Word: "Ready"},
		crash("router", "openshift-ingress"),
		crash("coredns", "kube-system"),
		crash("web", "shop"),
	}
	sendBatch(t, a, tok, report.Workloads{Full: report.FullBegin, Records: recs})
	sendBatch(t, a, tok, report.Workloads{Full: report.FullEnd})
	judge := func(at time.Time, want string) {
		t.Helper()
		if got := keysOf(a.workloadConditions(at)); got != want {
			t.Fatalf("conditions:\n got %s\nwant %s", got, want)
		}
	}
	judge(now, "critical:cluster/prod/clusteroperator/authentication critical:cluster/prod/clusteroperator/ingress warning:workload/prod/shop/deployment/web")

	// What the Clusters page shows.
	vs := a.clusterViews(now, nil)
	if len(vs) != 1 || vs[0].Version != "OKD 5.0.0-okd-scos.3" || vs[0].Operators != 3 || vs[0].OperatorsOK != 1 || vs[0].Status != "good" {
		t.Fatalf("view: %+v", vs)
	}
	var names []string
	for _, p := range vs[0].Parts {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, " "); got != "clusterversion/version machineconfigpool/worker clusteroperator/authentication clusteroperator/ingress" {
		t.Fatalf("parts: %s", got)
	}

	// Adopting the router's namespace brings it back.
	ec := ExpectedCluster{Namespaces: map[string]string{"openshift-ingress": "page"}}
	a.expMu.Lock()
	a.expected = &Expected{Clusters: map[string]ExpectedCluster{"prod": ec}}
	a.expMu.Unlock()
	judge(now, "critical:cluster/prod/clusteroperator/authentication critical:cluster/prod/clusteroperator/ingress "+
		"critical:workload/prod/openshift-ingress/deployment/router warning:workload/prod/shop/deployment/web")

	// An update starts: operators warn, a failing update doesn't.
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{
		part("m1", "machineconfigpool/worker", report.StateUpdating, "Updating: 1 of 3 nodes done", nil)}})
	judge(now, "warning:cluster/prod/clusteroperator/authentication warning:cluster/prod/clusteroperator/ingress "+
		"critical:workload/prod/openshift-ingress/deployment/router warning:workload/prod/shop/deployment/web")
	sendBatch(t, a, tok, report.Workloads{Records: []report.Workload{
		part("cv", "clusterversion/version", report.StateDegraded, "Update to 5.0.0-okd-scos.4 failing: machine-config degraded", nil)}})
	judge(now, "warning:cluster/prod/clusteroperator/authentication warning:cluster/prod/clusteroperator/ingress critical:cluster/prod/clusterversion/version "+
		"critical:workload/prod/openshift-ingress/deployment/router warning:workload/prod/shop/deployment/web")

	// The agent goes quiet mid-update (a single node rebooting): a warning
	// for up to 30 minutes, then the usual critical.
	if got := keysOf(a.workloadConditions(now.Add(5 * time.Minute))); got != "warning:cluster/prod/silent" {
		t.Fatalf("silent while updating: %s", got)
	}
	if got := keysOf(a.workloadConditions(now.Add(31 * time.Minute))); got != "critical:cluster/prod/silent" {
		t.Fatalf("silent past the stall: %s", got)
	}
}
