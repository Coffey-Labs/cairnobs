package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/units"
	"github.com/cairnobs/cairnobs/internal/version"
)

// maxReport bounds an agent's report body. A host with a few hundred
// containers stays well under it.
const maxReport = 4 << 20

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (srv *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	agent, ok := srv.agentFor(r)
	if !ok {
		http.Error(w, srv.agentRefusal(), http.StatusUnauthorized)
		return
	}
	if srv.clusterAgent(agent) {
		http.Error(w, agent+" is a cluster agent: it sends workloads, not host reports", http.StatusForbidden)
		return
	}
	var rep report.Report
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReport))
	if err == nil {
		err = json.Unmarshal(body, &rep)
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rep.V != report.Version {
		http.Error(w, fmt.Sprintf("protocol version %d, want %d", rep.V, report.Version), http.StatusBadRequest)
		return
	}
	queued, now := r.Header.Get("X-Beacon-Queued") != "", time.Now()
	if queued {
		// Sent from the agent's queue after obs-beacond was away: history
		// for the sparklines, not the host's state now (a newer report
		// follows it).
		srv.store.History(agent, rep)
	} else {
		srv.store.Put(agent, rep, now)
		srv.forgetSources(agent, rep.Sources)
	}
	srv.shareReport(agent, body, rep, queued, now)
	// Tell the agent which check list it should be running; it fetches the
	// list when this differs from what it has.
	if _, ver := srv.workFor(agent); ver != "" {
		w.Header().Set("X-Beacon-Checks", ver)
	}
	// What came of actions it took, and whether more wait for it.
	srv.actionsAnswer(w, agent, rep.Actions)
	w.WriteHeader(http.StatusNoContent)
}

// DiskView is one filesystem as the UI shows it.
type DiskView struct {
	Mount string  `json:"mount"`
	FS    string  `json:"fs"`
	Size  uint64  `json:"size"`
	Used  uint64  `json:"used"`
	Pct   float64 `json:"pct"`
}

// HostView is one host as the UI shows it.
type HostView struct {
	Name     string     `json:"name"`
	Status   string     `json:"status"` // good, warning, critical, waiting
	Word     string     `json:"word"`
	LastSeen *time.Time `json:"last_seen"`
	Silent   bool       `json:"silent"`
	// A workstation never pages (obs-beacond.yaml agents: workstation: true).
	Workstation bool `json:"workstation,omitempty"`
	// VM is the VM this host is, when a cluster's VM has its SMBIOS UUID.
	VM *VMLink `json:"vm,omitempty"`
	// VMs counts the libvirt VMs this host runs, when it runs libvirt.
	VMs *HostVMCount `json:"vms,omitempty"`

	Agent    string      `json:"agent,omitempty"`
	Kernel   string      `json:"kernel,omitempty"`
	OS       string      `json:"os,omitempty"`
	Uptime   float64     `json:"uptime_s"`
	CPUs     int         `json:"cpus"`
	CPU      float64     `json:"cpu_pct"`
	Load     [3]float64  `json:"load"`
	MemPct   float64     `json:"mem_pct"`
	MemTotal uint64      `json:"mem_total"`
	SwapPct  float64     `json:"swap_pct"`
	Disks    []DiskView  `json:"disks"`
	Fullest  *DiskView   `json:"fullest,omitempty"`
	Net      report.Net  `json:"net"`
	History  HostHistory `json:"history"`

	Containers      ContainerSummary `json:"containers"`
	ContainersError string           `json:"containers_error,omitempty"`
	Units           []UnitView       `json:"units"`
	// Events is a Windows host's event log: the errors and criticals of
	// the last day (docs/windows.md, step 7).
	Events *report.Events `json:"events,omitempty"`
	rows   []ContainerRow
}

// UnitView is one expected systemd unit as the UI shows it.
type UnitView struct {
	report.Unit
	Host     string `json:"host"`
	Severity string `json:"severity"` // good, critical, waiting
	Word     string `json:"word"`
}

// windowsHost says a report's OS is Windows: its units are Windows
// services (docs/windows.md).
func windowsHost(os string) bool { return strings.HasPrefix(os, "Windows") }

// judgeUnits compares a host's expected units with what its agent saw.
func judgeUnits(host string, want []string, got []report.Unit) []UnitView {
	seen := map[string]report.Unit{}
	for _, u := range got {
		seen[u.Name] = u
	}
	out := []UnitView{}
	for _, n := range want {
		n = units.Name(n)
		u, ok := seen[n]
		v := UnitView{Unit: u, Host: host}
		v.Name = n
		switch {
		case !ok:
			v.Severity, v.Word = "waiting", "Not reported yet"
		case u.Load == "not-found":
			v.Severity, v.Word = "critical", "Not installed"
		case u.Active == "active":
			v.Severity, v.Word = "good", "Running"
			if u.Sub != "running" && u.Sub != "" {
				v.Word = "Active (" + u.Sub + ")"
			}
		case u.Active == "activating" || u.Active == "reloading":
			v.Severity, v.Word = "warning", "Starting"
		default:
			v.Severity, v.Word = "critical", strings.ToUpper(u.Active[:1])+u.Active[1:]
		}
		out = append(out, v)
	}
	return out
}

// HostHistory is the in-memory history for sparklines, oldest first.
type HostHistory struct {
	CPU []float64 `json:"cpu"`
	Mem []float64 `json:"mem"`
	Rx  []float64 `json:"rx"`
	Tx  []float64 `json:"tx"`
}

// Attention is one "needs attention" card.
type Attention struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Href     string `json:"href"`
	Host     string `json:"host"`
	Key      string `json:"key,omitempty"`
	Affected string `json:"affected,omitempty"` // what else it takes down, from root-cause grouping
	// Incident is the open incident it is, once there is one (today.go).
	Incident *AttnIncident `json:"incident,omitempty"`
}

func nonNil(s []float64) []float64 {
	if s == nil {
		return []float64{}
	}
	return s
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(used) / float64(total)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Condition is one thing wrong right now. Key is stable for as long as the
// problem lasts (the same container on the same host, the same disk), so
// the alert engine can follow it from one evaluation to the next.
type Condition struct {
	Key      string      `json:"key"`
	Severity string      `json:"severity"` // warning or critical
	Title    string      `json:"title"`
	Detail   string      `json:"detail"`
	Host     string      `json:"host"`
	Href     string      `json:"href"`
	Affected string      `json:"affected,omitempty"` // what else this takes down (root-cause grouping)
	Grouped  []GroupedBy `json:"grouped,omitempty"`  // the learned dependencies that grouped it, when any did
	Map      string      `json:"map,omitempty"`      // where it is on the maps: a page path with its focus
	// Ends is when the certificate a condition is about ends, for one that
	// is: certSeverity keeps it a warning until it's close (#34).
	Ends time.Time `json:"-"`
	// Batch folds it with others alike from the same place (batches.go).
	Batch *Batch `json:"-"`
}

// view judges every host now: what the UI shows, and the conditions the
// alert engine works from.
func (srv *Server) view(now time.Time) ([]HostView, []Condition) {
	// Empty lists, never nil: the browser gets [] rather than null.
	hosts := []HostView{}
	conds := []Condition{}
	expected := srv.currentExpected()
	for _, h := range srv.store.Snapshot() {
		v := HostView{Name: h.name, Disks: []DiskView{}, Units: []UnitView{}, History: HostHistory{CPU: []float64{}, Mem: []float64{}, Rx: []float64{}, Tx: []float64{}}}
		href := "/hosts/" + h.name
		ws := srv.workstation(h.name)
		v.Workstation = ws
		// A workstation's problems never page: warning is as high as they go.
		capped := func(sev string) string {
			if ws {
				return "warning"
			}
			return sev
		}
		if h.seen.IsZero() {
			v.Status, v.Word = "waiting", "Waiting for its first report"
			// A host that never reports after obs-beacond starts is as bad as one that went quiet.
			if since := now.Sub(srv.started); since > srv.cfg.StaleAfter {
				v.Status, v.Word, v.Silent = capped("critical"), "Silent", true
				conds = append(conds, Condition{Key: "silent/" + h.name, Severity: capped("critical"), Title: h.name + " has gone silent",
					Detail: "No report since obs-beacond started " + since.Round(time.Second).String() + " ago. Its agent, the host or the link to it is down.", Host: h.name, Href: href})
			}
			hosts = append(hosts, v)
			continue
		}
		seen := h.seen
		r := h.last
		v.LastSeen = &seen
		if r.VMs != nil {
			n := &HostVMCount{Total: len(r.VMs.List), Error: r.VMs.Error, Href: workloadHref(h.name, "", "")}
			for _, w := range r.VMs.List {
				if vmRunning(w.State) {
					n.Running++
				}
			}
			v.VMs = n
		}
		v.Agent, v.Kernel, v.OS, v.Uptime, v.CPUs, v.CPU, v.Load = r.Agent, r.Kernel, r.OS, r.Uptime, r.CPUs, r.CPU, r.Load
		v.MemPct, v.MemTotal = memPct(r.Mem), r.Mem.Total
		v.SwapPct = pct(r.Mem.SwapTotal-min(r.Mem.SwapFree, r.Mem.SwapTotal), r.Mem.SwapTotal)
		v.Net = r.Net
		v.History = HostHistory{CPU: nonNil(h.cpu), Mem: nonNil(h.mem), Rx: nonNil(h.rx), Tx: nonNil(h.tx)}
		for _, d := range r.Disks {
			dv := DiskView{Mount: d.Mount, FS: d.FS, Size: d.Size, Used: d.Used, Pct: pct(d.Used, d.Used+d.Avail)}
			v.Disks = append(v.Disks, dv)
			if v.Fullest == nil || dv.Pct > v.Fullest.Pct {
				f := dv
				v.Fullest = &f
			}
		}
		// A workstation's containers come and go with whatever is being worked
		// on: they aren't judged, drawn on the maps or compared for drift.
		if r.Containers != nil && !ws {
			v.ContainersError = r.Containers.Error
			v.rows, v.Containers = expected.Judge(h.name, r.Containers.List)
		}

		crit, warn := 0, 0
		add := func(c Condition) {
			c.Host = h.name
			c.Severity = capped(c.Severity)
			if c.Severity == "critical" {
				crit++
			} else {
				warn++
			}
			conds = append(conds, c)
		}
		if age := now.Sub(h.seen); age > srv.cfg.StaleAfter {
			v.Silent = true
			add(Condition{Key: "silent/" + h.name, Severity: "critical", Title: h.name + " has gone silent",
				Detail: "No report for " + age.Round(time.Second).String() + ". Its agent, the host or the link to it is down.", Href: href})
		}
		for _, d := range v.Disks {
			key := "disk/" + h.name + "/" + d.Mount
			switch {
			case d.Pct >= 90:
				add(Condition{Key: key, Severity: "critical", Title: fmt.Sprintf("%s %s is %.0f%% full", h.name, d.Mount, d.Pct), Detail: "Alert at 90%.", Host: "", Href: href})
			case d.Pct >= 80:
				add(Condition{Key: key, Severity: "warning", Title: fmt.Sprintf("%s %s is %.0f%% full", h.name, d.Mount, d.Pct), Detail: "Warning at 80%.", Host: "", Href: href})
			}
		}
		if r.Mem.Total > 0 && float64(r.Mem.Available) < 0.1*float64(r.Mem.Total) {
			add(Condition{Key: "memory/" + h.name, Severity: "warning", Title: h.name + " is low on memory", Detail: fmt.Sprintf("%.0f%% in use.", v.MemPct), Host: "", Href: href})
		}
		for _, row := range v.rows {
			if row.Verdict.Severity != "critical" && row.Verdict.Severity != "warning" {
				continue
			}
			add(Condition{Key: "container/" + h.name + "/" + row.Name, Severity: row.Verdict.Severity,
				Title: fmt.Sprintf("%s on %s: %s", row.Name, h.name, strings.ToLower(row.Verdict.Word)), Detail: row.Verdict.Detail, Host: "", Href: "/containers"})
		}
		v.Units = judgeUnits(h.name, expected.Hosts[h.name].Units, r.Units)
		manager, win := "systemd", windowsHost(r.OS)
		if win {
			manager = "Windows"
		}
		for _, u := range v.Units {
			if u.Severity == "critical" {
				name := u.Name
				if win {
					name = units.ServiceName(name)
				}
				add(Condition{Key: "unit/" + h.name + "/" + u.Name, Severity: "critical", Title: fmt.Sprintf("%s on %s: %s", name, h.name, strings.ToLower(u.Word)),
					Detail: manager + " reports it " + u.Active + " (" + u.Sub + ").", Host: "", Href: href})
			}
		}
		if h.events != nil && now.Sub(h.events.At) < 2*time.Hour {
			v.Events = h.events
			for _, c := range eventConditions(h.name, h.events, now) {
				add(c)
			}
		}
		if v.ContainersError != "" {
			add(Condition{Key: "containers-unreadable/" + h.name, Severity: "warning", Title: "Can't read containers on " + h.name, Detail: v.ContainersError, Host: "", Href: href})
		}

		switch {
		case v.Silent:
			v.Status, v.Word = capped("critical"), "Silent"
		case crit > 0:
			v.Status, v.Word = "critical", plural(crit, "critical", "critical")
		case warn > 0:
			v.Status, v.Word = "warning", plural(warn, "warning", "warnings")
		default:
			v.Status, v.Word = "good", "Up"
		}
		hosts = append(hosts, v)
	}
	conds = append(conds, srv.cloudflareConditions(now)...)
	conds = append(conds, srv.akamaiConditions(now)...)
	conds = append(conds, srv.mailConditions(now)...)
	conds = append(conds, srv.backupConditions(now)...)
	conds = append(conds, srv.imageDrift(hosts, now)...)
	_, checkConds := srv.judgeChecks(now)
	conds = append(conds, checkConds...)
	_, dlConds := srv.dataLayerView(now)
	conds = append(conds, dlConds...)
	conds = append(conds, srv.workloadConditions(now)...)
	conds = append(conds, srv.certConditions(now)...)
	certSeverity(conds, now, srv.certCritical())
	if byHost, _ := srv.vmLinks(); len(byHost) > 0 {
		explainSilent(conds, byHost)
		for i := range hosts {
			if v, ok := byHost[hosts[i].Name]; ok {
				hosts[i].VM = &v
			}
		}
	}
	sort.SliceStable(conds, func(i, j int) bool { return conds[i].Severity == "critical" && conds[j].Severity != "critical" })
	return hosts, conds
}

// attention turns conditions into the dashboard's cards: per host, more
// than four container problems fold into one "N more" card.
func attention(conds []Condition) []Attention {
	out := []Attention{}
	shown := map[string]int{}
	total := map[string]int{}
	for _, c := range conds {
		if strings.HasPrefix(c.Key, "container/") {
			total[c.Host]++
		}
	}
	folded := map[string]bool{}
	for _, c := range conds {
		if strings.HasPrefix(c.Key, "container/") && total[c.Host] > 4 {
			if shown[c.Host] == 3 {
				if !folded[c.Host] {
					folded[c.Host] = true
					out = append(out, Attention{Severity: "warning", Title: fmt.Sprintf("%d more containers on %s need a look", total[c.Host]-3, c.Host), Href: "/containers", Host: c.Host})
				}
				continue
			}
			shown[c.Host]++
		}
		out = append(out, Attention{Severity: c.Severity, Title: c.Title, Detail: c.Detail, Href: c.Href, Host: c.Host, Key: c.Key, Affected: c.Affected})
	}
	return out
}

func (srv *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	hosts, conds := srv.view(now)
	attn := attention(srv.groupConditions(conds, srv.alerts.firing))
	var sum ContainerSummary
	for _, h := range hosts {
		sum.add(h.Containers)
	}
	open, _ := srv.alerts.snapshot()
	attn = withIncidents(attn, open)
	firing, unacked := 0, 0
	for _, inc := range open {
		if inc.Fired != nil {
			firing++
			if inc.Ack == nil {
				unacked++
			}
		}
	}
	checks := map[string]any{"down": 0, "severity": "warning"}
	views, _ := srv.judgeChecks(now)
	down := 0
	for _, v := range views {
		if v.Word == "Down" {
			down++
			if v.Status == "critical" {
				checks["severity"] = "critical"
			}
		}
	}
	checks["down"] = down
	writeJSON(w, http.StatusOK, map[string]any{
		// hosts is all of them in a small fleet, else the most in need
		// (lists.go); fleet counts every one.
		"now": now, "version": version.Version, "hosts": overviewList(hosts), "fleet": summarize(hosts), "checks": checks,
		"attention": attn, "containers": sum, "clusters": srv.clusterViews(now, conds),
		"alerts":   map[string]int{"firing": firing, "unacknowledged": unacked},
		"features": srv.features(),
		// The dashboard's briefing (today.go): what happened since this
		// person last looked, each service's last day, and the hosts
		// heading somewhere.
		"today": srv.todayFor(r, now), "services": srv.serviceBriefs(now), "watch": srv.watch(hosts, now),
	})
}

func (srv *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	hosts, conds := srv.view(time.Now())
	attn := attention(conds)
	for _, h := range hosts {
		if h.Name == name {
			mine := []Attention{}
			for _, a := range attn {
				if a.Host == name {
					mine = append(mine, a)
				}
			}
			rows := h.rows
			if rows == nil {
				rows = []ContainerRow{}
			}
			writeJSON(w, http.StatusOK, map[string]any{"host": h, "containers": srv.logsFor(r, rows), "attention": mine})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such host"})
}

// HostVMCount is how many libvirt VMs a host runs, and where to see them.
type HostVMCount struct {
	Total   int    `json:"total"`
	Running int    `json:"running"`
	Error   string `json:"error,omitempty"`
	Href    string `json:"href"`
}
