package clusterwatch

// OpenShift and OKD (docs/cluster-watcher.md, plan step 3): the cluster's
// version and whether an update is under way, its operators, and its
// machine config pools, sent as parts of the cluster (kind cluster, named
// "clusterversion/version", "clusteroperator/ingress",
// "machineconfigpool/worker"). A node the machine config operator is
// updating is Updating, not down, even before it's cordoned.

import (
	"fmt"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// The machine config operator's annotations on a node.
const (
	mcoState   = "machineconfiguration.openshift.io/state" // Done, Working or Degraded
	mcoReason  = "machineconfiguration.openshift.io/reason"
	mcoCurrent = "machineconfiguration.openshift.io/currentConfig"
	mcoDesired = "machineconfiguration.openshift.io/desiredConfig"
)

// part starts a record for one of the cluster's parts.
func (b *builder) part(name string, m meta) report.Workload {
	r := b.rec(report.KindCluster, name, m)
	r.Namespace, r.Labels = "", nil
	return r
}

// condWord is a condition's reason and message, short: "Degraded:
// IngressDegraded: The ingress controller…".
func condWord(prefix string, c *condition) string {
	w := prefix
	if c.Reason != "" {
		w += ": " + c.Reason
	}
	if c.Message != "" {
		w += ": " + c.Message
	}
	return short(w, 200)
}

// distribution names OpenShift's community build by its version string.
func distribution(version string) string {
	if strings.Contains(version, "okd") {
		return "OKD"
	}
	return "OpenShift"
}

func (b *builder) clusterVersion(cv *clusterVersion) report.Workload {
	r := b.part("clusterversion/"+cv.Metadata.Name, cv.Metadata)
	current := ""
	for _, h := range cv.Status.History { // newest first
		if h.State == "Completed" {
			current = h.Version
			break
		}
	}
	desired := cv.Status.Desired.Version
	if current == "" {
		current = desired
	}
	r.Facts = map[string]any{"version": current, "distribution": distribution(current + desired)}
	if cv.Spec.Channel != "" {
		r.Facts["channel"] = cv.Spec.Channel
	}
	progressing := findCond(cv.Status.Conditions, "Progressing")
	failing := findCond(cv.Status.Conditions, "Failing")
	updating := progressing != nil && progressing.Status == "True"
	if updating && desired != "" && desired != current {
		r.Facts["desired"] = desired
	}
	switch {
	case failing != nil && failing.Status == "True" && updating:
		r.State, r.Since = report.StateDegraded, failing.LastTransitionTime
		r.Word = short(fmt.Sprintf("Update to %s failing: %s", desired, failing.Message), 200)
	case failing != nil && failing.Status == "True":
		r.State, r.Word, r.Since = report.StateDegraded, condWord("Failing", failing), failing.LastTransitionTime
	case updating:
		r.State, r.Since = report.StateUpdating, progressing.LastTransitionTime
		r.Word = "Updating"
		if desired != "" && desired != current {
			r.Word += " to " + desired
		}
		if progressing.Message != "" {
			r.Word = short(r.Word+": "+progressing.Message, 200)
		}
	default:
		r.State, r.Word = report.StateOK, distribution(current)+" "+current
	}
	return r
}

func (b *builder) clusterOperator(co *clusterOperator) report.Workload {
	r := b.part("clusteroperator/"+co.Metadata.Name, co.Metadata)
	for _, v := range co.Status.Versions {
		if v.Name == "operator" {
			r.Facts = map[string]any{"version": v.Version}
		}
	}
	available := findCond(co.Status.Conditions, "Available")
	degraded := findCond(co.Status.Conditions, "Degraded")
	progressing := findCond(co.Status.Conditions, "Progressing")
	switch {
	case available == nil:
		r.State, r.Word = report.StateStarting, "No Available condition yet"
	case available.Status != "True":
		r.State, r.Word, r.Since = report.StateDown, condWord("Not available", available), available.LastTransitionTime
	case degraded != nil && degraded.Status == "True":
		r.State, r.Word, r.Since = report.StateDegraded, condWord("Degraded", degraded), degraded.LastTransitionTime
	case progressing != nil && progressing.Status == "True":
		r.State, r.Word, r.Since = report.StateUpdating, condWord("Progressing", progressing), progressing.LastTransitionTime
	default:
		r.State, r.Word, r.Since = report.StateOK, "Available", available.LastTransitionTime
	}
	return r
}

func (b *builder) machineConfigPool(p *machineConfigPool) report.Workload {
	r := b.part("machineconfigpool/"+p.Metadata.Name, p.Metadata)
	st := p.Status
	r.Facts = map[string]any{"nodes": st.MachineCount, "updated": st.UpdatedMachineCount, "ready": st.ReadyMachineCount}
	if st.DegradedMachineCount > 0 {
		r.Facts["degraded"] = st.DegradedMachineCount
	}
	degraded := findCond(st.Conditions, "Degraded")
	updating := findCond(st.Conditions, "Updating")
	updated := findCond(st.Conditions, "Updated")
	switch {
	case degraded != nil && degraded.Status == "True":
		r.State, r.Word, r.Since = report.StateDegraded, condWord("Degraded", degraded), degraded.LastTransitionTime
	case p.Spec.Paused && (updated == nil || updated.Status != "True"):
		r.State, r.Word = report.StateOK, fmt.Sprintf("Paused, with an update waiting (%d of %d nodes updated)", st.UpdatedMachineCount, st.MachineCount)
	case p.Spec.Paused:
		r.State, r.Word = report.StateOK, "Paused"
	case updating != nil && updating.Status == "True":
		r.State, r.Since = report.StateUpdating, updating.LastTransitionTime
		r.Word = fmt.Sprintf("Updating: %d of %d nodes done", st.UpdatedMachineCount, st.MachineCount)
	default:
		r.State, r.Word = report.StateOK, fmt.Sprintf("Up to date (%s)", nodesWord(st.MachineCount))
	}
	return r
}

func nodesWord(n int) string {
	if n == 1 {
		return "1 node"
	}
	return fmt.Sprintf("%d nodes", n)
}

// mcoUpdating says whether the machine config operator is working on a
// node, and its word for it; degraded says the operator couldn't.
func mcoUpdating(m meta) (updating, degraded bool, word string) {
	a := m.Annotations
	switch {
	case a[mcoState] == "Degraded":
		return false, true, short("Machine config degraded: "+a[mcoReason], 200)
	case a[mcoState] == "Working", a[mcoCurrent] != "" && a[mcoDesired] != "" && a[mcoCurrent] != a[mcoDesired]:
		return true, false, "Updating its machine config"
	}
	return false, false, ""
}
