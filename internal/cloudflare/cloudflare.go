// Package cloudflare reads what Cloudflare knows about our sites with a
// read-only API token: load balancer pools and their origins' health, each
// zone's SSL mode and edge certificates, and Cloudflare's own status page.
// It only reads; obs-beacond turns what it finds into conditions.
package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is the `cloudflare` section of obs-beacond.yaml.
type Config struct {
	TokenFile        string            `yaml:"token_file"`         // read-only API token, re-read on every poll
	Every            time.Duration     `yaml:"every"`              // default 5m
	Zones            []string          `yaml:"zones"`              // empty: every zone the token can read
	SSLMode          string            `yaml:"ssl_mode"`           // what every zone should use; default strict
	SSLModeZones     map[string]string `yaml:"ssl_mode_zones"`     // zones known to use another mode
	CertWarnDays     int               `yaml:"cert_warn_days"`     // default 14
	CertCriticalDays int               `yaml:"cert_critical_days"` // default 3

	// Traffic over the last 15 minutes, from Cloudflare's request analytics.
	ErrorWarnPct     float64 `yaml:"error_warn_pct"`     // share of requests answered 5xx; default 5
	ErrorCriticalPct float64 `yaml:"error_critical_pct"` // default 25
	ErrorMin         int     `yaml:"error_min"`          // fewer 5xx than this never alerts; default 20
	BlockMin         int     `yaml:"block_min"`          // blocked requests before it can be a spike; default 100
	BlockFactor      float64 `yaml:"block_factor"`       // times the last day's average; default 10

	// StandIn is a stand-in for Cloudflare on this machine, such as the
	// fleet simulator's (scripts/fleet): http on loopback only. The token
	// and every request go there instead, the status page too.
	StandIn string `yaml:"stand_in"`

	API       string `yaml:"-"` // tests point these at a fake
	StatusURL string `yaml:"-"`
}

// WantSSL is the SSL mode zone should use.
func (c *Config) WantSSL(zone string) string {
	if m, ok := c.SSLModeZones[zone]; ok {
		return m
	}
	return c.SSLMode
}

// Defaults fills in what was left out.
func (c *Config) Defaults() {
	if c.Every == 0 {
		c.Every = 5 * time.Minute
	}
	if c.SSLMode == "" {
		c.SSLMode = "strict"
	}
	if c.CertWarnDays == 0 {
		c.CertWarnDays = 14
	}
	if c.CertCriticalDays == 0 {
		c.CertCriticalDays = 3
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
	if c.BlockMin == 0 {
		c.BlockMin = 100
	}
	if c.BlockFactor == 0 {
		c.BlockFactor = 10
	}
	if c.API == "" {
		c.API = "https://api.cloudflare.com/client/v4"
	}
	if c.StatusURL == "" {
		c.StatusURL = "https://www.cloudflarestatus.com/api/v2/status.json"
	}
}

// Snapshot is the latest reading. Lists are never nil.
type Snapshot struct {
	Checked      time.Time `json:"checked"`                 // last successful poll; zero before the first
	Error        string    `json:"error,omitempty"`         // the last poll's error, if it failed
	ErrorSince   time.Time `json:"error_since,omitzero"`    // when polls started failing
	TrafficError string    `json:"traffic_error,omitempty"` // analytics failed; everything else is current
	Zones        []Zone    `json:"zones"`
	Pools        []Pool    `json:"pools"`
	Status       Status    `json:"status"`
}

type Zone struct {
	Name    string   `json:"name"`
	SSL     string   `json:"ssl"` // off, flexible, full, strict
	Certs   []Cert   `json:"certs"`
	LBs     []string `json:"load_balancers"` // hostnames with a load balancer
	Traffic *Traffic `json:"traffic,omitempty"`
	Records []Record `json:"-"` // obs-beacond compares these with what was last accepted
	DNS     int      `json:"dns_records"`
}

// Traffic is one zone's requests over a 15-minute window ending a minute
// ago (Cloudflare's analytics lag about that much).
type Traffic struct {
	Requests   int         `json:"requests"`
	Errors     int         `json:"errors"`      // answered 5xx
	ErrorHosts []HostCount `json:"error_hosts"` // where the 5xx were, most first
	Blocked    int         `json:"blocked"`     // blocked or challenged by Cloudflare itself
	BlockedDay int         `json:"blocked_day"` // the 24 hours before the window
}

type HostCount struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

// Record is a DNS record as far as drift is concerned: ids, comments and
// timestamps don't count.
type Record struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
}

// Key identifies the record's whole setting.
func (r Record) Key() string {
	p := ""
	if r.Priority != nil {
		p = fmt.Sprint(*r.Priority)
	}
	return fmt.Sprintf("%s %s %s proxied=%v ttl=%d prio=%s", r.Type, r.Name, r.Content, r.Proxied, r.TTL, p)
}

// String is how a person reads it.
func (r Record) String() string {
	s := r.Type + " " + r.Name + " → " + r.Content
	if r.Priority != nil {
		s += fmt.Sprintf(" (priority %d)", *r.Priority)
	}
	if r.Proxied {
		s += " (proxied)"
	} else if r.TTL > 1 {
		s += fmt.Sprintf(" (TTL %d)", r.TTL)
	}
	return s
}

type Cert struct {
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Hosts   []string  `json:"hosts"`
	Expires time.Time `json:"expires,omitzero"` // the soonest of the pack's certificates
}

type Pool struct {
	Name    string   `json:"name"`
	Enabled bool     `json:"enabled"`
	Healthy bool     `json:"healthy"`
	Origins []Origin `json:"origins"`
}

type Origin struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Enabled bool   `json:"enabled"`
	Healthy bool   `json:"healthy"` // healthy from every region that checks it
	Reason  string `json:"reason,omitempty"`
	Code    int    `json:"code,omitempty"`
	RTT     string `json:"rtt,omitempty"`
}

// Status is Cloudflare's own status page: indicator none, minor, major or critical.
type Status struct {
	Indicator   string `json:"indicator"`
	Description string `json:"description"`
}

// Watcher polls Cloudflare and keeps the latest Snapshot.
type Watcher struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex
	snap   Snapshot
}

// New checks the token file is readable; it is read again on every poll so
// a new token needs no restart.
func New(cfg Config) (*Watcher, error) {
	if cfg.StandIn != "" {
		u, err := url.Parse(cfg.StandIn)
		if err != nil || u.Scheme != "http" || u.Path != "" && u.Path != "/" || !loopback(u.Hostname()) {
			return nil, fmt.Errorf("cloudflare: stand_in %q: must be http://127.0.0.1:port (a stand-in on this machine)", cfg.StandIn)
		}
		cfg.API = "http://" + u.Host
		cfg.StatusURL = cfg.API + "/status.json"
	}
	cfg.Defaults()
	if cfg.TokenFile == "" {
		return nil, errors.New("cloudflare: token_file is required")
	}
	if _, err := readToken(cfg.TokenFile); err != nil {
		return nil, err
	}
	return &Watcher{cfg: cfg, client: &http.Client{Timeout: 20 * time.Second},
		snap: Snapshot{Zones: []Zone{}, Pools: []Pool{}}}, nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// Config returns the settings in use (defaults filled in).
func (w *Watcher) Config() Config { return w.cfg }

func readToken(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("cloudflare token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("cloudflare token %s is empty", p)
	}
	return t, nil
}

// Snapshot returns the latest reading.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

// Set replaces the reading; tests use it to judge a known state.
func (w *Watcher) Set(s Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snap = s
}

// Run polls now and then every cfg.Every until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Every)
	defer t.Stop()
	for {
		w.Poll(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll reads everything once. On failure the previous reading is kept and
// the error recorded, so a short API outage doesn't blank the page.
func (w *Watcher) Poll(ctx context.Context, now time.Time) {
	s, err := w.read(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if w.snap.Error == "" {
			w.snap.ErrorSince = now
		}
		w.snap.Error = err.Error()
		return
	}
	s.Checked = now
	w.snap = s
}

// envelope is the API's wrapper around every answer.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
	Info    *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (w *Watcher) get(ctx context.Context, token, path string) (envelope, error) {
	var env envelope
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.API+path, nil)
	if err != nil {
		return env, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := w.client.Do(req)
	if err != nil {
		// The URL is ours and holds no secret; the token is only in a header.
		return env, fmt.Errorf("cloudflare %s: %w", path, errors.Unwrap(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("cloudflare %s: HTTP %d, unreadable answer", path, resp.StatusCode)
	}
	if !env.Success {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(env.Errors) > 0 {
			msg = fmt.Sprintf("%s (code %d)", env.Errors[0].Message, env.Errors[0].Code)
		}
		return env, fmt.Errorf("cloudflare %s: %s", path, msg)
	}
	return env, nil
}

// list follows the API's pages and decodes every item into out.
func list[T any](ctx context.Context, w *Watcher, token, path string) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; ; page++ {
		env, err := w.get(ctx, token, fmt.Sprintf("%s%spage=%d&per_page=50", path, sep, page))
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(env.Result, &items); err != nil {
			return nil, fmt.Errorf("cloudflare %s: %w", path, err)
		}
		all = append(all, items...)
		if env.Info == nil || page >= env.Info.TotalPages {
			return all, nil
		}
	}
}

func (w *Watcher) read(ctx context.Context) (Snapshot, error) {
	s := Snapshot{Zones: []Zone{}, Pools: []Pool{}}
	token, err := readToken(w.cfg.TokenFile)
	if err != nil {
		return s, err
	}
	zones, err := list[zoneRow](ctx, w, token, "/zones")
	if err != nil {
		return s, err
	}
	want := map[string]bool{}
	for _, z := range w.cfg.Zones {
		want[strings.ToLower(z)] = true
	}
	accounts := map[string]bool{}
	ids := map[string]int{} // zone id → index in s.Zones
	// Zones are read six at a time: one after another, 26 zones took a
	// minute, which left the page empty that long after every restart.
	var todo []zoneRow
	for _, z := range zones {
		if len(want) > 0 && !want[z.Name] {
			continue
		}
		accounts[z.Account.ID] = true
		todo = append(todo, z)
	}
	read := make([]Zone, len(todo))
	errs := make([]error, len(todo))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, z := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			read[i], errs[i] = w.readZone(ctx, token, z)
		}()
	}
	wg.Wait()
	for i, z := range todo {
		if errs[i] != nil {
			return s, errs[i]
		}
		s.Zones = append(s.Zones, read[i])
		ids[z.ID] = len(s.Zones) - 1
	}
	if err := w.traffic(ctx, token, s.Zones, ids, time.Now()); err != nil {
		s.TrafficError = err.Error()
	}

	for acct := range accounts {
		type poolRow struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			Healthy *bool  `json:"healthy"`
			Origins []struct {
				Name    string `json:"name"`
				Address string `json:"address"`
				Enabled bool   `json:"enabled"`
			} `json:"origins"`
		}
		pools, err := list[poolRow](ctx, w, token, "/accounts/"+acct+"/load_balancers/pools")
		if err != nil {
			return s, err
		}
		for _, p := range pools {
			pool := Pool{Name: p.Name, Enabled: p.Enabled, Healthy: p.Healthy == nil || *p.Healthy, Origins: []Origin{}}
			health, err := w.poolHealth(ctx, token, acct, p.ID)
			if err != nil {
				return s, err
			}
			for _, o := range p.Origins {
				origin := Origin{Name: o.Name, Address: o.Address, Enabled: o.Enabled, Healthy: true}
				if h, ok := health[o.Address]; ok {
					origin.Healthy, origin.Reason, origin.Code, origin.RTT = h.Healthy, h.Reason, h.Code, h.RTT
				}
				pool.Origins = append(pool.Origins, origin)
			}
			s.Pools = append(s.Pools, pool)
		}
	}
	sort.Slice(s.Pools, func(i, j int) bool { return s.Pools[i].Name < s.Pools[j].Name })
	sort.Slice(s.Zones, func(i, j int) bool { return s.Zones[i].Name < s.Zones[j].Name })

	// The status page is public and separate: failing to read it isn't a
	// reason to drop everything else.
	s.Status = w.status(ctx)
	return s, nil
}

// poolHealth folds every region's view of each origin into one: an origin
// is healthy only if every region that checks it says so.
func (w *Watcher) poolHealth(ctx context.Context, token, acct, pool string) (map[string]Origin, error) {
	env, err := w.get(ctx, token, "/accounts/"+acct+"/load_balancers/pools/"+url.PathEscape(pool)+"/health")
	if err != nil {
		return nil, err
	}
	var h struct {
		PopHealth map[string]struct {
			Origins []map[string]struct {
				Healthy       bool   `json:"healthy"`
				RTT           string `json:"rtt"`
				FailureReason string `json:"failure_reason"`
				ResponseCode  int    `json:"response_code"`
			} `json:"origins"`
		} `json:"pop_health"`
	}
	if err := json.Unmarshal(env.Result, &h); err != nil {
		return nil, fmt.Errorf("cloudflare pool health: %w", err)
	}
	out := map[string]Origin{}
	regions := make([]string, 0, len(h.PopHealth))
	for r := range h.PopHealth {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	for _, r := range regions {
		for _, m := range h.PopHealth[r].Origins {
			for addr, o := range m {
				cur, seen := out[addr]
				if !seen {
					cur = Origin{Healthy: true, RTT: o.RTT, Code: o.ResponseCode}
				}
				if !o.Healthy && cur.Healthy {
					cur.Healthy, cur.Code, cur.RTT = false, o.ResponseCode, o.RTT
					cur.Reason = o.FailureReason + " (seen from " + r + ")"
				}
				out[addr] = cur
			}
		}
	}
	return out, nil
}

func (w *Watcher) status(ctx context.Context) Status {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.StatusURL, nil)
	if err != nil {
		return Status{Indicator: "unknown", Description: err.Error()}
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return Status{Indicator: "unknown", Description: "status page unreachable"}
	}
	defer resp.Body.Close()
	var v struct {
		Status Status `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil || v.Status.Indicator == "" {
		return Status{Indicator: "unknown", Description: "status page unreadable"}
	}
	return v.Status
}

// traffic reads every zone's last 15 minutes (and the day before, for the
// blocked baseline) from the GraphQL analytics, ten zones per query. Only
// request analytics: the firewall events dataset needs a paid plan, so
// "blocked" is any request Cloudflare's security refused itself (see
// blockingActions).
func (w *Watcher) traffic(ctx context.Context, token string, zones []Zone, ids map[string]int, now time.Time) error {
	end := now.UTC().Truncate(time.Minute).Add(-time.Minute)
	start, dayStart := end.Add(-15*time.Minute), end.Add(-15*time.Minute-24*time.Hour)
	const q = `query($tags:[string!], $a:Time!, $b:Time!, $c:Time!, $blocking:[string!]) { viewer { zones(filter:{zoneTag_in:$tags}) { zoneTag
  win: httpRequestsAdaptiveGroups(limit:5000, filter:{datetime_geq:$a, datetime_lt:$b}) { count dimensions { edgeResponseStatus securityAction clientRequestHTTPHost } }
  day: httpRequestsAdaptiveGroups(limit:5000, filter:{datetime_geq:$c, datetime_lt:$a, securityAction_in:$blocking}) { count }
} } }`
	tags := make([]string, 0, len(ids))
	for id := range ids {
		tags = append(tags, id)
	}
	sort.Strings(tags)
	for i := 0; i < len(tags); i += 10 {
		chunk := tags[i:min(i+10, len(tags))]
		body, _ := json.Marshal(map[string]any{"query": q, "variables": map[string]any{
			"tags": chunk, "a": start.Format(time.RFC3339), "b": end.Format(time.RFC3339), "c": dayStart.Format(time.RFC3339),
			"blocking": blockingActions}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.API+"/graphql", strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.client.Do(req)
		if err != nil {
			return fmt.Errorf("cloudflare analytics: %w", errors.Unwrap(err))
		}
		var out struct {
			Data struct {
				Viewer struct {
					Zones []struct {
						ZoneTag string `json:"zoneTag"`
						Win     []struct {
							Count int `json:"count"`
							Dim   struct {
								Status int    `json:"edgeResponseStatus"`
								Action string `json:"securityAction"`
								Host   string `json:"clientRequestHTTPHost"`
							} `json:"dimensions"`
						} `json:"win"`
						Day []struct {
							Count int `json:"count"`
						} `json:"day"`
					} `json:"zones"`
				} `json:"viewer"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("cloudflare analytics: HTTP %d, unreadable answer", resp.StatusCode)
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("cloudflare analytics: %s", out.Errors[0].Message)
		}
		for _, z := range out.Data.Viewer.Zones {
			idx, ok := ids[z.ZoneTag]
			if !ok {
				continue
			}
			t := &Traffic{ErrorHosts: []HostCount{}}
			hosts := map[string]int{}
			for _, g := range z.Win {
				t.Requests += g.Count
				if g.Dim.Status >= 500 {
					t.Errors += g.Count
					hosts[g.Dim.Host] += g.Count
				}
				if blocking(g.Dim.Action) {
					t.Blocked += g.Count
				}
			}
			for _, g := range z.Day {
				t.BlockedDay += g.Count
			}
			for h, n := range hosts {
				t.ErrorHosts = append(t.ErrorHosts, HostCount{h, n})
			}
			sort.Slice(t.ErrorHosts, func(i, j int) bool { return t.ErrorHosts[i].Count > t.ErrorHosts[j].Count })
			zones[idx].Traffic = t
		}
	}
	return nil
}

// blockingActions are the security actions that refused a request at the
// edge: a block, a challenge page, or a closed connection. It is a list of
// what counts, not of what doesn't, because Cloudflare keeps adding actions
// that let the request through: AI Labyrinth's link_maze_injected (decoy links
// added to a page that was served), challenge_solved and the other *_solved
// and *_bypassed follow-ups, rewrite. Counting those turned a crawler being
// fed decoy links into "Cloudflare is blocking a lot of traffic".
var blockingActions = []string{
	"block",
	"challenge", "jschallenge", "managed_challenge",
	"connection_close", "force_connection_close",
}

// blocking is a security action that answered the request at the edge.
func blocking(action string) bool {
	return slices.Contains(blockingActions, action)
}

type zoneRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

// readZone reads one zone's SSL mode, certificates, load balancers and DNS.
func (w *Watcher) readZone(ctx context.Context, token string, z zoneRow) (Zone, error) {
	zone := Zone{Name: z.Name, Certs: []Cert{}, LBs: []string{}}

	env, err := w.get(ctx, token, "/zones/"+z.ID+"/settings/ssl")
	if err != nil {
		return zone, err
	}
	var ssl struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(env.Result, &ssl)
	zone.SSL = ssl.Value

	type packRow struct {
		Type         string   `json:"type"`
		Status       string   `json:"status"`
		Hosts        []string `json:"hosts"`
		Certificates []struct {
			ExpiresOn time.Time `json:"expires_on"`
		} `json:"certificates"`
	}
	packs, err := list[packRow](ctx, w, token, "/zones/"+z.ID+"/ssl/certificate_packs")
	if err != nil {
		return zone, err
	}
	for _, p := range packs {
		c := Cert{Type: p.Type, Status: p.Status, Hosts: p.Hosts}
		for _, x := range p.Certificates {
			if !x.ExpiresOn.IsZero() && (c.Expires.IsZero() || x.ExpiresOn.Before(c.Expires)) {
				c.Expires = x.ExpiresOn
			}
		}
		if c.Hosts == nil {
			c.Hosts = []string{}
		}
		zone.Certs = append(zone.Certs, c)
	}

	type lbRow struct {
		Name string `json:"name"`
	}
	lbs, err := list[lbRow](ctx, w, token, "/zones/"+z.ID+"/load_balancers")
	if err != nil {
		return zone, err
	}
	for _, lb := range lbs {
		zone.LBs = append(zone.LBs, lb.Name)
	}

	if zone.Records, err = list[Record](ctx, w, token, "/zones/"+z.ID+"/dns_records"); err != nil {
		return zone, err
	}
	if zone.Records == nil {
		zone.Records = []Record{}
	}
	zone.DNS = len(zone.Records)
	return zone, nil
}
