package server

// Settings edited in the UI: active checks,
// maintenance windows and alert channels (channels.go). A section set in a
// config file wins: checks when `checks` names a file, maintenance windows
// when obs-beacond.yaml has a `maintenance` key (even an empty list), alert
// channels when it has `alerts.channels`. The UI then shows that section
// read-only, so hosts-baseline-style setups keep working unchanged.
// Otherwise the section lives in the database, kept as the YAML its file
// would hold: export downloads that text, import replaces it. Every change
// goes in the audit log.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/check"
	"github.com/cairnobs/cairnobs/internal/maint"
	"github.com/cairnobs/cairnobs/internal/state"
)

const (
	sectionChecks      = "checks"
	sectionMaintenance = "maintenance"
	maxSettingsYAML    = 1 << 20
)

// Where a section comes from.
const (
	sourceFile = "file" // a config file; read-only in the UI
	sourceUI   = "ui"   // the database; edited in the UI
)

// settingID is what a check id or window name made in the UI may be: it's
// part of a URL and of alert keys.
var settingID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

func (srv *Server) checksSource() string {
	switch {
	case srv.cfg.Checks != "":
		return sourceFile
	case srv.db != nil:
		return sourceUI
	}
	return ""
}

func (srv *Server) maintenanceSource() string {
	switch {
	case srv.cfg.Maintenance != nil:
		return sourceFile
	case srv.db != nil:
		return sourceUI
	}
	return ""
}

// maintWindows holds the maintenance windows in force.
type maintWindows struct {
	mu  sync.RWMutex
	cur []maint.Window // checked; replaced whole, never changed in place
	raw []maint.Window // as entered (UI only)
}

func (srv *Server) windows() []maint.Window {
	srv.maint.mu.RLock()
	defer srv.maint.mu.RUnlock()
	return srv.maint.cur
}

// loadSettings reads the UI-edited sections at start. Host names aren't
// checked here: a host removed since stays named but matches nothing, and
// the next edit of that check or window asks for it to go.
func (srv *Server) loadSettings() error {
	if srv.maintenanceSource() == sourceFile {
		srv.maint.cur = srv.cfg.Maintenance
	}
	if srv.db == nil {
		return nil
	}
	if err := srv.loadChecksSetting(); err != nil {
		return fmt.Errorf("stored checks: %w", err)
	}
	if err := srv.loadMaintenanceSetting(); err != nil {
		return fmt.Errorf("stored maintenance windows: %w", err)
	}
	if err := srv.loadChannels(); err != nil {
		return fmt.Errorf("stored alert channels: %w", err)
	}
	if err := srv.loadExpectedSetting(); err != nil {
		return fmt.Errorf("stored expected containers and services: %w", err)
	}
	if err := srv.loadMapFixes(); err != nil {
		return fmt.Errorf("stored map corrections: %w", err)
	}
	return nil
}

// loadChecksSetting puts the checks saved in the UI in force; the caller
// holds srv.checks.mu, or is starting up.
func (srv *Server) loadChecksSetting() error {
	if srv.checksSource() != sourceUI {
		return nil
	}
	s, ok, err := srv.db.Setting(sectionChecks)
	if err != nil {
		return err
	}
	if !ok {
		srv.checks.cur = &Checks{}
		return nil
	}
	raw, cur, err := parseUIChecks([]byte(s.YAML), nil)
	if err != nil {
		return err
	}
	srv.checks.raw, srv.checks.cur = raw, cur
	return nil
}

// loadMaintenanceSetting puts the windows saved in the UI in force; the
// caller holds srv.maint.mu, or is starting up.
func (srv *Server) loadMaintenanceSetting() error {
	if srv.maintenanceSource() != sourceUI {
		return nil
	}
	s, ok, err := srv.db.Setting(sectionMaintenance)
	if err != nil || !ok {
		return err
	}
	raw, cur, err := parseUIWindows([]byte(s.YAML), nil)
	if err != nil {
		return err
	}
	srv.maint.raw, srv.maint.cur = raw, cur
	return nil
}

// decodeStrict decodes one YAML document, refusing unknown keys. An empty
// document is an empty section.
func decodeStrict(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// parseUIChecks reads a checks document for the database: raw is as
// written, cur has the defaults filled in.
func parseUIChecks(b []byte, known map[string]bool) ([]check.Spec, *Checks, error) {
	var c Checks
	if err := decodeStrict(b, &c); err != nil {
		return nil, nil, err
	}
	return checkUIChecks(c.Checks, known)
}

func checkUIChecks(raw []check.Spec, known map[string]bool) ([]check.Spec, *Checks, error) {
	for _, s := range raw {
		if !settingID.MatchString(s.ID) {
			return nil, nil, fmt.Errorf("check id %q: letters, digits, space, dot, dash and underscore, up to 64", s.ID)
		}
	}
	cur := &Checks{Checks: slices.Clone(raw)}
	if err := cur.check(known); err != nil {
		return nil, nil, err
	}
	if raw == nil {
		raw = []check.Spec{}
	}
	return raw, cur, nil
}

type windowsDoc struct {
	Maintenance []maint.Window `yaml:"maintenance"`
}

func parseUIWindows(b []byte, known map[string]bool) ([]maint.Window, []maint.Window, error) {
	var d windowsDoc
	if err := decodeStrict(b, &d); err != nil {
		return nil, nil, err
	}
	return checkUIWindows(d.Maintenance, known)
}

func checkUIWindows(raw []maint.Window, known map[string]bool) ([]maint.Window, []maint.Window, error) {
	cur := slices.Clone(raw)
	names := map[string]bool{}
	for i := range cur {
		w := &cur[i]
		if !settingID.MatchString(w.Name) {
			return nil, nil, fmt.Errorf("window name %q: letters, digits, space, dot, dash and underscore, up to 64", w.Name)
		}
		if names[w.Name] {
			return nil, nil, fmt.Errorf("two maintenance windows called %q", w.Name)
		}
		names[w.Name] = true
		if err := w.Check(); err != nil {
			return nil, nil, err
		}
		if len(w.Hosts) == 0 {
			return nil, nil, fmt.Errorf("maintenance window %q names no hosts", w.Name)
		}
		for _, h := range w.Hosts {
			if known != nil && !known[h] {
				return nil, nil, fmt.Errorf("maintenance window %q: no host called %q", w.Name, h)
			}
		}
	}
	if raw == nil {
		raw = []maint.Window{}
	}
	return raw, cur, nil
}

// compactYAML writes v as YAML without the keys left at their zero value,
// so an exported file reads like one written by hand.
func compactYAML(v any) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	prune(&n)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

// prune drops zero-valued keys from every mapping.
func prune(n *yaml.Node) {
	for _, c := range n.Content {
		prune(c)
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	var keep []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if zeroNode(v) {
			continue
		}
		keep = append(keep, k, v)
	}
	n.Content = keep
}

func zeroNode(v *yaml.Node) bool {
	switch v.Kind {
	case yaml.ScalarNode:
		switch v.Tag {
		case "!!null":
			return true
		case "!!str":
			return v.Value == "" || v.Value == "0s"
		case "!!int":
			return v.Value == "0"
		case "!!bool":
			return v.Value == "false"
		}
	case yaml.SequenceNode, yaml.MappingNode:
		return len(v.Content) == 0
	}
	return false
}

// saveSection stores a section's YAML; the caller holds the section's lock.
func (srv *Server) saveSection(section string, doc any, who string) error {
	b, err := compactYAML(doc)
	if err != nil {
		return err
	}
	if err := srv.db.PutSetting(section, state.Setting{YAML: string(b), Updated: time.Now(), UpdatedBy: who}); err != nil {
		return err
	}
	srv.publish(noticeSetting, section)
	return nil
}

// saveSealed stores a section sealed with the state key, for sections that
// hold secrets.
func (srv *Server) saveSealed(section string, b []byte, who string) error {
	if err := srv.db.PutSetting(section, state.Setting{YAML: srv.db.Seal(b), Updated: time.Now(), UpdatedBy: who}); err != nil {
		return err
	}
	srv.publish(noticeSetting, section)
	return nil
}

func (srv *Server) knownHosts() map[string]bool {
	return agentNames(srv.agentList())
}

// readOnly answers a change to a section set in a config file, or with no
// database to keep it in. It reports whether it answered.
func (srv *Server) readOnly(w http.ResponseWriter, source, what, file string) bool {
	switch source {
	case sourceUI:
		return false
	case sourceFile:
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("The %s are set in %s; change them there.", what, file)})
	default:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Cairn OBS has no database configured, so nothing set here could be kept. Set `database` in obs-beacond.yaml."})
	}
	return true
}

// checkForm is one check as the UI edits it.
type checkForm struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`   // http, tcp, dns, ping or rbl
	Target   string   `json:"target"` // the URL, host:port, name, host or address
	Lists    []string `json:"lists"`
	Status   int      `json:"status"`
	Contains string   `json:"contains"`
	TLS      bool     `json:"tls"`
	Type     string   `json:"type"`
	Server   string   `json:"server"`
	EveryS   int      `json:"every_s"`
	TimeoutS int      `json:"timeout_s"`
	From     []string `json:"from"`
	Severity string   `json:"severity"`
	WarnDays int      `json:"tls_warn_days"`
	CritDays int      `json:"tls_critical_days"`
}

func formOf(s check.Spec) checkForm {
	f := checkForm{ID: s.ID, Name: s.Name, Kind: s.Kind(), Lists: s.Lists, Status: s.Status, Contains: s.Contains, TLS: s.TLS,
		Type: s.Type, Server: s.Server, EveryS: int(s.Every / time.Second), TimeoutS: int(s.Timeout / time.Second),
		From: s.From, Severity: s.Severity, WarnDays: s.TLSWarnDays, CritDays: s.TLSCriticalDays}
	f.Target = map[string]string{"http": s.HTTP, "tcp": s.TCP, "dns": s.DNS, "ping": s.Ping, "rbl": s.RBL}[f.Kind]
	if f.From == nil {
		f.From = []string{}
	}
	if f.Lists == nil {
		f.Lists = []string{}
	}
	return f
}

func (f checkForm) spec() (check.Spec, error) {
	s := check.Spec{ID: strings.TrimSpace(f.ID), Name: strings.TrimSpace(f.Name), Contains: f.Contains,
		Every: time.Duration(f.EveryS) * time.Second, Timeout: time.Duration(f.TimeoutS) * time.Second,
		From: f.From, Severity: f.Severity, TLSWarnDays: f.WarnDays, TLSCriticalDays: f.CritDays}
	t := strings.TrimSpace(f.Target)
	if t == "" {
		return s, errors.New("say what to check")
	}
	switch f.Kind {
	case "http":
		s.HTTP, s.Status = t, f.Status
	case "tcp":
		s.TCP, s.TLS = t, f.TLS
	case "dns":
		s.DNS, s.Type, s.Server = t, f.Type, strings.TrimSpace(f.Server)
	case "ping":
		s.Ping = t
	case "rbl":
		s.RBL, s.Lists = t, f.Lists
	default:
		return s, fmt.Errorf("kind %q isn't http, tcp, dns, ping or rbl", f.Kind)
	}
	if s.Name == s.ID {
		s.Name = "" // the default; keeps exports short
	}
	return s, nil
}

func (srv *Server) handleSettingsChecks(w http.ResponseWriter, r *http.Request) {
	src := srv.checksSource()
	forms := []checkForm{}
	if src == sourceUI {
		srv.checks.mu.Lock()
		for _, s := range srv.checks.raw {
			forms = append(forms, formOf(s))
		}
		srv.checks.mu.Unlock()
	}
	hosts := []string{}
	for _, a := range srv.agentList() {
		hosts = append(hosts, a.Name)
	}
	slices.Sort(hosts)
	writeJSON(w, http.StatusOK, map[string]any{"source": src, "file": srv.cfg.Checks, "checks": forms, "hosts": hosts})
}

// handleCheckPut adds a check or replaces the one with that id.
func (srv *Server) handleCheckPut(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.checksSource(), "checks", srv.cfg.Checks) {
		return
	}
	var f checkForm
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&f); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	f.ID = r.PathValue("id")
	s, err := f.spec()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	srv.checks.mu.Lock()
	defer srv.checks.mu.Unlock()
	raw := slices.Clone(srv.checks.raw)
	verb := "added a check"
	if i := slices.IndexFunc(raw, func(c check.Spec) bool { return c.ID == s.ID }); i >= 0 {
		raw[i], verb = s, "changed a check"
	} else {
		raw = append(raw, s)
	}
	// Only this check's hosts are checked: another check naming a host
	// removed since shouldn't stop this edit.
	known := srv.knownHosts()
	for _, h := range s.From {
		if !known[h] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("No host called %q.", h)})
			return
		}
	}
	raw, cur, err := checkUIChecks(raw, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := srv.saveSection(sectionChecks, Checks{Checks: raw}, srv.who(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.checks.raw, srv.checks.cur = raw, cur
	srv.audit(srv.who(r), verb, s.ID+": "+s.Kind()+" "+s.Target())
	writeJSON(w, http.StatusOK, formOf(s))
}

func (srv *Server) handleCheckDelete(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.checksSource(), "checks", srv.cfg.Checks) {
		return
	}
	id := r.PathValue("id")
	srv.checks.mu.Lock()
	defer srv.checks.mu.Unlock()
	i := slices.IndexFunc(srv.checks.raw, func(c check.Spec) bool { return c.ID == id })
	if i < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No check called " + id + "."})
		return
	}
	raw := slices.Delete(slices.Clone(srv.checks.raw), i, i+1)
	cur := &Checks{Checks: slices.Clone(srv.checks.cur.Checks)}
	cur.Checks = slices.DeleteFunc(cur.Checks, func(c check.Spec) bool { return c.ID == id })
	if err := srv.saveSection(sectionChecks, Checks{Checks: raw}, srv.who(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.checks.raw, srv.checks.cur = raw, cur
	srv.audit(srv.who(r), "removed a check", id)
	w.WriteHeader(http.StatusNoContent)
}

func (srv *Server) handleSettingsWindows(w http.ResponseWriter, r *http.Request) {
	src := srv.maintenanceSource()
	out := []windowForm{}
	if src == sourceUI {
		srv.maint.mu.RLock()
		for _, w := range srv.maint.raw {
			out = append(out, windowFormOf(w))
		}
		srv.maint.mu.RUnlock()
	}
	hosts := []string{}
	for _, a := range srv.agentList() {
		hosts = append(hosts, a.Name)
	}
	slices.Sort(hosts)
	writeJSON(w, http.StatusOK, map[string]any{"source": src, "windows": out, "hosts": hosts})
}

// windowForm is one maintenance window as the UI edits it.
type windowForm struct {
	Name      string   `json:"name"`
	Hosts     []string `json:"hosts"`
	Daily     string   `json:"daily"`
	Timezone  string   `json:"timezone"`
	DurationS int      `json:"duration_s"`
}

func windowFormOf(w maint.Window) windowForm {
	f := windowForm{Name: w.Name, Hosts: w.Hosts, Daily: w.Daily, Timezone: w.Timezone, DurationS: int(w.Duration / time.Second)}
	if f.Hosts == nil {
		f.Hosts = []string{}
	}
	return f
}

// handleWindowPut adds a maintenance window or replaces the one with that
// name.
func (srv *Server) handleWindowPut(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.maintenanceSource(), "maintenance windows", "obs-beacond.yaml") {
		return
	}
	var f windowForm
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&f); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	nw := maint.Window{Name: r.PathValue("name"), Hosts: f.Hosts, Daily: strings.TrimSpace(f.Daily),
		Timezone: strings.TrimSpace(f.Timezone), Duration: time.Duration(f.DurationS) * time.Second}
	srv.maint.mu.Lock()
	defer srv.maint.mu.Unlock()
	raw := slices.Clone(srv.maint.raw)
	verb := "added a maintenance window"
	if i := slices.IndexFunc(raw, func(x maint.Window) bool { return x.Name == nw.Name }); i >= 0 {
		raw[i], verb = nw, "changed a maintenance window"
	} else {
		raw = append(raw, nw)
	}
	known := srv.knownHosts()
	for _, h := range nw.Hosts {
		if !known[h] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("No host called %q.", h)})
			return
		}
	}
	raw, cur, err := checkUIWindows(raw, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := srv.saveSection(sectionMaintenance, windowsDoc{raw}, srv.who(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.maint.raw, srv.maint.cur = raw, cur
	tz := nw.Timezone
	if tz == "" {
		tz = "UTC"
	}
	srv.audit(srv.who(r), verb, fmt.Sprintf("%s: daily %s %s for %s, %s", nw.Name, nw.Daily, tz, nw.Duration, strings.Join(nw.Hosts, ", ")))
	writeJSON(w, http.StatusOK, windowFormOf(nw))
}

func (srv *Server) handleWindowDelete(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.maintenanceSource(), "maintenance windows", "obs-beacond.yaml") {
		return
	}
	name := r.PathValue("name")
	srv.maint.mu.Lock()
	defer srv.maint.mu.Unlock()
	i := slices.IndexFunc(srv.maint.raw, func(x maint.Window) bool { return x.Name == name })
	if i < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No maintenance window called " + name + "."})
		return
	}
	raw := slices.Delete(slices.Clone(srv.maint.raw), i, i+1)
	cur := slices.DeleteFunc(slices.Clone(srv.maint.cur), func(x maint.Window) bool { return x.Name == name })
	if err := srv.saveSection(sectionMaintenance, windowsDoc{raw}, srv.who(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	srv.maint.raw, srv.maint.cur = raw, cur
	srv.audit(srv.who(r), "removed a maintenance window", name)
	w.WriteHeader(http.StatusNoContent)
}

// handleExport downloads a section as YAML: what the UI holds, or what the
// config file says. Checks export as a whole checks.yaml; windows as the
// `maintenance:` block of obs-beacond.yaml.
func (srv *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var doc any
	switch r.PathValue("section") {
	case sectionChecks:
		srv.checks.mu.Lock()
		if srv.checksSource() == sourceUI {
			doc = Checks{Checks: srv.checks.raw}
		} else if srv.checks.cur != nil {
			doc = Checks{Checks: srv.checks.cur.Checks}
		}
		srv.checks.mu.Unlock()
	case sectionMaintenance:
		srv.maint.mu.RLock()
		if srv.maintenanceSource() == sourceUI {
			doc = windowsDoc{srv.maint.raw}
		} else {
			doc = windowsDoc{srv.maint.cur}
		}
		srv.maint.mu.RUnlock()
	case sectionChannels:
		doc = srv.exportChannels()
	case sectionExpected:
		doc = srv.exportExpected()
	case sectionMaps:
		doc = srv.mapFixesNow()
	default:
		http.NotFound(w, r)
		return
	}
	if doc == nil {
		doc = Checks{}
	}
	b, err := compactYAML(doc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("{}")) {
		if r.PathValue("section") == sectionExpected {
			b = []byte("hosts: {}\n")
		} else {
			b = []byte(r.PathValue("section") + ": []\n")
		}
	}
	if r.PathValue("section") == sectionChannels {
		b = append([]byte("# Secrets aren't exported. Importing this keeps the ones already set\n# for each channel with the same id and type.\n"), b...)
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.yaml"`, r.PathValue("section")))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// handleImport replaces a UI-edited section with an uploaded YAML document,
// all or nothing.
func (srv *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	if section != sectionChecks && section != sectionMaintenance && section != sectionChannels && section != sectionExpected {
		http.NotFound(w, r)
		return
	}
	if section == sectionChecks && srv.readOnly(w, srv.checksSource(), "checks", srv.cfg.Checks) ||
		section == sectionMaintenance && srv.readOnly(w, srv.maintenanceSource(), "maintenance windows", "obs-beacond.yaml") ||
		section == sectionChannels && srv.readOnly(w, srv.channelsSource(), "alert channels", "obs-beacond.yaml") ||
		section == sectionExpected && srv.readOnly(w, srv.expectedSource(), "expected containers and services", srv.cfg.Expected) {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSettingsYAML))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "That's more than 1 MiB."})
		return
	}
	known := srv.knownHosts()
	who := srv.who(r)
	var n int
	switch section {
	case sectionChecks:
		raw, cur, err := parseUIChecks(b, known)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Not imported: " + err.Error()})
			return
		}
		srv.checks.mu.Lock()
		err = srv.saveSection(sectionChecks, Checks{Checks: raw}, who)
		if err == nil {
			srv.checks.raw, srv.checks.cur = raw, cur
		}
		srv.checks.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		n = len(raw)
		srv.audit(who, "imported the checks", plural(n, "check", "checks"))
	case sectionMaintenance:
		raw, cur, err := parseUIWindows(b, known)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Not imported: " + err.Error()})
			return
		}
		srv.maint.mu.Lock()
		err = srv.saveSection(sectionMaintenance, windowsDoc{raw}, who)
		if err == nil {
			srv.maint.raw, srv.maint.cur = raw, cur
		}
		srv.maint.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		n = len(raw)
		srv.audit(who, "imported the maintenance windows", plural(n, "window", "windows"))
	case sectionChannels:
		var err error
		if n, err = srv.importChannels(b, who); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errStore) {
				code = http.StatusInternalServerError
			}
			writeJSON(w, code, map[string]string{"error": "Not imported: " + err.Error()})
			return
		}
		srv.audit(who, "imported the alert channels", plural(n, "channel", "channels"))
	case sectionExpected:
		var err error
		if n, err = srv.importExpected(b, who); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Not imported: " + err.Error()})
			return
		}
		srv.audit(who, "imported what should run where", plural(n, "host", "hosts"))
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}
