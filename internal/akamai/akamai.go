// Package akamai reads what Akamai knows about our sites, with a read-only
// API client (docs/akamai.md): Global Traffic Management domains, their
// properties and each datacenter's servers, alive or not. It only reads
// (GET, EdgeGrid-signed); obs-beacond turns what it finds into conditions.
package akamai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Config is the `akamai` section of obs-beacond.yaml.
type Config struct {
	Edgerc  string        `yaml:"edgerc_file"` // the API client's .edgerc, re-read on every poll
	Section string        `yaml:"section"`     // default "default"
	Every   time.Duration `yaml:"every"`       // default 5m
	Domains []string      `yaml:"domains"`     // GTM domains; empty: every one the client can read
	// Contracts whose CPS enrollments are read; empty: every contract
	// Property Manager lists, else none named.
	Contracts []string `yaml:"contracts"`

	// Traffic (step 5): a CP code's share of edge hits answered 5xx, as
	// Cloudflare's: warning and critical percentages, and fewer 5xx than
	// ErrorMin never alert. Defaults 5, 25, 20.
	ErrorWarnPct     float64 `yaml:"error_warn_pct"`
	ErrorCriticalPct float64 `yaml:"error_critical_pct"`
	ErrorMin         int     `yaml:"error_min"`

	API       string `yaml:"-"` // tests point this at a fake: "http://127.0.0.1:…"
	StatusURL string `yaml:"-"` // Akamai's status page summary; tests point it at a fake
}

// Defaults fills in what was left out.
func (c *Config) Defaults() {
	if c.Section == "" {
		c.Section = "default"
	}
	if c.Every == 0 {
		c.Every = 5 * time.Minute
	}
	if c.ErrorWarnPct == 0 {
		c.ErrorWarnPct = 5
	}
	if c.ErrorCriticalPct == 0 {
		c.ErrorCriticalPct = 25
	}
	if c.ErrorMin == 0 {
		c.ErrorMin = 20
	}
	if c.StatusURL == "" && c.API != "" {
		c.StatusURL = c.API + "/status.json" // a fake's
	}
	if c.StatusURL == "" {
		c.StatusURL = "https://www.akamaistatus.com/api/v2/status.json"
	}
}

// Snapshot is one reading.
type Snapshot struct {
	Checked    time.Time `json:"checked"` // the last good reading; zero before the first
	Error      string    `json:"error,omitempty"`
	ErrorSince time.Time `json:"error_since"`
	Domains    []Domain  `json:"domains"`
	// CPS enrollments (step 2); nil when CPS isn't read. Read every
	// cpsEvery, not every poll: CPSAt is when they were.
	Enrollments []Enrollment `json:"enrollments"`
	CPSAt       time.Time    `json:"cps_at"`
	CPSError    string       `json:"cps_error,omitempty"` // the last CPS reading failed; Enrollments are the one before
	// Property Manager properties (step 3), on the same pacing as CPS.
	PMProperties []PMProperty `json:"pm_properties"`
	PAPIAt       time.Time    `json:"papi_at"`
	PAPIError    string       `json:"papi_error,omitempty"`
	// Traffic (step 5): each CP code's edge hits and 5xx over a 5-minute
	// window, read every poll; TrafficError when the last read failed.
	Traffic      []CPTraffic `json:"traffic"`
	TrafficStart time.Time   `json:"traffic_start"`
	TrafficEnd   time.Time   `json:"traffic_end"`
	TrafficError string      `json:"traffic_error,omitempty"`
	// Akamai's status page: shown, never alerting.
	Status Status `json:"status"`
	// Edge DNS zones (step 4), on the same pacing.
	DNSZones []DNSZone `json:"dns_zones"`
	DNSAt    time.Time `json:"dns_at"`
	DNSError string    `json:"dns_error,omitempty"`
	Absent   []string  `json:"absent,omitempty"` // products the client can't read ("gtm"): not in the account, or not granted
}

// Domain is a GTM domain.
type Domain struct {
	Name        string     `json:"name"`
	Type        string     `json:"type,omitempty"`        // basic, full, weighted, static, failover-only
	Propagation string     `json:"propagation,omitempty"` // COMPLETE, PENDING, DENIED
	Since       time.Time  `json:"since"`                 // when the propagation status was set
	Message     string     `json:"message,omitempty"`
	Properties  []Property `json:"properties"`
	Error       string     `json:"error,omitempty"`
}

// Property is a GTM property: a name GTM answers for, from its datacenters.
type Property struct {
	Name    string    `json:"name"`
	Type    string    `json:"type,omitempty"` // failover, weighted-round-robin, performance, …
	Targets []Target  `json:"targets"`
	At      time.Time `json:"at"` // the availability report's time; zero: not read
	Error   string    `json:"error,omitempty"`
}

// Target is one datacenter's servers for a property.
type Target struct {
	Datacenter string   `json:"datacenter"` // its nickname
	ID         int      `json:"id"`
	Enabled    bool     `json:"enabled"`
	Weight     float64  `json:"weight"`
	Servers    []Server `json:"servers"`
}

// Server is one address GTM hands out, as its liveness tests see it.
type Server struct {
	IP        string  `json:"ip"`
	Alive     bool    `json:"alive"`
	HandedOut bool    `json:"handed_out"`
	Score     float64 `json:"score"`
	Known     bool    `json:"known"` // the report had it; false: configured but not reported yet
}

// Live says whether the target has a live server.
func (t Target) Live() bool {
	return slices.ContainsFunc(t.Servers, func(s Server) bool { return s.Alive })
}

// Watcher polls Akamai and keeps the latest reading.
type Watcher struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex
	snap   Snapshot
}

// New checks the config; the .edgerc is read on every poll.
func New(cfg Config) (*Watcher, error) {
	cfg.Defaults()
	if cfg.Edgerc == "" {
		return nil, errors.New("akamai: edgerc_file is required")
	}
	return &Watcher{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (w *Watcher) Config() Config { return w.cfg }

func (w *Watcher) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

// Set puts a reading in place (another copy of obs-beacond took it).
func (w *Watcher) Set(s Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snap = s
}

// Poll reads everything once.
func (w *Watcher) Poll(ctx context.Context, now time.Time) {
	s, err := w.read(ctx, now)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if w.snap.Error == "" {
			w.snap.ErrorSince = now
		}
		w.snap.Error = err.Error()
		return // the last good reading stays
	}
	w.snap = s
}

// errAbsent is a product the client can't read: 403 or 404.
var errAbsent = errors.New("not in this account, or not granted")

// errNotAcceptable is a media type the API doesn't serve (406, 415).
var errNotAcceptable = errors.New("media type not accepted")

func (w *Watcher) get(ctx context.Context, c Creds, path string, v any) error {
	return w.getAccept(ctx, c, path, "application/json", v)
}

func (w *Watcher) getAccept(ctx context.Context, c Creds, path, accept string, v any) error {
	return w.do(ctx, c, http.MethodGet, path, accept, nil, v)
}

// do sends one request. Everything is a GET but the traffic report's
// query, which the Reporting API only takes as a POST; it changes
// nothing, and no other POST can be sent.
func (w *Watcher) do(ctx context.Context, c Creds, method, path, accept string, body []byte, v any) error {
	if method != http.MethodGet && !(method == http.MethodPost && path == trafficPath) {
		return fmt.Errorf("akamai: %s %s refused: only reads are sent", method, path)
	}
	base := "https://" + c.Host
	switch {
	case w.cfg.API != "":
		base = w.cfg.API
	case c.Plain:
		base = "http://" + c.Host
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Signed as an https request to the client's host, wherever it goes.
	u := *req.URL
	u.Scheme, u.Host = "https", c.Host
	sreq := &http.Request{Method: req.Method, URL: &u, Header: http.Header{}}
	c.Sign(sreq, body, time.Now())
	req.Header.Set("Authorization", sreq.Header.Get("Authorization"))
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		return errAbsent
	case resp.StatusCode == http.StatusNotAcceptable || resp.StatusCode == http.StatusUnsupportedMediaType:
		return errNotAcceptable
	case resp.StatusCode != http.StatusOK:
		var p struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = json.Unmarshal(b, &p)
		msg := strings.TrimSpace(p.Title + ": " + p.Detail)
		if p.Title == "" {
			msg = resp.Status
		}
		return fmt.Errorf("%s: %s", path, strings.Trim(msg, ": "))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v)
}

// GTM's answers: the fields we use (field names from Akamai's GTM Config
// and GTM Reporting API references).
type gtmDomains struct {
	Items []struct {
		Name string `json:"name"`
	} `json:"items"`
}

type gtmDomain struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Datacenters []struct {
		DatacenterID int    `json:"datacenterId"`
		Nickname     string `json:"nickname"`
	} `json:"datacenters"`
	Properties []struct {
		Name           string `json:"name"`
		Type           string `json:"type"`
		TrafficTargets []struct {
			DatacenterID int      `json:"datacenterId"`
			Enabled      bool     `json:"enabled"`
			Weight       float64  `json:"weight"`
			Servers      []string `json:"servers"`
		} `json:"trafficTargets"`
	} `json:"properties"`
}

type gtmStatus struct {
	PropagationStatus     string    `json:"propagationStatus"`
	PropagationStatusDate time.Time `json:"propagationStatusDate"`
	Message               string    `json:"message"`
}

type ipAvailability struct {
	DataRows []struct {
		Timestamp   time.Time `json:"timestamp"`
		Datacenters []struct {
			DatacenterID int    `json:"datacenterId"`
			Nickname     string `json:"nickname"`
			IPs          []struct {
				IP        string  `json:"ip"`
				HandedOut bool    `json:"handedOut"`
				Score     float64 `json:"score"`
				Alive     bool    `json:"alive"`
			} `json:"IPs"`
		} `json:"datacenters"`
	} `json:"dataRows"`
}

// reportsAtOnce is how many availability reports are read at a time.
const reportsAtOnce = 4

func (w *Watcher) read(ctx context.Context, now time.Time) (Snapshot, error) {
	c, err := ReadEdgerc(w.cfg.Edgerc, w.cfg.Section)
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{Checked: now, Domains: []Domain{}}
	names := w.cfg.Domains
	if len(names) == 0 {
		var ds gtmDomains
		switch err := w.get(ctx, c, "/config-gtm/v1/domains", &ds); {
		case errors.Is(err, errAbsent):
			s.Absent = append(s.Absent, "gtm")
		case err != nil:
			return Snapshot{}, err
		}
		for _, d := range ds.Items {
			names = append(names, d.Name)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		s.Domains = append(s.Domains, w.readDomain(ctx, c, n))
	}
	prev := w.Snapshot()
	switch t, start, end, err := w.readTraffic(ctx, c); {
	case errors.Is(err, errAbsent):
		s.Absent = append(s.Absent, "traffic")
	case err != nil: // the last numbers stay, marked
		s.Traffic, s.TrafficStart, s.TrafficEnd, s.TrafficError = prev.Traffic, prev.TrafficStart, prev.TrafficEnd, err.Error()
	default:
		s.Traffic, s.TrafficStart, s.TrafficEnd = t, start, end
	}
	s.Status = w.status(ctx, c)
	if !prev.CPSAt.IsZero() && now.Sub(prev.CPSAt) < slowEvery {
		s.Enrollments, s.CPSAt, s.CPSError = prev.Enrollments, prev.CPSAt, prev.CPSError
		if slices.Contains(prev.Absent, "cps") {
			s.Absent = append(s.Absent, "cps")
		}
	} else {
		switch es, err := w.readCPS(ctx, c); {
		case errors.Is(err, errAbsent):
			s.Absent = append(s.Absent, "cps")
			s.CPSAt = now
		case err != nil: // GTM's reading still counts; the last enrollments stay
			s.Enrollments, s.CPSError = prev.Enrollments, err.Error()
		default:
			s.Enrollments, s.CPSAt = es, now
		}
	}
	if !prev.PAPIAt.IsZero() && now.Sub(prev.PAPIAt) < slowEvery {
		s.PMProperties, s.PAPIAt, s.PAPIError = prev.PMProperties, prev.PAPIAt, prev.PAPIError
		if slices.Contains(prev.Absent, "papi") {
			s.Absent = append(s.Absent, "papi")
		}
	} else {
		switch ps, err := w.readPAPI(ctx, c, now, prev.PMProperties); {
		case errors.Is(err, errAbsent):
			s.Absent = append(s.Absent, "papi")
			s.PAPIAt = now
		case err != nil:
			s.PMProperties, s.PAPIError = prev.PMProperties, err.Error()
		default:
			s.PMProperties, s.PAPIAt = ps, now
		}
	}
	if !prev.DNSAt.IsZero() && now.Sub(prev.DNSAt) < slowEvery {
		s.DNSZones, s.DNSAt, s.DNSError = prev.DNSZones, prev.DNSAt, prev.DNSError
		if slices.Contains(prev.Absent, "dns") {
			s.Absent = append(s.Absent, "dns")
		}
	} else {
		switch zs, err := w.readDNS(ctx, c, prev.DNSZones); {
		case errors.Is(err, errAbsent):
			s.Absent = append(s.Absent, "dns")
			s.DNSAt = now
		case err != nil:
			s.DNSZones, s.DNSError = prev.DNSZones, err.Error()
		default:
			s.DNSZones, s.DNSAt = zs, now
		}
	}
	return s, nil
}

// slowEvery is how often CPS, Property Manager and Edge DNS are read: they change
// slowly, and their APIs' rate limits are per account (docs/akamai.md,
// Scale).
const slowEvery = 15 * time.Minute

func (w *Watcher) readDomain(ctx context.Context, c Creds, name string) Domain {
	d := Domain{Name: name, Properties: []Property{}}
	esc := url.PathEscape(name)
	var g gtmDomain
	if err := w.get(ctx, c, "/config-gtm/v1/domains/"+esc, &g); err != nil {
		d.Error = err.Error()
		return d
	}
	d.Type = g.Type
	var st gtmStatus
	if err := w.get(ctx, c, "/config-gtm/v1/domains/"+esc+"/status/current", &st); err == nil {
		d.Propagation, d.Since, d.Message = st.PropagationStatus, st.PropagationStatusDate, st.Message
	}
	dcName := map[int]string{}
	for _, dc := range g.Datacenters {
		dcName[dc.DatacenterID] = dc.Nickname
	}
	d.Properties = make([]Property, len(g.Properties))
	var wg sync.WaitGroup
	sem := make(chan struct{}, reportsAtOnce)
	for i, gp := range g.Properties {
		p := Property{Name: gp.Name, Type: gp.Type, Targets: []Target{}}
		for _, t := range gp.TrafficTargets {
			tg := Target{Datacenter: dcName[t.DatacenterID], ID: t.DatacenterID, Enabled: t.Enabled, Weight: t.Weight, Servers: []Server{}}
			if tg.Datacenter == "" {
				tg.Datacenter = fmt.Sprintf("datacenter %d", t.DatacenterID)
			}
			for _, ip := range t.Servers {
				tg.Servers = append(tg.Servers, Server{IP: ip})
			}
			p.Targets = append(p.Targets, tg)
		}
		d.Properties[i] = p
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			w.availability(ctx, c, name, &d.Properties[i])
		}()
	}
	wg.Wait()
	for i := range d.Properties {
		slices.SortFunc(d.Properties[i].Targets, func(a, b Target) int { return strings.Compare(a.Datacenter, b.Datacenter) })
	}
	slices.SortFunc(d.Properties, func(a, b Property) int { return strings.Compare(a.Name, b.Name) })
	return d
}

// availability fills in each server's liveness from the most recent
// IP availability report.
func (w *Watcher) availability(ctx context.Context, c Creds, domain string, p *Property) {
	var r ipAvailability
	path := "/gtm-api/v1/reports/ip-availability/domains/" + url.PathEscape(domain) + "/properties/" + url.PathEscape(p.Name) + "?mostRecent=true"
	if err := w.get(ctx, c, path, &r); err != nil {
		p.Error = err.Error()
		return
	}
	if len(r.DataRows) == 0 {
		return
	}
	row := r.DataRows[len(r.DataRows)-1]
	p.At = row.Timestamp
	for _, dc := range row.Datacenters {
		for ti := range p.Targets {
			t := &p.Targets[ti]
			if t.ID != dc.DatacenterID {
				continue
			}
			for _, ip := range dc.IPs {
				i := slices.IndexFunc(t.Servers, func(s Server) bool { return s.IP == ip.IP })
				if i < 0 {
					t.Servers = append(t.Servers, Server{IP: ip.IP})
					i = len(t.Servers) - 1
				}
				t.Servers[i].Alive, t.Servers[i].HandedOut, t.Servers[i].Score, t.Servers[i].Known = ip.Alive, ip.HandedOut, ip.Score, true
			}
		}
	}
}
