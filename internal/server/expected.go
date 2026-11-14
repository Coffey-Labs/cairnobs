package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Expected is what should run where. See docs/formats.md.
type Expected struct {
	Hosts    map[string]ExpectedHost    `yaml:"hosts"`
	CIJobs   []string                   `yaml:"ci_jobs"` // name patterns: counted, never listed or alerted on
	Clusters map[string]ExpectedCluster `yaml:"clusters,omitempty"`
}

// ExpectedCluster is what's adopted in one cluster (docs/cluster-watcher.md,
// What pages, and who decides): whole namespaces, and single workloads.
// Adopted ones page; the rest only warn on the pages.
type ExpectedCluster struct {
	Namespaces map[string]string  `yaml:"namespaces,omitempty"` // namespace → page, warn or off
	Workloads  []ExpectedWorkload `yaml:"workloads,omitempty"`
}

// ExpectedWorkload is one adopted workload: "shop/deployment/web", or
// "vms/vm/db1" for a VM. Alert is page (the default), warn or off.
// Stopped marks a VM that's stopped on purpose; otherwise an adopted VM
// should be running.
type ExpectedWorkload struct {
	Name    string `yaml:"name" json:"name"`
	Alert   string `yaml:"alert,omitempty" json:"alert"`
	Stopped bool   `yaml:"stopped,omitempty" json:"stopped"`
}

// k8sName is a Kubernetes namespace or object name, and workloadKey an
// adopted workload's namespace/kind/name.
var (
	k8sName     = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	workloadKey = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)/([a-z0-9]{1,63})/([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?)$`)
)

func checkAlert(a string, empty bool) error {
	switch a {
	case report.AlertPage, report.AlertWarn, report.AlertOff:
		return nil
	case "":
		if empty {
			return nil
		}
	}
	return fmt.Errorf("alert %q isn't page, warn or off", a)
}

func (ec ExpectedCluster) check() error {
	for ns, a := range ec.Namespaces {
		if !k8sName.MatchString(ns) || len(ns) > 63 {
			return fmt.Errorf("%q isn't a namespace name", ns)
		}
		if err := checkAlert(a, false); err != nil {
			return fmt.Errorf("namespace %s: %w", ns, err)
		}
	}
	seen := map[string]bool{}
	for _, w := range ec.Workloads {
		m := workloadKey.FindStringSubmatch(w.Name)
		if m == nil {
			return fmt.Errorf("%q isn't namespace/kind/name, as in shop/deployment/web", w.Name)
		}
		if seen[w.Name] {
			return fmt.Errorf("%s is listed twice", w.Name)
		}
		seen[w.Name] = true
		if err := checkAlert(w.Alert, true); err != nil {
			return fmt.Errorf("%s: %w", w.Name, err)
		}
		if w.Stopped && m[3] != "vm" {
			return fmt.Errorf("%s: only a VM can be stopped on purpose", w.Name)
		}
	}
	return nil
}

// ExpectedHost lists container name patterns for one host. "*" matches any
// run of characters, as in a shell.
type ExpectedHost struct {
	Running []string      `yaml:"running"`
	Stopped []StoppedRule `yaml:"stopped"`
	Units   []string      `yaml:"units"` // systemd units that must be active ("inbuxa" means inbuxa.service)
}

// StoppedRule marks containers that are stopped on purpose.
type StoppedRule struct {
	Match string `yaml:"match" json:"match"`
	Note  string `yaml:"note" json:"note"`
}

// LoadExpected reads an expected-containers file. An empty path means no
// expectations: containers are judged on their own state only.
func LoadExpected(p string) (*Expected, error) {
	e := &Expected{}
	if p == "" {
		return e, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if e, err = parseExpected(b); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return e, nil
}

// parseExpected reads and checks an expected.yaml document.
func parseExpected(b []byte) (*Expected, error) {
	e := &Expected{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(e); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for _, pat := range e.CIJobs {
		if _, err := path.Match(pat, ""); err != nil {
			return nil, fmt.Errorf("ci_jobs: bad pattern %q", pat)
		}
	}
	for h, eh := range e.Hosts {
		if err := eh.check(); err != nil {
			return nil, fmt.Errorf("host %s: %w", h, err)
		}
	}
	for c, ec := range e.Clusters {
		if err := ec.check(); err != nil {
			return nil, fmt.Errorf("cluster %s: %w", c, err)
		}
	}
	return e, nil
}

// unitName is what a unit in an expected list may be called.
var unitName = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,200}$`)

func (eh ExpectedHost) check() error {
	for _, pat := range append(append([]string{}, eh.Running...), stoppedPatterns(eh.Stopped)...) {
		if pat == "" {
			return errors.New("an empty container name")
		}
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("bad pattern %q", pat)
		}
	}
	for _, u := range eh.Units {
		if !unitName.MatchString(u) {
			return fmt.Errorf("unit %q isn't a systemd unit name", u)
		}
	}
	return nil
}

func stoppedPatterns(rs []StoppedRule) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Match
	}
	return out
}

func matches(pats []string, name string) (string, bool) {
	for _, p := range pats {
		if ok, _ := path.Match(p, name); ok {
			return p, true
		}
	}
	return "", false
}

// Verdict is Cairn OBS's judgement of one container.
type Verdict struct {
	Severity string `json:"severity"` // good, warning, critical, off
	Word     string `json:"word"`
	Detail   string `json:"detail,omitempty"`
}

// ContainerRow is a container as the UI shows it. Missing containers (expected
// but not there at all) have State "missing" and no ID.
type ContainerRow struct {
	report.Container
	Host     string  `json:"host"`
	Expected bool    `json:"expected"` // named in the host's running list
	Verdict  Verdict `json:"verdict"`
}

// ContainerSummary counts one host's containers, or all of them.
type ContainerSummary struct {
	Running  int `json:"running"`  // up and fine
	Problems int `json:"problems"` // warning or critical
	Stopped  int `json:"stopped"`  // stopped on purpose
	CI       int `json:"ci"`       // CI job containers right now
	Total    int `json:"total"`    // listed rows
}

func (s *ContainerSummary) add(o ContainerSummary) {
	s.Running += o.Running
	s.Problems += o.Problems
	s.Stopped += o.Stopped
	s.CI += o.CI
	s.Total += o.Total
}

// Judge turns one host's container list into rows with verdicts.
func (e *Expected) Judge(host string, list []report.Container) ([]ContainerRow, ContainerSummary) {
	eh, haveHost := e.Hosts[host]
	var rows []ContainerRow
	var sum ContainerSummary
	found := map[string]bool{} // running patterns that matched something
	for _, c := range list {
		if _, ci := matches(e.CIJobs, c.Name); ci {
			if c.State == "running" {
				sum.CI++
			}
			continue
		}
		row := ContainerRow{Container: c, Host: host}
		if rule, ok := stoppedRule(eh.Stopped, c.Name); ok {
			if c.State == "running" {
				row.Verdict = Verdict{"warning", "Running", "listed as stopped on purpose: " + rule.Note}
			} else {
				row.Verdict = Verdict{"off", "Stopped on purpose", rule.Note}
			}
		} else if pat, ok := matches(eh.Running, c.Name); ok {
			found[pat] = true
			row.Expected = true
			row.Verdict = stateVerdict(c, true)
		} else {
			row.Verdict = stateVerdict(c, false)
			if haveHost && c.State == "running" && row.Verdict.Severity == "good" {
				row.Verdict = Verdict{"warning", "Not expected", "running, but not in the expected list for " + host}
			}
		}
		rows = append(rows, row)
	}
	for _, pat := range eh.Running {
		if !found[pat] {
			rows = append(rows, ContainerRow{
				Container: report.Container{Name: pat, State: "missing"},
				Host:      host, Expected: true,
				Verdict: Verdict{"critical", "Missing", "expected on " + host + ", but Docker doesn't list it"},
			})
		}
	}
	for _, r := range rows {
		sum.Total++
		switch r.Verdict.Severity {
		case "good":
			sum.Running++
		case "off":
			if r.Verdict.Word == "Stopped on purpose" {
				sum.Stopped++
			}
		default:
			sum.Problems++
		}
	}
	return rows, sum
}

func stoppedRule(rs []StoppedRule, name string) (StoppedRule, bool) {
	for _, r := range rs {
		if ok, _ := path.Match(r.Match, name); ok {
			return r, true
		}
	}
	return StoppedRule{}, false
}

// stateVerdict judges a container on Docker's own state. expected says
// whether something should be running.
func stateVerdict(c report.Container, expected bool) Verdict {
	switch c.State {
	case "running":
		switch c.Health {
		case "unhealthy":
			return Verdict{"critical", "Unhealthy", c.Status}
		case "starting":
			return Verdict{"warning", "Starting", c.Status}
		}
		return Verdict{"good", "Running", c.Status}
	case "restarting":
		return Verdict{"critical", "Restarting", c.Status}
	case "paused":
		return Verdict{"warning", "Paused", c.Status}
	}
	if expected {
		return Verdict{"critical", "Not running", c.Status}
	}
	return Verdict{"off", "Stopped", c.Status}
}
