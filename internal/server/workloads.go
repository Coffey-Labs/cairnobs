// SPDX-FileCopyrightText: 2026 Coffey Labs LLC
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// Workloads from cluster agents (docs/cluster-watcher.md): nodes, pods,
// controllers, jobs, volumes and VMs, as short records. Any copy takes a
// batch, writes it to the database and announces `workloads`; every copy
// keeps all of them in memory, read back from the database by what changed
// since it last looked. Without a database they're kept in memory only.

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	// maxWorkloads bounds one batch: a page of a full listing is 500
	// records, about half a megabyte.
	maxWorkloads = 16 << 20

	// clusterSilentAfter is how long a cluster agent may go without a
	// batch. It sends one at least every 30 s.
	clusterSilentAfter = 2 * time.Minute

	// workloadsOverlap is how far back each read reaches before the newest
	// change it has seen, for a transaction that took its time to commit.
	workloadsOverlap = 5 * time.Second
	// workloadsReload is how often a copy reads everything again, and how
	// long tombstones are kept: longer than that.
	workloadsReload = 10 * time.Minute
	workloadsPurge  = time.Hour

	// Pods may wait this long to be scheduled or start before it's a
	// problem: a rollout does that all the time.
	podPendingFor = 5 * time.Minute
	// A node may be Updating this long before it's stuck.
	nodeUpdateStall = 30 * time.Minute
)

type wrec struct {
	report.Workload
	gen     string // memory only: the listing it came in
	changed int64  // the database's clock, ms
}

type wsource struct {
	heard, listed time.Time
	generation    string
	agent         string
	recs          map[string]*wrec
	usageAt       time.Time               // the latest usage reading (usage.go)
	usage         map[string]report.Usage // by record id; replaced, never changed
}

type workloadStore struct {
	syncMu sync.Mutex // one read from the database at a time
	mu     sync.RWMutex
	src    map[string]*wsource
	upto   int64     // newest change read, on the database's clock
	full   time.Time // the last read of everything
}

func (ws *workloadStore) source(name string) *wsource {
	s := ws.src[name]
	if s == nil {
		s = &wsource{recs: map[string]*wrec{}}
		ws.src[name] = s
	}
	return s
}

var workloadKinds = map[string]bool{report.KindCluster: true, report.KindNode: true, report.KindPod: true,
	report.KindController: true, report.KindJob: true, report.KindPVC: true, report.KindVM: true,
	report.KindService: true, report.KindIngress: true, report.KindCert: true}

func checkWorkload(w report.Workload) error {
	switch {
	case !workloadKinds[w.Kind]:
		return fmt.Errorf("unknown kind %q", w.Kind)
	case w.ID == "" || len(w.ID) > 128:
		return fmt.Errorf("%s %q: an id of 1 to 128 characters", w.Kind, w.Name)
	case w.Name == "" || len(w.Name) > 300 || len(w.Namespace) > 300:
		return fmt.Errorf("%s %s: a name of 1 to 300 characters", w.Kind, w.ID)
	case stateSeverity(w.State) == "":
		return fmt.Errorf("%s %s: unknown state %q", w.Kind, w.Name, w.State)
	}
	return nil
}

func (srv *Server) handleWorkloads(w http.ResponseWriter, r *http.Request) {
	agent, ok := srv.agentFor(r)
	if !ok {
		http.Error(w, srv.agentRefusal(), http.StatusUnauthorized)
		return
	}
	// Any agent sends workloads for itself: a cluster agent its cluster's,
	// a host agent on a Proxmox node its Proxmox cluster's.
	var b report.Workloads
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWorkloads))
	if err == nil {
		err = json.Unmarshal(body, &b)
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if b.V != report.Version {
		http.Error(w, fmt.Sprintf("protocol version %d, want %d", b.V, report.Version), http.StatusBadRequest)
		return
	}
	switch b.Full {
	case "", report.FullBegin, report.FullPage, report.FullEnd:
	default:
		http.Error(w, "bad batch: full is begin, page, end or empty", http.StatusBadRequest)
		return
	}
	if b.Generation == "" {
		http.Error(w, "bad batch: no generation", http.StatusBadRequest)
		return
	}
	if b.Source != "" && !sourceName.MatchString(b.Source) {
		http.Error(w, "bad batch: a source is named as a host is (a-z, 0-9, dots and dashes)", http.StatusBadRequest)
		return
	}
	// What the batch is about: the agent's own cluster, or something it
	// reads beside its host, kept apart from anything else it reads.
	key := report.SourceKey(agent, b.Source)
	// A kind this server doesn't know comes from a newer agent: dropped,
	// so the rest still counts, rather than refusing the batch.
	b.Records = slices.DeleteFunc(b.Records, func(rec report.Workload) bool { return !workloadKinds[rec.Kind] })
	for _, rec := range b.Records {
		if err := checkWorkload(rec); err != nil {
			http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := srv.storeWorkloads(key, b, time.Now()); err != nil {
		http.Error(w, "can't store the batch", http.StatusServiceUnavailable)
		return
	}
	// A cluster agent's actions (docs/cert-rotation.md, step 3) come and
	// go with its batches, as a host agent's do with its reports.
	srv.actionsAnswer(w, agent, b.Actions)
	w.WriteHeader(http.StatusNoContent)
}

// storeWorkloads keeps a batch for source key, where every copy reads it:
// an agent's, or one obs-beacond made itself (vaultread.go).
func (srv *Server) storeWorkloads(key string, b report.Workloads, now time.Time) error {
	if srv.db == nil {
		srv.applyBatch(key, b, now)
		return nil
	}
	batch := state.WorkloadBatch{Source: key, Agent: b.Agent, Generation: b.Generation, Full: b.Full, Removed: b.Removed, Heard: now}
	for _, rec := range b.Records {
		j, _ := json.Marshal(rec)
		batch.Records = append(batch.Records, state.WorkloadRecord{ID: rec.ID, Kind: rec.Kind, Record: j})
	}
	if err := srv.db.PutWorkloads(batch); err != nil {
		log.Printf("workloads: can't store %s's batch: %v", key, err)
		return err
	}
	if len(b.Usage) > 0 && !b.UsageAt.IsZero() {
		// Usage is a nicety: the batch counts even when it can't be kept.
		j, _ := json.Marshal(b.Usage)
		if err := srv.db.PutWorkloadUsage(key, b.UsageAt, j); err != nil {
			log.Printf("workloads: can't store %s's usage: %v", key, err)
		}
	}
	srv.syncWorkloads(false)
	srv.publish(noticeWorkloads, key)
	return nil
}

// applyBatch is a batch, without a database.
func (srv *Server) applyBatch(agent string, b report.Workloads, now time.Time) {
	ws := &srv.wl
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s := ws.source(agent)
	s.heard, s.generation, s.agent = now, b.Generation, b.Agent
	for _, rec := range b.Records {
		s.recs[rec.ID] = &wrec{Workload: rec, gen: b.Generation, changed: now.UnixMilli()}
	}
	for _, id := range b.Removed {
		delete(s.recs, id)
	}
	if len(b.Usage) > 0 && b.UsageAt.After(s.usageAt) {
		s.usageAt, s.usage = b.UsageAt, usageByID(b.Usage)
	}
	if b.Full == report.FullEnd {
		s.listed = now
		for id, rec := range s.recs {
			if rec.gen != b.Generation {
				delete(s.recs, id)
			}
		}
	}
}

// syncWorkloads reads what changed in the database since this copy last
// looked, or everything when all is set, it has never read, or it's been
// workloadsReload.
func (srv *Server) syncWorkloads(all bool) {
	if srv.db == nil {
		return
	}
	ws := &srv.wl
	ws.syncMu.Lock()
	defer ws.syncMu.Unlock()
	defer srv.syncUsage() // after the records (deferred calls run last first)
	all = all || ws.full.IsZero() || time.Since(ws.full) > workloadsReload
	since := ws.upto - workloadsOverlap.Milliseconds()
	if all {
		since = -1
	}
	rows, err := srv.db.WorkloadsSince(since)
	if err != nil {
		log.Printf("workloads: %v", err)
		return
	}
	sources, err := srv.db.WorkloadSources()
	if err != nil {
		log.Printf("workloads: %v", err)
		return
	}
	// Parsed before taking the lock: the pages read the store meanwhile.
	parsed := make([]*wrec, len(rows))
	for i, row := range rows {
		if row.Gone {
			continue
		}
		var rec report.Workload
		if err := json.Unmarshal(row.Record, &rec); err != nil {
			log.Printf("workloads: %s %s: %v", row.Source, row.ID, err)
			continue
		}
		parsed[i] = &wrec{Workload: rec, changed: row.Changed}
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if all {
		ws.src = map[string]*wsource{}
		ws.full = time.Now()
	}
	for i, row := range rows {
		ws.upto = max(ws.upto, row.Changed)
		s := ws.source(row.Source)
		if cur := s.recs[row.ID]; cur != nil && cur.changed > row.Changed {
			continue
		}
		switch {
		case row.Gone:
			delete(s.recs, row.ID)
		case parsed[i] != nil:
			s.recs[row.ID] = parsed[i]
		}
	}
	kept := make(map[string]bool, len(sources))
	for _, src := range sources {
		kept[src.Source] = true
		s := ws.source(src.Source)
		s.heard, s.listed, s.generation, s.agent = src.Heard, src.Listed, src.Generation, src.Agent
	}
	for n := range ws.src {
		if !kept[n] {
			delete(ws.src, n) // forgotten, on this copy or another
		}
	}
}

// purgeWorkloads forgets old tombstones, every few minutes: on the evaluator.
func (srv *Server) purgeWorkloads(stop <-chan struct{}) {
	t := time.NewTicker(workloadsReload)
	defer t.Stop()
	for {
		if now, err := srv.db.DBNowMS(); err == nil {
			if _, err := srv.db.PurgeWorkloads(now - workloadsPurge.Milliseconds()); err != nil {
				log.Printf("workloads: purge: %v", err)
			}
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// --- judging ---

// stateSeverity is how a state shows: good, warning, critical, waiting or
// off ("" for a state that doesn't exist).
func stateSeverity(s string) string {
	switch s {
	case report.StateOK:
		return "good"
	case report.StateStopped:
		return "off"
	case report.StateStarting, report.StatePending, report.StateUpdating, report.StateUnknown:
		return "waiting"
	case report.StateDegraded, report.StateCrashloop:
		return "warning"
	case report.StateDown:
		return "critical"
	}
	return ""
}

// wsnap is one source's workloads, copied out of the store.
type wsnap struct {
	name          string
	heard, listed time.Time
	agent         string
	recs          []report.Workload
	usageAt       time.Time
	usage         map[string]report.Usage // shared with the store: read only
	host          bool                    // a host's libvirt VMs, not a cluster
	vmErr         string                  // the host couldn't read them
}

// workloadSnapshot copies every source's records, configured cluster
// agents that never sent anything included, and hosts' libvirt VMs, sorted
// by name.
func (srv *Server) workloadSnapshot() []wsnap {
	out := append(srv.clusterSnaps(), srv.hostVMSnaps()...)
	if e := srv.currentExpected(); e != nil {
		for i := range out {
			if want := e.Clusters[out[i].name].Subsystems; len(want) > 0 && out[i].distribution() == ibmiDistribution {
				out[i].recs = expectSubsystems(out[i].recs, want)
			}
		}
	}
	slices.SortFunc(out, func(a, b wsnap) int { return strings.Compare(a.name, b.name) })
	return out
}

func (srv *Server) clusterSnaps() []wsnap {
	ws := &srv.wl
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	names := srv.clusterAgents()
	for n := range ws.src {
		if !slices.Contains(names, n) && (srv.knownAgent(sourceAgent(n)) || srv.serverReads(n)) {
			names = append(names, n) // what a host agent reads (Proxmox, a vCenter), or a cluster agent's
		}
	}
	out := make([]wsnap, 0, len(names))
	for _, n := range names {
		sn := wsnap{name: n}
		if s := ws.src[n]; s != nil {
			sn.heard, sn.listed, sn.agent = s.heard, s.listed, s.agent
			sn.usageAt, sn.usage = s.usageAt, s.usage
			sn.recs = make([]report.Workload, 0, len(s.recs))
			for _, r := range s.recs {
				sn.recs = append(sn.recs, r.Workload)
			}
		}
		out = append(out, sn)
	}
	return out
}

// title is the cluster's own name: its record's, which is the source's
// for a cluster agent, and the Proxmox cluster's ("prod-pve (host1)") for
// a Proxmox node's agent.
func (s wsnap) title() string {
	for _, r := range s.recs {
		if r.Kind == report.KindCluster && r.ID == report.ClusterSelf && r.Name != "" && r.Name != s.name {
			return r.Name + " (" + sourceAgent(s.name) + ")"
		}
	}
	return s.name
}

// aboutWorkloads says whether a condition is about a source's workloads,
// rather than about the host of the same name (a Proxmox node's agent is
// both).
func aboutWorkloads(key, src string) bool {
	kind, rest, _ := strings.Cut(key, "/")
	switch kind {
	case "cert", "cluster", "job", "node", "pod", "pvc", "storage", "vm", "workload":
		return strings.HasPrefix(rest, src+"/")
	}
	return false
}

// hostVMSnaps are the hosts reporting libvirt VMs, as sources of their
// own: their records come with each report, which every copy has.
func (srv *Server) hostVMSnaps() []wsnap {
	var out []wsnap
	srv.wl.mu.RLock()
	batches := map[string]bool{}
	for n := range srv.wl.src {
		batches[n] = true
	}
	srv.wl.mu.RUnlock()
	for _, h := range srv.store.HostVMs() {
		if srv.clusterAgent(h.name) || batches[h.name] {
			continue // its batches are the source by that name
		}
		out = append(out, wsnap{name: h.name, heard: h.seen, listed: h.seen, agent: h.agent, recs: h.vms.List,
			usageAt: h.at, usage: usageByID(h.vms.Usage), host: true, vmErr: h.vms.Error})
	}
	return out
}

// sourceStatus says whether a cluster's records are current.
func (srv *Server) sourceStatus(s wsnap, now time.Time) (status, word string, silent bool) {
	if s.host {
		// The host's own condition says it's silent; its VMs are as last seen.
		if now.Sub(s.heard) > srv.cfg.StaleAfter {
			return "critical", "Its host is silent", true
		}
		return "good", "Current", false
	}
	switch {
	case s.heard.IsZero() && now.Sub(srv.started) <= srv.cfg.StaleAfter:
		return "waiting", "Waiting for its agent", false
	case s.heard.IsZero():
		return "critical", "Its agent has never reported", true
	case now.Sub(s.heard) > clusterSilentAfter && clusterUpdating(s.recs) && now.Sub(s.heard) <= nodeUpdateStall:
		// A single-node cluster's agent goes with its node while the
		// update reboots it (docs/cluster-watcher.md, Updates aren't outages).
		return "waiting", "Its agent is silent while the cluster updates", true
	case now.Sub(s.heard) > clusterSilentAfter && sourceAgent(s.name) == serverSource:
		return "critical", "obs-beacond hasn't read it lately", true
	case now.Sub(s.heard) > clusterSilentAfter:
		return "critical", "Its agent is silent", true
	case s.listed.IsZero():
		return "waiting", "Listing everything", false
	}
	for _, r := range s.recs {
		if r.Kind == report.KindCluster && r.ID == report.ClusterSelf && r.State == report.StateDown {
			return "critical", r.Word, true
		}
	}
	return "good", "Current", false
}

// clusterUpdating says whether an OpenShift or OKD update is under way:
// the cluster version or a machine config pool updating.
func clusterUpdating(recs []report.Workload) bool {
	for _, r := range recs {
		if r.Kind == report.KindCluster && r.State == report.StateUpdating &&
			(strings.HasPrefix(r.Name, "clusterversion/") || strings.HasPrefix(r.Name, "machineconfigpool/")) {
			return true
		}
	}
	return false
}

// isOpenShift says whether the cluster's agent sends OpenShift's parts.
func isOpenShift(recs []report.Workload) bool {
	for _, r := range recs {
		if r.Kind == report.KindCluster && strings.HasPrefix(r.Name, "clusteroperator/") {
			return true
		}
	}
	return false
}

// platformNamespace is where OpenShift runs itself: judged through its
// ClusterOperators, not pod by pod, unless adopted or annotated.
func platformNamespace(ns string) bool {
	return strings.HasPrefix(ns, "openshift-") || ns == "openshift" || strings.HasPrefix(ns, "kube-")
}

// partTitle names one of a cluster's parts for people.
func partTitle(name string) string {
	kind, n, _ := strings.Cut(name, "/")
	switch kind {
	case "clusterversion":
		return "The cluster version"
	case "clusteroperator":
		return "Operator " + n
	case "machineconfigpool":
		return "Machine config pool " + n
	case "pod": // a FlashArray's (docs/pure.md)
		return "Array pod " + n
	case "connection":
		return "Replication to " + n
	case "token": // Vault's (docs/vault.md)
		return "Its " + n + " token"
	case "pki":
		return "PKI mount " + n
	case "audit":
		return "Its audit log"
	case "replication": // Vault Enterprise's
		if n == "dr" {
			return "DR replication"
		}
		return upperFirst(n) + " replication"
	case "license":
		return "Its license"
	case "subsystem": // an IBM i partition's (docs/ibmi.md)
		return "Subsystem " + n
	case "interface":
		return "Interface " + n
	case "inquiry":
		return "Unanswered inquiry " + n
	case "message":
		return "QSYSOPR message " + n
	case "jobqueue":
		return "Job queue " + n
	case "asp":
		switch {
		case n == "1":
			return "The system ASP"
		case strings.Trim(n, "0123456789") == "":
			return "User ASP " + n
		}
		return "Independent ASP " + n
	}
	return name
}

// noun is what the source is, for people: most are clusters; a vCenter,
// Prism Central or an array is said as such.
func (s wsnap) noun() string {
	switch d := s.distribution(); {
	case strings.HasPrefix(d, "Vault"), d == "OpenBao":
		return strings.TrimSuffix(d, " Enterprise")
	case d == "vCenter", d == "Prism Central", d == hmcDistribution:
		return d
	case strings.HasPrefix(d, "Purity"):
		return "Array"
	case d == ibmiDistribution:
		return "Partition"
	}
	return "Cluster"
}

// nodeNoun is what its nodes are: a FlashArray's are controllers, a
// FlashBlade's blades; a vCenter's and Prism Central's, hosts; an HMC's,
// Power servers.
func (s wsnap) nodeNoun() string {
	switch d := s.distribution(); {
	case d == "vCenter", d == "Prism Central":
		return "Host"
	case d == hmcDistribution:
		return "Server"
	case d == "Purity//FB":
		return "Blade"
	case strings.HasPrefix(d, "Vault"), d == "OpenBao":
		return "Server"
	case strings.HasPrefix(d, "Purity"):
		return "Controller"
	}
	return "Node"
}

func (s wsnap) distribution() string {
	for _, r := range s.recs {
		if r.Kind == report.KindCluster && r.ID == report.ClusterSelf {
			d, _ := r.Facts["distribution"].(string)
			return d
		}
	}
	return ""
}

// alertable applies a workload's alert setting: off drops it, and only
// page (or, with no setting, a cluster, a node or a certificate) keeps
// its severity;
// everything else is a warning on the pages, which doesn't page.
func alertable(c Condition, alert, kind string) (Condition, bool) {
	switch {
	case alert == report.AlertOff:
		return c, false
	case alert == report.AlertPage, alert == "" && (kind == report.KindCluster || kind == report.KindNode || kind == report.KindCert):
	default:
		c.Severity = "warning"
	}
	return c, true
}

func workloadHref(source, ns, q string) string {
	p := "/workloads?source=" + url.QueryEscape(source)
	if ns != "" {
		p += "&ns=" + url.QueryEscape(ns)
	}
	if q != "" {
		p += "&q=" + url.QueryEscape(q) // a Proxmox guest's "100 (db)"
	}
	return p
}

// where names a workload for people: "web in shop".
func where(name, ns string) string {
	if ns == "" {
		return name
	}
	return name + " in " + ns
}

// workloadConditions judges every cluster's workloads (docs/cluster-
// watcher.md, Judging and alerting): pods as their controller, everything
// on a node that's down under the node, a cluster whose agent is silent as
// one condition.
//
// A condition that has already fired is never folded in: it stays as it is,
// as with hosts (groupConditions), so nobody is told it's resolved when it
// isn't, and told again when the node is back.
func (srv *Server) workloadConditions(now time.Time) []Condition {
	var conds []Condition
	e := srv.currentExpected()
	snaps := srv.workloadSnapshot()
	for _, s := range snaps {
		conds = append(conds, srv.judgeSource(s, now, srv.alerts.firing, adoptionFor(e, s.name))...)
	}
	byPVC, byVolume := volumeLinks(snaps)
	linkConditions(conds, byPVC, byVolume)
	return conds
}

// heldKey is the condition a record on a node that's down would be judged
// under: its controller's for a pod that has one.
func heldKey(src string, r report.Workload) string {
	switch {
	case r.Kind == report.KindPod && r.Owner != "":
		return "workload/" + src + "/" + r.Namespace + "/" + r.Owner
	case r.Kind == report.KindPod:
		return "pod/" + src + "/" + r.Namespace + "/" + r.Name
	case r.Kind == report.KindVM:
		return "vm/" + src + "/" + r.Namespace + "/" + r.Name
	}
	return ""
}

// judgeSource judges one cluster. A cluster whose agent is silent is one
// condition, with only what had already fired beside it.
func (srv *Server) judgeSource(s wsnap, now time.Time, firing func(key string) bool, ad adoption) []Condition {
	_, word, silent := srv.sourceStatus(s, now)
	if s.host {
		return judgeHostVMs(s, silent, now, firing, ad)
	}
	if !silent {
		return judgeRecords(s, now, firing, ad)
	}
	sev := "critical"
	if clusterUpdating(s.recs) && now.Sub(s.heard) <= nodeUpdateStall {
		sev = "warning"
	}
	conds := []Condition{{Key: "cluster/" + s.name + "/silent", Severity: sev, Title: s.noun() + " " + s.title() + ": " + lowerFirst(word),
		Detail:   fmt.Sprintf("Nothing it shows is current: %s are as last seen.", plural(len(s.recs), "workload", "workloads")),
		Affected: plural(len(s.recs), "workload", "workloads"), Host: s.name, Href: "/clusters"}}
	for _, c := range judgeRecords(s, now, firing, ad) {
		if firing(c.Key) {
			conds = append(conds, c)
		}
	}
	return conds
}

// judgeHostVMs judges a host's libvirt VMs. When the host is silent its
// own condition says so, and only what had already fired stays beside it.
func judgeHostVMs(s wsnap, silent bool, now time.Time, firing func(key string) bool, ad adoption) []Condition {
	var conds []Condition
	for _, c := range judgeRecords(s, now, firing, ad) {
		if !silent || firing(c.Key) {
			conds = append(conds, c)
		}
	}
	if s.vmErr != "" && !silent {
		conds = append(conds, Condition{Key: "vms/" + s.name, Severity: "warning", Title: s.name + ": can't read its VMs",
			Detail: s.vmErr + ". libvirt is running but its read-only socket didn't answer the helper.", Host: s.name, Href: "/hosts/" + s.name})
	}
	return conds
}

func judgeRecords(s wsnap, now time.Time, firing func(key string) bool, ad adoption) []Condition {
	var conds []Condition
	name := s.title() // for people: "vcsa-01 (mgmt1)"
	src := s.name
	present := map[string]bool{} // adoption keys of what's there, for what's missing
	updating := clusterUpdating(s.recs)
	ad.platform = isOpenShift(s.recs)
	add := func(c Condition, alert, kind string) {
		c.Host = src // so a maintenance window naming the cluster holds it
		if c.Href == "" {
			c.Href = "/clusters"
		}
		if c, ok := alertable(c, alert, kind); ok {
			conds = append(conds, c)
		}
	}
	downNodes := map[string]bool{}
	onNode := map[string][2]int{} // pods, VMs
	for _, r := range s.recs {
		if r.Kind == report.KindNode && r.State == report.StateDown {
			downNodes[r.Name] = true
		}
	}
	type group struct {
		ns, owner string
		pods, bad int
		onDown    int // pods on a node that's down: the node explains them
		worst     string
		words     map[string]int
		alert     string
		ctrl      *report.Workload
	}
	groups := map[string]*group{}
	grp := func(ns, owner string) *group {
		k := ns + "/" + owner
		g := groups[k]
		if g == nil {
			g = &group{ns: ns, owner: owner, words: map[string]int{}}
			groups[k] = g
		}
		return g
	}
	type pvcBad struct {
		r     report.Workload
		class string
	}
	var badPVCs []pvcBad
	for _, r := range s.recs {
		if r.Node != "" && downNodes[r.Node] && r.Kind != report.KindNode && !firing(heldKey(src, r)) {
			n := onNode[r.Node]
			switch r.Kind {
			case report.KindPod:
				n[0]++
			case report.KindVM:
				n[1]++
			}
			onNode[r.Node] = n
			if r.Kind == report.KindPod && r.Owner != "" {
				grp(r.Namespace, r.Owner).onDown++
			}
			present[recKey(r)] = true
			continue
		}
		present[recKey(r)] = true
		long := !r.Since.IsZero() && now.Sub(r.Since) > podPendingFor
		switch r.Kind {
		case report.KindCluster:
			if r.State != report.StateDegraded && r.State != report.StateDown {
				break
			}
			// What obs-beacond reads itself (Vault) warns when degraded: Raft
			// short of tolerance, a token weeks from its end, a mount it
			// can't list. A cluster agent's pages.
			sev := "critical"
			if r.State == report.StateDegraded && serverSourced(src) {
				sev = "warning"
			}
			if r.ID == report.ClusterSelf {
				add(Condition{Key: "cluster/" + src + "/" + r.ID, Severity: sev, Title: fmt.Sprintf("%s %s: %s", s.noun(), name, lowerFirst(r.Word)),
					Detail: r.Name}, r.Alert, r.Kind)
				break
			}
			// One of OpenShift's parts. Operators come and go during an
			// update, so they warn then; a failing update itself doesn't.
			detail := ""
			if updating && strings.HasPrefix(r.Name, "clusteroperator/") {
				sev, detail = "warning", "The cluster is updating, and operators often say so for a while. It pages if the update fails."
			}
			add(Condition{Key: "cluster/" + src + "/" + r.Name, Severity: sev, Title: fmt.Sprintf("%s in %s: %s", partTitle(r.Name), name, lowerFirst(r.Word)),
				Detail: detail}, r.Alert, r.Kind)
		case report.KindNode:
			key := "node/" + src + "/" + r.Name
			switch {
			case r.State == report.StateDown: // below, with what it takes down
			case r.State == report.StateUpdating && !r.Since.IsZero() && now.Sub(r.Since) > nodeUpdateStall:
				add(Condition{Key: key, Severity: "warning", Title: fmt.Sprintf("%s %s in %s has been updating for %s", s.nodeNoun(), r.Name, name, human(now.Sub(r.Since))),
					Detail: "It's cordoned for an update that hasn't finished.", Href: workloadHref(src, "", r.Name)}, r.Alert, r.Kind)
			case r.State == report.StateDegraded, r.State == report.StateUnknown, r.State == report.StatePending && long:
				add(Condition{Key: key, Severity: "warning", Title: fmt.Sprintf("%s %s in %s: %s", s.nodeNoun(), r.Name, name, lowerFirst(r.Word)),
					Href: workloadHref(src, "", r.Name)}, r.Alert, r.Kind)
			}
		case report.KindController:
			g := grp(r.Namespace, r.Name)
			rc := r
			g.ctrl, g.alert = &rc, r.Alert
		case report.KindPod:
			if r.Owner == "" {
				if bad, word := podBad(r, long); bad {
					add(Condition{Key: "pod/" + src + "/" + r.Namespace + "/" + r.Name, Severity: "warning",
						Title: fmt.Sprintf("Pod %s (%s): %s", where(r.Name, r.Namespace), name, lowerFirst(word)), Href: workloadHref(src, r.Namespace, r.Name)}, ad.alert(r, r.Alert), r.Kind)
				}
				continue
			}
			g := grp(r.Namespace, r.Owner)
			g.pods++
			if g.alert == "" {
				g.alert = r.Alert
			}
			if bad, word := podBad(r, long); bad {
				g.bad++
				g.words[word]++
				if r.State == report.StateCrashloop || r.State == report.StateDown {
					g.worst = "critical"
				}
			}
		case report.KindJob:
			if r.State == report.StateDown {
				add(Condition{Key: "job/" + src + "/" + r.Namespace + "/" + r.Name, Severity: "warning",
					Title: fmt.Sprintf("Job %s (%s): %s", where(r.Name, r.Namespace), name, lowerFirst(r.Word)), Href: workloadHref(src, r.Namespace, r.Name)}, ad.alert(r, r.Alert), r.Kind)
			}
		case report.KindPVC:
			if r.State == report.StateDown || r.State == report.StateDegraded || (r.State == report.StatePending && long) {
				class, _ := r.Facts["storage_class"].(string)
				badPVCs = append(badPVCs, pvcBad{r, class})
			}
		case report.KindCert:
			sev := ""
			switch r.State {
			case report.StateDown:
				sev = "critical"
			case report.StateDegraded:
				sev = "warning"
			}
			if sev != "" {
				what := "Certificate"
				if ca, _ := r.Facts["ca"].(bool); ca {
					what = "CA"
				}
				from := "" // a Vault's mount, a cluster's namespace
				if r.Namespace != "" {
					from = " from " + r.Namespace
				}
				// A cluster's (cert-inventory.md, step 4): which
				// cert-manager Certificate, and where it's served.
				var detail []string
				if c, _ := r.Facts["certificate"].(string); c != "" {
					detail = append(detail, "cert-manager Certificate "+r.Namespace+"/"+c+".")
				}
				if on, _ := r.Facts["served_on"].(string); on != "" {
					detail = append(detail, "Served on "+on+".")
				}
				add(Condition{Key: "cert/" + src + "/" + r.ID, Severity: sev, Detail: strings.Join(detail, " "),
					Title: fmt.Sprintf("%s %s%s in %s: %s", what, r.Name, from, name, lowerFirst(r.Word)), Href: workloadHref(src, r.Namespace, r.Name)}, ad.alert(r, r.Alert), r.Kind)
			}
		case report.KindVM:
			sev, detail := "", ""
			if w, ok := ad.wl[recKey(r)]; ok && !w.Stopped && r.State == report.StateStopped {
				sev, detail = "critical", "It's adopted as running, on Configure, Clusters."
			}
			switch r.State {
			case report.StateDown, report.StateCrashloop:
				sev = "critical"
			case report.StateDegraded:
				sev = "warning"
			case report.StatePending, report.StateStarting:
				if long {
					sev = "warning"
				}
			}
			if sev != "" {
				add(Condition{Key: "vm/" + src + "/" + r.Namespace + "/" + r.Name, Severity: sev, Detail: detail,
					Title: fmt.Sprintf("VM %s (%s): %s", where(r.Name, r.Namespace), name, lowerFirst(r.Word)), Href: workloadHref(src, r.Namespace, r.Name)}, ad.alert(r, r.Alert), r.Kind)
			}
			for _, c := range vmExtras(src, r, now) {
				add(c, ad.alert(r, r.Alert), r.Kind)
			}
		}
	}
	// Nodes that are down, with what's on them.
	for _, r := range s.recs {
		if r.Kind != report.KindNode || !downNodes[r.Name] {
			continue
		}
		n := onNode[r.Name]
		var on []string
		if n[0] > 0 {
			on = append(on, plural(n[0], "pod", "pods"))
		}
		if n[1] > 0 {
			on = append(on, plural(n[1], "VM", "VMs"))
		}
		detail := r.Word + "."
		if len(on) > 0 {
			detail += " On it: " + strings.Join(on, " and ") + "."
		}
		add(Condition{Key: "node/" + src + "/" + r.Name, Severity: "critical", Title: fmt.Sprintf("%s %s in %s is down", s.nodeNoun(), r.Name, name),
			Detail: detail, Affected: strings.Join(on, " and "), Href: workloadHref(src, "", r.Name)}, r.Alert, r.Kind)
	}
	// Controllers, with their pods.
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		g := groups[k]
		if g.onDown > 0 && g.bad == 0 {
			continue // short of pods because a node is down: under the node
		}
		sev, word := "", ""
		// Short of ready pods counts after 5 minutes, as a rollout or a
		// rescheduled pod catches up well before that.
		if c := g.ctrl; c != nil && (c.Since.IsZero() || now.Sub(c.Since) > podPendingFor) {
			switch c.State {
			case report.StateDown:
				sev, word = "critical", c.Word
			case report.StateDegraded:
				sev, word = "warning", c.Word
			}
		}
		if g.bad > 0 {
			sev = cmpSeverity(sev, "warning")
			if g.worst == "critical" && sev != "critical" && g.bad == g.pods {
				sev = "critical"
			}
			ws := make([]string, 0, len(g.words))
			for w, n := range g.words {
				ws = append(ws, fmt.Sprintf("%d %s", n, lowerFirst(w)))
			}
			slices.Sort(ws)
			if word != "" {
				word += "; "
			}
			word += fmt.Sprintf("%d of %d pods: ", g.bad, g.pods) + strings.Join(ws, ", ")
		}
		if sev == "" {
			continue
		}
		add(Condition{Key: "workload/" + src + "/" + g.ns + "/" + g.owner, Severity: sev,
			Title: fmt.Sprintf("%s (%s): %s", where(g.owner, g.ns), name, word), Href: workloadHref(src, g.ns, ownerName(g.owner))},
			ad.alertKey(g.ns, g.ns+"/"+g.owner, g.alert), report.KindController)
	}
	// Volumes: several failing in one storage class is one problem.
	byClass := map[string][]pvcBad{}
	for _, p := range badPVCs {
		byClass[p.class] = append(byClass[p.class], p)
	}
	for class, ps := range byClass {
		if len(ps) >= 3 && class != "" {
			names := make([]string, 0, len(ps))
			sev, alert := "warning", report.AlertOff
			for _, p := range ps {
				names = append(names, where(p.r.Name, p.r.Namespace))
				if p.r.State == report.StateDown {
					sev = "critical"
				}
				alert = louder(alert, ad.alert(p.r, p.r.Alert))
			}
			slices.Sort(names)
			add(Condition{Key: "storage/" + src + "/" + class, Severity: sev, Title: fmt.Sprintf("%d volumes in %s won't bind (storage class %s)", len(ps), name, class),
				Detail: "The storage class's provisioner may be failing.", Affected: strings.Join(names, ", "), Href: workloadHref(src, "", "")}, alert, report.KindPVC)
			continue
		}
		for _, p := range ps {
			sev := "warning"
			if p.r.State == report.StateDown {
				sev = "critical"
			}
			add(Condition{Key: "pvc/" + src + "/" + p.r.Namespace + "/" + p.r.Name, Severity: sev,
				Title: fmt.Sprintf("Volume %s (%s): %s", where(p.r.Name, p.r.Namespace), name, lowerFirst(p.r.Word)), Href: workloadHref(src, p.r.Namespace, p.r.Name)}, ad.alert(p.r, p.r.Alert), report.KindPVC)
		}
	}
	// Adopted workloads that aren't there: only once a full listing is in,
	// or everything would be missing until it is.
	if !s.listed.IsZero() {
		for _, w := range ad.list {
			if present[w.Name] {
				continue
			}
			ns, kn, _ := strings.Cut(w.Name, "/")
			key := "workload/" + src + "/" + w.Name // as the controller's own condition
			if vm, ok := strings.CutPrefix(kn, "vm/"); ok {
				key = "vm/" + src + "/" + ns + "/" + vm
			}
			add(Condition{Key: key, Severity: "critical", Title: fmt.Sprintf("%s (%s) is missing", where(kn, ns), name),
				Detail: "It's adopted on Configure, Clusters, but the cluster doesn't have it. If it's gone on purpose, take it off there.",
				Href:   "/configure/clusters/" + src}, ad.alertKey(ns, w.Name, ""), report.KindController)
		}
	}
	slices.SortFunc(conds, func(a, b Condition) int { return strings.Compare(a.Key, b.Key) })
	return conds
}

// podBad says whether a pod is a problem, and in a word what kind.
func podBad(r report.Workload, long bool) (bool, string) {
	switch r.State {
	case report.StateCrashloop, report.StateDown, report.StateDegraded:
		return true, r.Word
	case report.StatePending, report.StateStarting, report.StateUnknown:
		return long, r.Word
	}
	return false, ""
}

// ownerName is the name in "deployment/web".
func ownerName(owner string) string {
	_, n, ok := strings.Cut(owner, "/")
	if !ok {
		return owner
	}
	return n
}

func cmpSeverity(a, b string) string {
	if a == "critical" || b == "critical" {
		return "critical"
	}
	if a == "warning" || b == "warning" {
		return "warning"
	}
	return a
}

// --- the pages ---

// ClusterView is one cluster as the dashboard and the Clusters page show it.
type ClusterView struct {
	Name       string                    `json:"name"`
	Status     string                    `json:"status"`
	Word       string                    `json:"word"`
	Heard      *time.Time                `json:"heard"`
	Listed     *time.Time                `json:"listed"`
	Agent      string                    `json:"agent,omitempty"`
	Version    string                    `json:"version,omitempty"`
	Nodes      int                       `json:"nodes"`
	NodesReady int                       `json:"nodes_ready"`
	Workloads  int                       `json:"workloads"` // pods, controllers, jobs, volumes and VMs
	Problems   int                       `json:"problems"`  // conditions open on it now
	Kinds      map[string]map[string]int `json:"kinds"`     // kind → severity → count
	// Title is the cluster's own name when it isn't the source's: a
	// Proxmox cluster read by one of its nodes' agents.
	Title string `json:"title,omitempty"`
	// ReadBy is the agent that reads it, when that isn't its name.
	ReadBy string `json:"read_by,omitempty"`
	// OpenShift and OKD: the version and the machine config pools, and the
	// operators that aren't fine (the rest are only counted).
	Parts       []ClusterPart `json:"parts,omitempty"`
	Operators   int           `json:"operators,omitempty"`
	OperatorsOK int           `json:"operators_ok,omitempty"`
	// Usage is the cluster's own: an array's performance and space now
	// (docs/pure.md, step 4). History is its name in workload hours.
	Usage   *UsageNow `json:"usage,omitempty"`
	History string    `json:"history,omitempty"`
	// Noun is what it is ("Cluster", "Array", "vCenter"), NodeNoun what
	// its nodes are ("Node", "Controller", "Host").
	Noun     string `json:"noun"`
	NodeNoun string `json:"node_noun"`
	// Partition is an IBM i partition's own numbers (docs/ibmi.md).
	Partition *PartitionView `json:"partition,omitempty"`
}

// expectSubsystems marks an IBM i partition's subsystems that
// expected.yaml says must be active (docs/ibmi.md, step 2): one that isn't
// is down, and one that isn't there at all is added, down. recs is the
// snapshot's own copy; a record's facts are copied before they change.
func expectSubsystems(recs []report.Workload, want []string) []report.Workload {
	for _, w := range want {
		lib, name, ok := strings.Cut(strings.ToUpper(w), "/")
		if !ok {
			lib, name = "", lib
		}
		found := false
		for i, r := range recs {
			if r.Name != "subsystem/"+name || (lib != "" && r.Facts["library"] != lib) {
				continue
			}
			found = true
			f := maps.Clone(r.Facts)
			if f == nil {
				f = map[string]any{}
			}
			f["expected"] = true
			r.Facts = f
			if r.State != report.StateOK && r.State != report.StateStarting {
				r.State, r.Word = report.StateDown, r.Word+", and expected.yaml says it should be active"
			}
			recs[i] = r
		}
		if !found {
			recs = append(recs, report.Workload{Kind: report.KindCluster, ID: "subsystem/expected/" + strings.ToUpper(w), Name: "subsystem/" + name,
				State: report.StateDown, Word: "Not on the partition, or its profile may not use the subsystem's description (*USE)",
				Facts: map[string]any{"expected": true}})
		}
	}
	return recs
}

// ibmiDistribution is what an IBM i partition's own record says it is
// (ibmi.Distribution, said again here to keep the reader out of obs-beacond).
const ibmiDistribution = "IBM i"

// hmcDistribution is what an HMC's own record says it is (hmc.Distribution).
const hmcDistribution = "HMC"

// PowerServer is the Power server an IBM i partition runs on, as an HMC
// lists it (docs/ibmi.md, step 4): found by the server's serial number and
// the partition's number, which both the partition and the HMC say.
type PowerServer struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Word      string `json:"word"`
	HMC       string `json:"hmc"`       // the HMC's source
	Partition string `json:"partition"` // what the HMC calls the partition
	Href      string `json:"href"`      // the partition on Workloads
}

// powerIndex is every partition the HMCs list, by "serial/number".
func powerIndex(snaps []wsnap) map[string]PowerServer {
	out := map[string]PowerServer{}
	dup := map[string]bool{}
	for _, s := range snaps {
		if s.host || s.distribution() != hmcDistribution {
			continue
		}
		servers := map[string]report.Workload{}
		for _, r := range s.recs {
			if r.Kind == report.KindNode {
				servers[r.Name] = r
			}
		}
		for _, r := range s.recs {
			serial, _ := r.Facts["server_serial"].(string)
			id := factInt(r.Facts["partition_id"])
			if r.Kind != report.KindVM || serial == "" || id <= 0 {
				continue
			}
			k := fmt.Sprintf("%s/%d", strings.ToUpper(serial), id)
			if _, ok := out[k]; ok {
				dup[k] = true // two HMCs of a pair, both read: either will do
				continue
			}
			sv := servers[r.Node]
			out[k] = PowerServer{Name: r.Node, State: sv.State, Word: sv.Word, HMC: s.name, Partition: r.Name, Href: workloadHref(s.name, "", r.Name)}
		}
	}
	return out
}

// factInt is a number fact, as an agent's JSON or a reader's own record has it.
func factInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// PartitionView is what the Clusters page shows of an IBM i partition in
// place of nodes: its CPU over the last minute, its system ASP and jobs.
type PartitionView struct {
	CPUPct        *float64 `json:"cpu_pct,omitempty"`
	SystemASPPct  float64  `json:"system_asp_pct"`
	ActiveJobs    int      `json:"active_jobs"`
	Jobs          int      `json:"jobs"`
	Model         string   `json:"model,omitempty"`
	ProcessingCap float64  `json:"processing_units,omitempty"`
	// Subsystems and interfaces, counted: the ones that aren't fine and
	// the expected subsystems are its parts, the rest only counted.
	Subsystems       int `json:"subsystems"`
	SubsystemsActive int `json:"subsystems_active"`
	Interfaces       int `json:"interfaces"`
	InterfacesActive int `json:"interfaces_active"`
	// Server is the Power server it runs on, when an HMC lists it.
	Server *PowerServer `json:"server,omitempty"`
}

// partitionView reads a partition's own record's facts, and counts its
// subsystems and interfaces.
func partitionView(r report.Workload, recs []report.Workload, power map[string]PowerServer) *PartitionView {
	f := func(k string) float64 {
		switch v := r.Facts[k].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		}
		return 0
	}
	p := &PartitionView{SystemASPPct: f("system_asp_used_pct"), ActiveJobs: int(f("active_jobs")), Jobs: int(f("jobs")), ProcessingCap: f("processing_units")}
	if _, ok := r.Facts["cpu_pct"]; ok {
		c := f("cpu_pct")
		p.CPUPct = &c
	}
	p.Model, _ = r.Facts["model"].(string)
	if serial, _ := r.Facts["serial"].(string); serial != "" {
		if ps, ok := power[fmt.Sprintf("%s/%d", strings.ToUpper(serial), factInt(r.Facts["partition_id"]))]; ok {
			p.Server = &ps
		}
	}
	for _, x := range recs {
		up := x.State == report.StateOK
		switch {
		case strings.HasPrefix(x.Name, "subsystem/") && !strings.HasPrefix(x.ID, "subsystem/expected/"):
			p.Subsystems++
			if up {
				p.SubsystemsActive++
			}
		case strings.HasPrefix(x.Name, "interface/"):
			p.Interfaces++
			if up {
				p.InterfacesActive++
			}
		}
	}
	return p
}

// ClusterPart is one of a cluster's own parts as the Clusters page shows it.
type ClusterPart struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	State    string `json:"state"`
	Word     string `json:"word"`
	Severity string `json:"severity"`
}

func (srv *Server) clusterViews(now time.Time, conds []Condition) []ClusterView {
	probs := map[string]int{}
	snaps := srv.clusterSnaps()
	sources := map[string]bool{}
	for _, s := range snaps {
		sources[s.name] = true
	}
	for _, c := range conds {
		if c.Host != "" && sources[c.Host] && aboutWorkloads(c.Key, c.Host) {
			probs[c.Host]++
		}
	}
	out := []ClusterView{}
	all := srv.workloadSnapshot()
	power := powerIndex(all)
	for _, s := range all {
		if s.host {
			continue // a host's VMs: on its page and Workloads, not Clusters
		}
		v := ClusterView{Name: s.name, Agent: s.agent, Problems: probs[s.name], Kinds: map[string]map[string]int{}, Noun: s.noun(), NodeNoun: s.nodeNoun()}
		v.Status, v.Word, _ = srv.sourceStatus(s, now)
		if !s.heard.IsZero() {
			h := s.heard
			v.Heard = &h
		}
		if !s.listed.IsZero() {
			l := s.listed
			v.Listed = &l
		}
		for _, r := range s.recs {
			sev := stateSeverity(r.State)
			if v.Kinds[r.Kind] == nil {
				v.Kinds[r.Kind] = map[string]int{}
			}
			v.Kinds[r.Kind][sev]++
			switch r.Kind {
			case report.KindCluster:
				if r.ID != report.ClusterSelf {
					op := strings.HasPrefix(r.Name, "clusteroperator/")
					if op {
						v.Operators++
						if r.State == report.StateOK {
							v.OperatorsOK++
						}
					}
					// An IBM i partition's subsystems and interfaces are many:
					// only the ones that need a look, and the expected
					// subsystems, are listed.
					many := strings.HasPrefix(r.Name, "subsystem/") || strings.HasPrefix(r.Name, "interface/")
					quiet := many && (sev == "good" || sev == "off") && r.Facts["expected"] != true
					if (!op || r.State != report.StateOK) && !quiet {
						v.Parts = append(v.Parts, ClusterPart{Name: r.Name, Title: partTitle(r.Name), State: r.State, Word: r.Word, Severity: sev})
					}
				}
				if ver, ok := r.Facts["version"].(string); ok && strings.HasPrefix(r.Name, "clusterversion/") {
					v.Version = fmt.Sprint(r.Facts["distribution"], " ", ver) // OpenShift's version over Kubernetes'
				} else if ok && v.Version == "" && r.ID == report.ClusterSelf {
					v.Version = ver
					if d, ok := r.Facts["distribution"].(string); ok {
						v.Version = d + " " + ver // Proxmox VE 8.4.1
					}
				}
				if r.ID == report.ClusterSelf && r.Name != "" && r.Name != s.name {
					v.Title, v.ReadBy = r.Name, sourceAgent(s.name)
				}
				if r.ID == report.ClusterSelf {
					if v.Usage = s.usageOf(r.ID, now); v.Usage != nil {
						v.History = historyKey(r)
					}
					if r.Facts["distribution"] == ibmiDistribution {
						v.Partition = partitionView(r, s.recs, power)
					}
				}
			case report.KindNode:
				v.Nodes++
				if r.State == report.StateOK {
					v.NodesReady++
				}
			default:
				v.Workloads++
			}
		}
		// What a host agent reads is "host:source": say which host, title or not.
		if host, _, ok := strings.Cut(s.name, ":"); ok && v.ReadBy == "" {
			v.ReadBy = host
		}
		// The version first, then the pools, then operators, each by name.
		slices.SortFunc(v.Parts, func(a, b ClusterPart) int {
			order := func(n string) int {
				switch {
				case strings.HasPrefix(n, "clusterversion/"):
					return 0
				case strings.HasPrefix(n, "machineconfigpool/"):
					return 1
				}
				return 2
			}
			return cmp.Or(cmp.Compare(order(a.Name), order(b.Name)), strings.Compare(a.Name, b.Name))
		})
		if v.Status == "good" && v.Problems > 0 {
			v.Status, v.Word = "warning", plural(v.Problems, "problem", "problems")
		}
		out = append(out, v)
	}
	return out
}

// handleClusters is every cluster, with its nodes most in need first
// (?nodes= caps them; 200, at most 2000).
func (srv *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	_, conds := srv.view(now)
	most := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("nodes")); err == nil && n >= 0 {
		most = min(n, 2000)
	}
	type nodeRow struct {
		Name     string         `json:"name"`
		State    string         `json:"state"`
		Severity string         `json:"severity"`
		Word     string         `json:"word"`
		Since    *time.Time     `json:"since,omitempty"`
		Facts    map[string]any `json:"facts,omitempty"`
	}
	views := srv.clusterViews(now, conds)
	nodes := map[string][]nodeRow{}
	for _, s := range srv.workloadSnapshot() {
		list := []nodeRow{}
		for _, rec := range s.recs {
			if rec.Kind != report.KindNode {
				continue
			}
			n := nodeRow{Name: rec.Name, State: rec.State, Severity: stateSeverity(rec.State), Word: rec.Word, Facts: rec.Facts}
			if !rec.Since.IsZero() {
				t := rec.Since
				n.Since = &t
			}
			list = append(list, n)
		}
		slices.SortFunc(list, func(a, b nodeRow) int {
			return cmp.Or(cmp.Compare(rank(a.Severity), rank(b.Severity)), strings.Compare(a.Name, b.Name))
		})
		nodes[s.name] = window(list, 0, most)
	}
	type full struct {
		ClusterView
		NodeList []nodeRow `json:"node_list"`
	}
	out := make([]full, 0, len(views))
	for _, v := range views {
		out = append(out, full{v, nodes[v.Name]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

// rank orders severities most in need first.
func rank(sev string) int {
	switch sev {
	case "critical":
		return 0
	case "warning":
		return 1
	case "waiting":
		return 2
	case "good":
		return 3
	}
	return 4
}

// WorkloadRow is one workload in the Workloads list.
type WorkloadRow struct {
	Source string `json:"source"`
	report.Workload
	Severity string `json:"severity"`
	// Host is the host agent inside a VM, linked by its SMBIOS UUID.
	Host *HostLink `json:"host,omitempty"`
	// Usage is CPU and memory now, for nodes, controllers and VMs on a
	// cluster with metrics.k8s.io.
	Usage *UsageNow `json:"usage,omitempty"`
	// Volumes are the array volumes a PVC uses; PVCs the PVCs an array
	// volume serves (docs/pure.md, step 3).
	Volumes []VolumeLink `json:"volumes,omitempty"`
	PVCs    []PVCLink    `json:"pvcs,omitempty"`
}

// handleWorkloadList is every workload, most in need first: ?source=,
// ?kind=, ?ns=, ?node=, ?owner=, ?show= (problems, good, waiting, off),
// ?q= (name, namespace, owner or node), ?offset=, ?limit= (100, at most
// 1000). counts leave show and kind out and kinds leaves kind out, so each
// choice can say how many it holds; namespaces are the chosen cluster's.
func (srv *Server) handleWorkloadList(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	qv := r.URL.Query()
	source, kind, ns, node, owner, show := qv.Get("source"), qv.Get("kind"), qv.Get("ns"), qv.Get("node"), qv.Get("owner"), qv.Get("show")
	q := strings.ToLower(strings.TrimSpace(qv.Get("q")))
	offset, limit := page(r, 100, 1000)
	counts := map[string]int{"all": 0, "problems": 0, "good": 0, "waiting": 0, "off": 0}
	kinds := map[string]int{}
	nsSet := map[string]bool{}
	rows := []WorkloadRow{}
	_, byVM := srv.vmLinks()
	snaps := srv.workloadSnapshot()
	byPVC, byVolume := volumeLinks(snaps)
	for _, s := range snaps {
		if source != "" && s.name != source {
			continue
		}
		for _, rec := range s.recs {
			if rec.Kind == report.KindCluster {
				continue
			}
			if rec.Namespace != "" {
				nsSet[rec.Namespace] = true
			}
			if (ns != "" && rec.Namespace != ns) || (node != "" && rec.Node != node && !(rec.Kind == report.KindNode && rec.Name == node)) ||
				(owner != "" && rec.Owner != owner && rec.Name != owner) {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(rec.Name+" "+rec.Namespace+" "+rec.Owner+" "+rec.Node), q) {
				continue
			}
			sev := stateSeverity(rec.State)
			counts["all"]++
			switch sev {
			case "critical", "warning":
				counts["problems"]++
			default:
				counts[sev]++
			}
			if show != "" && !(show == sev || show == "problems" && (sev == "critical" || sev == "warning")) {
				continue
			}
			kinds[rec.Kind]++
			if kind != "" && rec.Kind != kind {
				continue
			}
			row := WorkloadRow{Source: s.name, Workload: rec, Severity: sev, Usage: s.usageOf(rec.ID, now)}
			if h, ok := byVM["vm/"+s.name+"/"+rec.Namespace+"/"+rec.Name]; ok && rec.Kind == report.KindVM {
				row.Host = &h
			}
			if rec.Kind == report.KindPVC {
				k := s.name + "/" + rec.Namespace + "/" + rec.Name
				row.Volumes, row.PVCs = byPVC[k], byVolume[k]
			}
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b WorkloadRow) int {
		return cmp.Or(cmp.Compare(rank(a.Severity), rank(b.Severity)), strings.Compare(a.Source, b.Source),
			strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	namespaces := make([]string, 0, len(nsSet))
	if source != "" {
		for n := range nsSet {
			namespaces = append(namespaces, n)
		}
		slices.Sort(namespaces)
	}
	// Facts and labels stay for whoever opens a row: the list is the rest.
	win := window(rows, offset, limit)
	writeJSON(w, http.StatusOK, map[string]any{"workloads": win, "total": len(rows), "offset": offset, "limit": limit,
		"counts": counts, "kinds": kinds, "namespaces": namespaces, "clusters": names(srv.clusterSnaps()), "hypervisors": names(srv.hostVMSnaps())})
}

// lowerFirst lowers a word's first letter, for the middle of a sentence,
// and leaves names like CrashLoopBackOff alone.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// upperFirst raises a word's first letter, for the start of a title.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// names are the sources' names, sorted, for the source list.
func names(snaps []wsnap) []string {
	out := []string{}
	for _, s := range snaps {
		out = append(out, s.name)
	}
	slices.Sort(out)
	return out
}
