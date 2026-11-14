package clusterwatch

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// alertAnnotation is how a team sets alerting in its own manifests
// (docs/cluster-watcher.md, What pages): page, warn or off, on a
// namespace, a controller, a job, a volume or a pod.
const alertAnnotation = "obs.cairnobs.org/alert"

// labelPrefix is the labels kept on a record; the rest stay in the
// cluster.
const labelPrefix = "app.kubernetes.io/"

// view is every object the agent holds, by resource name and uid.
type view map[string]map[string]any

func each[T any](v view, res string) []*T {
	out := make([]*T, 0, len(v[res]))
	for _, o := range v[res] {
		if t, ok := o.(*T); ok {
			out = append(out, t)
		}
	}
	return out
}

// newBuilder knows, from the view, what records need to say about each
// other: alert annotations, which Deployment a ReplicaSet stands for,
// each CronJob's latest run.
func newBuilder(v view) *builder {
	b := &builder{nsAlert: map[string]string{}, ownAlert: map[string]string{}, rsOwner: map[string]string{}, latestRun: map[string]time.Time{}}
	own := func(kind string, m meta) {
		if a := alertValue(m.Annotations); a != "" {
			b.ownAlert[m.Namespace+"/"+kind+"/"+m.Name] = a
		}
	}
	for _, d := range each[deployment](v, "deployments") {
		own("deployment", d.Metadata)
	}
	for _, s := range each[statefulSet](v, "statefulsets") {
		own("statefulset", s.Metadata)
	}
	for _, d := range each[daemonSet](v, "daemonsets") {
		own("daemonset", d.Metadata)
	}
	for _, j := range each[job](v, "jobs") {
		own("job", j.Metadata)
	}
	for _, ns := range each[namespace](v, "namespaces") {
		b.nsAlert[ns.Metadata.Name] = alertValue(ns.Metadata.Annotations)
	}
	// A ReplicaSet a Deployment made stands for the Deployment.
	for _, rs := range each[replicaSet](v, "replicasets") {
		if o := rs.Metadata.controllerRef(); o != nil && o.Kind == "Deployment" {
			b.rsOwner[rs.Metadata.Namespace+"/"+rs.Metadata.Name] = "deployment/" + o.Name
		}
	}
	for _, j := range each[job](v, "jobs") {
		if o := j.Metadata.controllerRef(); o != nil && o.Kind == "CronJob" {
			k := j.Metadata.Namespace + "/" + o.Name
			if t := j.Metadata.CreationTimestamp; t.After(b.latestRun[k]) {
				b.latestRun[k] = t
			}
		}
	}
	return b
}

// build turns what the agent holds into records. It's a pure function of
// the objects, so every batch says the same thing about the same state.
// Since is the time Kubernetes gives for the state, when it gives one; the
// sender keeps it from there.
func build(v view) []report.Workload {
	b := newBuilder(v)
	var out []report.Workload
	for _, n := range each[node](v, "nodes") {
		out = append(out, b.node(n))
	}
	dvs := map[string]*dataVolume{}
	for _, d := range each[dataVolume](v, "datavolumes") {
		dvs[nsName(d.Metadata)] = d
	}
	for _, p := range each[pod](v, "pods") {
		if !launcherPod(p) && !dvWorker(p, dvs) {
			out = append(out, b.pod(p))
		}
	}
	for _, d := range each[deployment](v, "deployments") {
		out = append(out, b.deployment(d))
	}
	for _, s := range each[statefulSet](v, "statefulsets") {
		out = append(out, b.statefulSet(s))
	}
	for _, d := range each[daemonSet](v, "daemonsets") {
		out = append(out, b.daemonSet(d))
	}
	for _, rs := range each[replicaSet](v, "replicasets") {
		if rs.Metadata.controllerRef() == nil { // a bare ReplicaSet is a controller of its own
			out = append(out, b.replicaSet(rs))
		}
	}
	for _, j := range each[job](v, "jobs") {
		out = append(out, b.job(j))
	}
	for _, c := range each[cronJob](v, "cronjobs") {
		out = append(out, b.cronJob(c))
	}
	for _, c := range each[pvc](v, "persistentvolumeclaims") {
		r := b.pvc(c)
		if d := dvs[nsName(c.Metadata)]; d != nil {
			dataVolumeState(&r, d)
			delete(dvs, nsName(c.Metadata))
		}
		out = append(out, r)
	}
	// A DataVolume whose volume isn't made yet stands for it.
	for _, d := range dvs {
		r := b.rec(report.KindPVC, d.Metadata.Name, d.Metadata)
		r.State, r.Word, r.Since = report.StatePending, "Pending (its DataVolume hasn't made it yet)", d.Metadata.CreationTimestamp
		dataVolumeState(&r, d)
		out = append(out, r)
	}
	out = append(out, b.vmRecords(v)...)
	out = append(out, b.edgeRecords(v)...)
	out = append(out, b.certRecords(v, nowFunc())...)
	for _, cv := range each[clusterVersion](v, "clusterversions") {
		out = append(out, b.clusterVersion(cv))
	}
	for _, co := range each[clusterOperator](v, "clusteroperators") {
		out = append(out, b.clusterOperator(co))
	}
	for _, p := range each[machineConfigPool](v, "machineconfigpools") {
		out = append(out, b.machineConfigPool(p))
	}
	slices.SortFunc(out, func(a, b report.Workload) int { return strings.Compare(a.ID, b.ID) })
	return out
}

type builder struct {
	nsAlert   map[string]string    // namespace → its alert annotation
	ownAlert  map[string]string    // ns/"deployment/web" → the controller's own alert annotation
	rsOwner   map[string]string    // ns/replicaset → "deployment/web"
	latestRun map[string]time.Time // ns/cronjob → its newest Job's creation
}

func alertValue(a map[string]string) string {
	switch v := strings.ToLower(strings.TrimSpace(a[alertAnnotation])); v {
	case report.AlertPage, report.AlertWarn, report.AlertOff:
		return v
	}
	return ""
}

// rec starts a record from an object's metadata: its own alert setting,
// else its namespace's, and its app.kubernetes.io labels.
func (b *builder) rec(kind, name string, m meta) report.Workload {
	r := report.Workload{Kind: kind, ID: m.UID, Name: name, Namespace: m.Namespace, Alert: alertValue(m.Annotations)}
	if r.Alert == "" {
		r.Alert = b.nsAlert[m.Namespace]
	}
	for k, v := range m.Labels {
		if strings.HasPrefix(k, labelPrefix) {
			if r.Labels == nil {
				r.Labels = map[string]string{}
			}
			r.Labels[k] = v
		}
	}
	return r
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func (b *builder) node(n *node) report.Workload {
	r := b.rec(report.KindNode, n.Metadata.Name, n.Metadata)
	r.Namespace = ""
	var roles []string
	for k := range n.Metadata.Labels {
		if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
			roles = append(roles, role)
		}
	}
	slices.Sort(roles)
	r.Facts = map[string]any{"kubelet": n.Status.NodeInfo.KubeletVersion}
	if os := n.Status.NodeInfo.OSImage; os != "" {
		r.Facts["os"] = os // Red Hat Enterprise Linux CoreOS 9.6…, on OpenShift
	}
	if a := n.Status.NodeInfo.Architecture; a != "" {
		r.Facts["arch"] = a
	}
	if len(roles) > 0 {
		r.Facts["roles"] = strings.Join(roles, ",")
	}
	for _, k := range []string{"cpu", "memory"} {
		if q := n.Status.Allocatable[k]; q != "" {
			r.Facts[k] = q
		}
	}
	ready := findCond(n.Status.Conditions, "Ready")
	switch {
	case ready == nil:
		r.State, r.Word = report.StateUnknown, "No Ready condition yet"
	case ready.Status == "True":
		r.State, r.Word, r.Since = report.StateOK, "Ready", ready.LastTransitionTime
	case ready.Status == "Unknown":
		r.State, r.Word, r.Since = report.StateDown, "Not responding (the kubelet stopped reporting)", ready.LastTransitionTime
	default:
		r.State, r.Word, r.Since = report.StateDown, "Not ready", ready.LastTransitionTime
		if ready.Reason != "" && ready.Reason != "KubeletNotReady" {
			r.Word += " (" + ready.Reason + ")"
		}
	}
	// The machine config operator updating it (OpenShift, OKD): Updating,
	// even while it reboots and before it's cordoned.
	if updating, degraded, word := mcoUpdating(n.Metadata); updating && r.State != report.StateUnknown {
		if r.State == report.StateDown {
			word += ", not ready (rebooting)"
		}
		r.State, r.Word = report.StateUpdating, word
		return r
	} else if degraded && r.State == report.StateOK {
		r.State, r.Word = report.StateDegraded, word
		return r
	}
	// Cordoned: being updated or drained by hand. Not down, even while it
	// reboots (docs/cluster-watcher.md, Updates aren't outages).
	if n.Spec.Unschedulable && r.State != report.StateUnknown {
		word := "Cordoned"
		if r.State == report.StateDown {
			word = "Cordoned, not ready"
		}
		r.State, r.Word = report.StateUpdating, word
		return r
	}
	if r.State == report.StateOK {
		for _, p := range []struct{ t, word string }{
			{"MemoryPressure", "Memory pressure"}, {"DiskPressure", "Disk pressure"},
			{"PIDPressure", "Process ID pressure"}, {"NetworkUnavailable", "Network unavailable"},
		} {
			if c := findCond(n.Status.Conditions, p.t); c != nil && c.Status == "True" {
				r.State, r.Word, r.Since = report.StateDegraded, p.word, c.LastTransitionTime
				break
			}
		}
	}
	return r
}

// podOwner is what a pod belongs to, as the server groups it:
// "deployment/web" for a ReplicaSet a Deployment made.
func (b *builder) podOwner(m meta) (owner, kind string) {
	o := m.controllerRef()
	if o == nil {
		return "", ""
	}
	if o.Kind == "ReplicaSet" {
		if d := b.rsOwner[m.Namespace+"/"+o.Name]; d != "" {
			return d, "Deployment"
		}
	}
	return strings.ToLower(o.Kind) + "/" + o.Name, o.Kind
}

// imageTrouble are waiting reasons that mean the container can't start
// until someone fixes something.
var imageTrouble = map[string]bool{
	"ImagePullBackOff": true, "ErrImagePull": true, "InvalidImageName": true, "ErrImageNeverPull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true,
}

func (b *builder) pod(p *pod) report.Workload {
	r := b.rec(report.KindPod, p.Metadata.Name, p.Metadata)
	r.Node = p.Spec.NodeName
	var ownerKind string
	r.Owner, ownerKind = b.podOwner(p.Metadata)
	if a := b.ownAlert[p.Metadata.Namespace+"/"+r.Owner]; a != "" && alertValue(p.Metadata.Annotations) == "" {
		r.Alert = a // its controller's setting before its namespace's
	}
	st := p.Status
	ready, restarts := 0, 0
	var waiting, lastExit string
	crashing := false
	for _, c := range st.ContainerStatuses {
		if c.Ready {
			ready++
		}
		restarts += c.RestartCount
		if w := c.State.Waiting; w != nil && waiting == "" && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
			waiting = w.Reason
		}
		if t := c.LastState.Terminated; t != nil {
			lastExit = t.word()
		}
		// Between back-offs a crashing container shows as just exited,
		// not waiting: the same trouble, said the same way.
		if t := c.State.Terminated; t != nil && c.RestartCount > 0 && st.Phase == "Running" {
			crashing, lastExit = true, t.word()
		}
	}
	initWaiting := ""
	for _, c := range st.InitContainerStatuses {
		if w := c.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			initWaiting = w.Reason
		}
	}
	r.Facts = map[string]any{"ready": fmt.Sprintf("%d/%d", ready, len(st.ContainerStatuses))}
	if restarts > 0 {
		r.Facts["restarts"] = restarts
	}
	if lastExit != "" {
		r.Facts["last_exit"] = lastExit
	}
	r.Since = p.Metadata.CreationTimestamp
	if c := findCond(st.Conditions, "Ready"); c != nil && !c.LastTransitionTime.IsZero() {
		r.Since = c.LastTransitionTime
	}
	jobPod := ownerKind == "Job"
	switch {
	case p.Metadata.DeletionTimestamp != nil:
		r.State, r.Word, r.Since = report.StateStopped, "Terminating", *p.Metadata.DeletionTimestamp
	case st.Phase == "Succeeded":
		r.State, r.Word = report.StateStopped, "Completed"
	case st.Phase == "Failed" && (jobPod || st.Reason == "Evicted" || st.Reason == "Shutdown" || st.Reason == "NodeShutdown" || st.Reason == "Terminated"):
		// The Job is judged, not its pods; an evicted pod has a
		// replacement, which is.
		word := "Failed"
		if st.Reason != "" {
			word = st.Reason
		}
		r.State, r.Word = report.StateStopped, word
	case st.Phase == "Failed":
		r.State, r.Word = report.StateDown, "Failed"
		if st.Reason != "" {
			r.Word += " (" + st.Reason + ")"
		}
	case waiting == "CrashLoopBackOff" || initWaiting == "CrashLoopBackOff" || crashing:
		r.State, r.Word = report.StateCrashloop, "Restarting (CrashLoopBackOff)"
		if initWaiting == "CrashLoopBackOff" {
			r.Word = "Init container restarting (CrashLoopBackOff)"
		}
		if lastExit != "" {
			r.Word += ", last " + lastExit
		}
	case imageTrouble[waiting] || imageTrouble[initWaiting]:
		w := waiting
		if w == "" {
			w = initWaiting
		}
		word := "Can't start"
		if strings.Contains(w, "Image") {
			word = "Can't pull its image"
		}
		r.State, r.Word = report.StateDown, word+" ("+w+")"
	case st.Phase == "Pending":
		r.State, r.Word = report.StatePending, "Pending"
		if c := findCond(st.Conditions, "PodScheduled"); c != nil && c.Status == "False" {
			r.Word = "Can't be scheduled"
			if c.Message != "" {
				r.Word += ": " + short(c.Message, 120)
			}
		} else if p.Spec.NodeName != "" {
			r.State, r.Word = report.StateStarting, "Starting"
		}
	case st.Phase == "Running" && ready == len(st.ContainerStatuses):
		r.State, r.Word = report.StateOK, "Running"
	case st.Phase == "Running":
		r.State, r.Word = report.StateStarting, fmt.Sprintf("Running, %d of %d containers ready", ready, len(st.ContainerStatuses))
	default:
		r.State, r.Word = report.StateUnknown, "Unknown"
		if st.Phase != "" {
			r.Word = st.Phase
		}
	}
	return r
}

// replicas is a controller's state from desired and ready, as the server
// words it ("2 of 3 ready"); rolling says an update is under way.
func replicas(desired, ready int, rolling bool, updated int) (state, word string) {
	switch {
	case desired == 0:
		return report.StateStopped, "Scaled to 0"
	case rolling:
		return report.StateUpdating, fmt.Sprintf("Rolling out, %d of %d updated", min(updated, desired), desired)
	case ready >= desired:
		return report.StateOK, fmt.Sprintf("%d of %d ready", desired, desired)
	case ready == 0:
		return report.StateDown, fmt.Sprintf("0 of %d ready", desired)
	}
	return report.StateDegraded, fmt.Sprintf("%d of %d ready", ready, desired)
}

func orOne(p *int) int {
	if p == nil {
		return 1
	}
	return *p
}

func (b *builder) deployment(d *deployment) report.Workload {
	r := b.rec(report.KindController, "deployment/"+d.Metadata.Name, d.Metadata)
	desired, st := orOne(d.Spec.Replicas), d.Status
	stuck := findCond(st.Conditions, "Progressing")
	rolling := st.ObservedGeneration < d.Metadata.Generation || st.UpdatedReplicas < desired || st.Replicas > desired
	r.State, r.Word = replicas(desired, st.ReadyReplicas, rolling && st.ReadyReplicas < desired, st.UpdatedReplicas)
	if stuck != nil && stuck.Status == "False" && stuck.Reason == "ProgressDeadlineExceeded" && desired > 0 {
		r.State, r.Word, r.Since = report.StateDegraded, fmt.Sprintf("Rollout stuck, %d of %d ready (ProgressDeadlineExceeded)", st.ReadyReplicas, desired), stuck.LastTransitionTime
	} else if c := findCond(st.Conditions, "Available"); c != nil {
		r.Since = c.LastTransitionTime
	}
	r.Facts = map[string]any{"desired": desired, "ready": st.ReadyReplicas, "updated": st.UpdatedReplicas}
	return r
}

func (b *builder) statefulSet(s *statefulSet) report.Workload {
	r := b.rec(report.KindController, "statefulset/"+s.Metadata.Name, s.Metadata)
	desired, st := orOne(s.Spec.Replicas), s.Status
	rolling := st.ObservedGeneration < s.Metadata.Generation || (st.UpdateRevision != "" && st.CurrentRevision != st.UpdateRevision)
	r.State, r.Word = replicas(desired, st.ReadyReplicas, rolling && st.ReadyReplicas < desired, st.UpdatedReplicas)
	r.Facts = map[string]any{"desired": desired, "ready": st.ReadyReplicas}
	return r
}

func (b *builder) daemonSet(d *daemonSet) report.Workload {
	r := b.rec(report.KindController, "daemonset/"+d.Metadata.Name, d.Metadata)
	st := d.Status
	rolling := st.ObservedGeneration < d.Metadata.Generation || st.UpdatedNumberScheduled < st.DesiredNumberScheduled
	r.State, r.Word = replicas(st.DesiredNumberScheduled, st.NumberReady, rolling && st.NumberReady < st.DesiredNumberScheduled, st.UpdatedNumberScheduled)
	if st.DesiredNumberScheduled == 0 {
		r.State, r.Word = report.StateOK, "On no nodes"
	}
	r.Facts = map[string]any{"desired": st.DesiredNumberScheduled, "ready": st.NumberReady}
	return r
}

func (b *builder) replicaSet(rs *replicaSet) report.Workload {
	r := b.rec(report.KindController, "replicaset/"+rs.Metadata.Name, rs.Metadata)
	desired := orOne(rs.Spec.Replicas)
	r.State, r.Word = replicas(desired, rs.Status.ReadyReplicas, false, 0)
	r.Facts = map[string]any{"desired": desired, "ready": rs.Status.ReadyReplicas}
	return r
}

func (b *builder) job(j *job) report.Workload {
	r := b.rec(report.KindJob, j.Metadata.Name, j.Metadata)
	st := j.Status
	if o := j.Metadata.controllerRef(); o != nil {
		r.Owner = strings.ToLower(o.Kind) + "/" + o.Name
	}
	r.Facts = map[string]any{"succeeded": st.Succeeded, "failed": st.Failed}
	if st.StartTime != nil {
		r.Since = *st.StartTime
	}
	failed, complete := findCond(st.Conditions, "Failed"), findCond(st.Conditions, "Complete")
	switch {
	case failed != nil && failed.Status == "True":
		r.State, r.Word, r.Since = report.StateDown, "Failed", failed.LastTransitionTime
		if failed.Reason != "" {
			r.Word += " (" + failed.Reason + ")"
		}
		// A CronJob's older failed runs stay until they're cleaned up;
		// only its newest run says how it's doing.
		if o := j.Metadata.controllerRef(); o != nil && o.Kind == "CronJob" &&
			b.latestRun[j.Metadata.Namespace+"/"+o.Name].After(j.Metadata.CreationTimestamp) {
			r.State, r.Word = report.StateStopped, r.Word+"; a later run has started"
		}
	case complete != nil && complete.Status == "True":
		r.State, r.Word = report.StateOK, "Succeeded"
		if st.CompletionTime != nil {
			r.Since = *st.CompletionTime
		}
	case j.Spec.Suspend != nil && *j.Spec.Suspend:
		r.State, r.Word = report.StateStopped, "Suspended"
	case st.Active > 0:
		r.State, r.Word = report.StateStarting, "Running"
	default:
		r.State, r.Word = report.StatePending, "Waiting to start"
	}
	return r
}

func (b *builder) cronJob(c *cronJob) report.Workload {
	r := b.rec(report.KindController, "cronjob/"+c.Metadata.Name, c.Metadata)
	r.State, r.Word = report.StateOK, "Scheduled ("+c.Spec.Schedule+")"
	if c.Spec.Suspend != nil && *c.Spec.Suspend {
		r.State, r.Word = report.StateStopped, "Suspended"
	}
	r.Facts = map[string]any{"schedule": c.Spec.Schedule}
	if t := c.Status.LastSuccessfulTime; t != nil {
		r.Facts["last_success"] = t.UTC().Format(time.RFC3339)
	}
	return r
}

func (b *builder) pvc(c *pvc) report.Workload {
	r := b.rec(report.KindPVC, c.Metadata.Name, c.Metadata)
	r.Facts = map[string]any{}
	if c.Spec.StorageClassName != nil {
		r.Facts["storage_class"] = *c.Spec.StorageClassName
	}
	if q := c.Status.Capacity["storage"]; q != "" {
		r.Facts["capacity"] = q
	}
	if c.Spec.VolumeName != "" {
		r.Facts["volume"] = c.Spec.VolumeName // its PersistentVolume, for the array volume behind it
	}
	switch c.Status.Phase {
	case "Bound":
		r.State, r.Word = report.StateOK, "Bound"
	case "Lost":
		r.State, r.Word = report.StateDown, "Lost (its volume is gone)"
	default:
		r.State, r.Word, r.Since = report.StatePending, "Pending (waiting for a volume)", c.Metadata.CreationTimestamp
	}
	if len(r.Facts) == 0 {
		r.Facts = nil
	}
	return r
}

// clusterRecord is the cluster's own record: whether the agent can read
// it, and its version.
func clusterRecord(name, version string, problems map[string]string, apiDown string) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, State: report.StateOK, Word: "API answering"}
	if version != "" {
		r.Facts = map[string]any{"version": version}
	}
	switch {
	case apiDown != "":
		r.State, r.Word = report.StateDown, "Can't reach its API: "+short(apiDown, 160)
	case len(problems) > 0:
		keys := slices.Sorted(maps.Keys(problems))
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+": "+short(problems[k], 80))
		}
		r.State, r.Word = report.StateDegraded, "Can't read "+strings.Join(parts, "; ")
	}
	return r
}
