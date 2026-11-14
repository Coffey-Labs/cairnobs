package akamai

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake answers as Akamai's APIs do, for the tests and the fleet simulator
// (docs/akamai.md: no account is needed). It checks every request's
// EdgeGrid signature against Creds.
type Fake struct {
	Creds Creds
	mu    sync.Mutex
	gtm   map[string]*FakeDomain
	NoGTM bool // answer GTM with 403, as an account without it does

	cps   map[int]*FakeEnrollment
	NoCPS bool

	pm     map[string]*FakePMProperty
	NoPAPI bool           // answer groups and properties with 403
	calls  map[string]int // requests by path, for the tests

	zones map[string]*FakeZone
	NoDNS bool

	traffic   map[int64][2]int64 // CP code → hits, 5xx
	NoTraffic bool
	// Status is what the fake status page says at /status.json
	// ("none" when empty); it needs no signature, as the real one.
	Status string
}

// SetTraffic sets a CP code's edge hits and how many were 5xx.
func (f *Fake) SetTraffic(cp, hits, errors int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.traffic == nil {
		f.traffic = map[int64][2]int64{}
	}
	f.traffic[cp] = [2]int64{hits, errors}
}

// FakeZone is an Edge DNS zone the fake serves; SetZone gives it a new
// version each time.
type FakeZone struct {
	Type    string // PRIMARY when empty
	Records []RecordSet
	version int
}

// SetZone adds or replaces an Edge DNS zone.
func (f *Fake) SetZone(name string, z FakeZone) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.zones == nil {
		f.zones = map[string]*FakeZone{}
	}
	if o := f.zones[name]; o != nil {
		z.version = o.version + 1
	}
	f.zones[name] = &z
}

// FakePMProperty is a Property Manager property the fake serves.
type FakePMProperty struct {
	Name, Group, Contract       string // Group is its name; ids are made from it
	Latest, Staging, Production int
	Activations                 []FakeActivation
}

// FakeActivation is one activation of a property version.
type FakeActivation struct {
	Version   int
	Network   string // STAGING, PRODUCTION
	Status    string // ACTIVE, FAILED, PENDING, …
	Submitted time.Time
	Note      string
}

// SetPMProperty adds or replaces a Property Manager property, by id.
func (f *Fake) SetPMProperty(id string, p FakePMProperty) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pm == nil {
		f.pm = map[string]*FakePMProperty{}
	}
	f.pm[id] = &p
}

// Calls says how many requests the fake has had for a path.
func (f *Fake) Calls(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

// FakeEnrollment is a CPS enrollment the fake serves. It speaks the
// enrollments v10 and deployment v7 media types only, as an older API
// would, so a client must fall back from newer ones.
type FakeEnrollment struct {
	CN         string
	SANs       []string
	Validation string
	Contract   string
	Deployed   []string // PEM certificates on production; none: not deployed yet
	Waiting    string   // a pending change needing this input ("third-party-csr"); "" for a change in progress, "-" for none
}

// SetEnrollment adds or replaces a CPS enrollment.
func (f *Fake) SetEnrollment(id int, e FakeEnrollment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cps == nil {
		f.cps = map[int]*FakeEnrollment{}
	}
	f.cps[id] = &e
}

// FakeDomain is a GTM domain the fake serves.
type FakeDomain struct {
	Type        string
	Propagation string // COMPLETE when empty
	Since       time.Time
	Datacenters map[int]string // id → nickname
	Properties  []FakeProperty
}

// FakeProperty is a GTM property: per datacenter, its servers, each alive
// or not.
type FakeProperty struct {
	Name    string
	Type    string
	Targets map[int]map[string]bool // datacenter id → server → alive
	Off     map[int]bool            // datacenters whose target is disabled
}

// SetDomain adds or replaces a GTM domain.
func (f *Fake) SetDomain(name string, d FakeDomain) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gtm == nil {
		f.gtm = map[string]*FakeDomain{}
	}
	f.gtm[name] = &d
}

// SetAlive sets one server's liveness.
func (f *Fake) SetAlive(domain, property string, dc int, ip string, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.gtm[domain]; d != nil {
		for _, p := range d.Properties {
			if p.Name == property && p.Targets[dc] != nil {
				p.Targets[dc][ip] = alive
			}
		}
	}
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/status.json" {
		f.mu.Lock()
		st := f.Status
		f.mu.Unlock()
		desc := map[string]string{"": "All Systems Operational", "none": "All Systems Operational", "minor": "Partially Degraded Service", "major": "Partial System Outage", "critical": "Major System Outage"}[st]
		if st == "" {
			st = "none"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"name": "Akamai"}, "status": map[string]any{"indicator": st, "description": desc}})
		return
	}
	if r.Method != http.MethodGet && !(r.Method == http.MethodPost && r.URL.Path == trafficPath) {
		http.Error(w, "read-only", http.StatusMethodNotAllowed)
		return
	}
	if !f.Creds.Check(r) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"title": "Not authorized", "detail": "The signature does not match"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[r.URL.Path]++
	out := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	gtm := len(parts) >= 3 && (parts[0] == "config-gtm" || parts[0] == "gtm-api")
	papi := parts[0] == "papi" && r.URL.Path != "/papi/v1/contracts"
	if r.URL.Path == trafficPath && f.NoTraffic ||
		gtm && f.NoGTM || parts[0] == "cps" && f.NoCPS || papi && f.NoPAPI || parts[0] == "config-dns" && f.NoDNS {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	accept := func(t string) bool {
		if r.Header.Get("Accept") != t {
			w.WriteHeader(http.StatusNotAcceptable)
			return false
		}
		return true
	}
	switch {
	case r.URL.Path == "/config-gtm/v1/domains":
		var items []map[string]any
		for n := range f.gtm {
			items = append(items, map[string]any{"name": n})
		}
		out(map[string]any{"items": items})
	case len(parts) == 4 && parts[0] == "config-gtm":
		d := f.gtm[parts[3]]
		if d == nil {
			http.NotFound(w, r)
			return
		}
		var dcs, props []map[string]any
		for id, n := range d.Datacenters {
			dcs = append(dcs, map[string]any{"datacenterId": id, "nickname": n})
		}
		for _, p := range d.Properties {
			var tts []map[string]any
			for id, servers := range p.Targets {
				var ips []string
				for ip := range servers {
					ips = append(ips, ip)
				}
				tts = append(tts, map[string]any{"datacenterId": id, "enabled": !p.Off[id], "weight": 1, "servers": ips})
			}
			props = append(props, map[string]any{"name": p.Name, "type": p.Type, "trafficTargets": tts})
		}
		out(map[string]any{"name": parts[3], "type": d.Type, "datacenters": dcs, "properties": props})
	case len(parts) == 6 && parts[0] == "config-gtm" && parts[4] == "status":
		d := f.gtm[parts[3]]
		if d == nil {
			http.NotFound(w, r)
			return
		}
		st := d.Propagation
		if st == "" {
			st = "COMPLETE"
		}
		out(map[string]any{"propagationStatus": st, "propagationStatusDate": d.Since, "message": "Current configuration has been propagated to all GTM nameservers"})
	case len(parts) == 8 && parts[0] == "gtm-api" && parts[3] == "ip-availability":
		d := f.gtm[parts[5]]
		if d == nil {
			http.NotFound(w, r)
			return
		}
		for _, p := range d.Properties {
			if p.Name != parts[7] {
				continue
			}
			var dcs []map[string]any
			for id, servers := range p.Targets {
				var ips []map[string]any
				for ip, alive := range servers {
					ips = append(ips, map[string]any{"ip": ip, "handedOut": alive && !p.Off[id], "score": map[bool]float64{true: 0.05, false: 1}[alive], "alive": alive})
				}
				dcs = append(dcs, map[string]any{"datacenterId": id, "nickname": d.Datacenters[id], "IPs": ips})
			}
			out(map[string]any{"dataRows": []map[string]any{{"timestamp": time.Now().UTC().Truncate(time.Minute), "datacenters": dcs}}})
			return
		}
		http.NotFound(w, r)
	case r.URL.Path == "/papi/v1/contracts":
		var items []map[string]any
		seen := map[string]bool{}
		for _, e := range f.cps {
			if e.Contract != "" && !seen[e.Contract] {
				seen[e.Contract] = true
				items = append(items, map[string]any{"contractId": "ctr_" + e.Contract})
			}
		}
		for _, p := range f.pm {
			if p.Contract != "" && !seen[p.Contract] {
				seen[p.Contract] = true
				items = append(items, map[string]any{"contractId": "ctr_" + p.Contract})
			}
		}
		out(map[string]any{"contracts": map[string]any{"items": items}})
	case r.URL.Path == "/cps/v2/enrollments":
		if !accept("application/vnd.akamai.cps.enrollments.v10+json") {
			return
		}
		ct := r.URL.Query().Get("contractId")
		var es []map[string]any
		for id, e := range f.cps {
			if ct != "" && e.Contract != ct {
				continue
			}
			pending := []map[string]any{}
			if e.Waiting != "-" {
				pending = append(pending, map[string]any{"location": "/cps/v2/enrollments/" + strconv.Itoa(id) + "/changes/" + strconv.Itoa(id+1), "changeType": "renewal"})
			}
			es = append(es, map[string]any{"id": id, "csr": map[string]any{"cn": e.CN, "sans": e.SANs}, "validationType": e.Validation,
				"certificateType": "san", "pendingChanges": pending})
		}
		out(map[string]any{"enrollments": es})
	case len(parts) == 6 && parts[0] == "cps" && parts[4] == "deployments" && parts[5] == "production":
		id, _ := strconv.Atoi(parts[3])
		e := f.cps[id]
		if e == nil || len(e.Deployed) == 0 {
			http.NotFound(w, r)
			return
		}
		if !accept("application/vnd.akamai.cps.deployment.v7+json") {
			return
		}
		d := map[string]any{"primaryCertificate": map[string]any{"certificate": e.Deployed[0]}}
		var more []map[string]any
		for _, p := range e.Deployed[1:] {
			more = append(more, map[string]any{"certificate": p})
		}
		d["multiStackedCertificates"] = more
		out(d)
	case len(parts) == 6 && parts[0] == "cps" && parts[4] == "changes":
		id, _ := strconv.Atoi(parts[3])
		e := f.cps[id]
		if e == nil || e.Waiting == "-" {
			http.NotFound(w, r)
			return
		}
		if !accept("application/vnd.akamai.cps.change.v2+json") {
			return
		}
		in := []map[string]any{}
		if e.Waiting != "" {
			in = append(in, map[string]any{"type": e.Waiting, "requiredToProceed": true, "info": r.URL.Path + "/input/info/" + e.Waiting})
		}
		out(map[string]any{"statusInfo": map[string]any{"state": "awaiting-input"}, "allowedInput": in})
	case r.URL.Path == "/papi/v1/groups":
		cts := map[string][]string{}
		for _, p := range f.pm {
			if !slices.Contains(cts[p.Group], "ctr_"+p.Contract) {
				cts[p.Group] = append(cts[p.Group], "ctr_"+p.Contract)
			}
		}
		var items []map[string]any
		for g, c := range cts {
			items = append(items, map[string]any{"groupId": fakeGroupID(g), "groupName": g, "contractIds": c})
		}
		out(map[string]any{"groups": map[string]any{"items": items}})
	case r.URL.Path == "/papi/v1/properties":
		q := r.URL.Query()
		var items []map[string]any
		for id, p := range f.pm {
			if "ctr_"+p.Contract != q.Get("contractId") || fakeGroupID(p.Group) != q.Get("groupId") {
				continue
			}
			it := map[string]any{"propertyId": id, "propertyName": p.Name, "contractId": "ctr_" + p.Contract, "groupId": fakeGroupID(p.Group),
				"latestVersion": p.Latest, "stagingVersion": nil, "productionVersion": nil}
			if p.Staging > 0 {
				it["stagingVersion"] = p.Staging
			}
			if p.Production > 0 {
				it["productionVersion"] = p.Production
			}
			items = append(items, it)
		}
		out(map[string]any{"properties": map[string]any{"items": items}})
	case len(parts) == 5 && parts[0] == "papi" && parts[2] == "properties" && parts[4] == "activations":
		p := f.pm[parts[3]]
		if p == nil || r.URL.Query().Get("contractId") != "ctr_"+p.Contract {
			http.NotFound(w, r)
			return
		}
		var items []map[string]any
		for i, a := range p.Activations {
			items = append(items, map[string]any{"activationId": "atv_" + strconv.Itoa(1000+i), "propertyName": p.Name, "propertyId": parts[3],
				"propertyVersion": a.Version, "network": a.Network, "activationType": "ACTIVATE", "status": a.Status,
				"submitDate": a.Submitted, "updateDate": a.Submitted.Add(10 * time.Minute), "note": a.Note, "notifyEmails": []string{"ops@example.com"}})
		}
		out(map[string]any{"activations": map[string]any{"items": items}})
	case r.URL.Path == trafficPath:
		var q struct {
			Dimensions []string `json:"dimensions"`
			Metrics    []string `json:"metrics"`
		}
		if json.NewDecoder(r.Body).Decode(&q) != nil || !slices.Equal(q.Dimensions, []string{"cpcode", "responseClass"}) || !slices.Equal(q.Metrics, []string{"edgeHitsSum"}) {
			w.WriteHeader(http.StatusBadRequest)
			out(map[string]any{"title": "Bad request", "detail": "the fake answers only hits by cpcode and responseClass"})
			return
		}
		var rows []map[string]any
		for cp, t := range f.traffic {
			ok := t[0] - t[1]
			rows = append(rows, map[string]any{"cpcode": cp, "responseClass": "2xx", "edgeHitsSum": ok * 9 / 10},
				map[string]any{"cpcode": cp, "responseClass": "3xx", "edgeHitsSum": ok - ok*9/10})
			if t[1] > 0 {
				rows = append(rows, map[string]any{"cpcode": cp, "responseClass": "5xx", "edgeHitsSum": t[1]})
			}
		}
		end := time.Now().UTC().Add(-10 * time.Minute).Truncate(5 * time.Minute)
		out(map[string]any{"metadata": map[string]any{"name": "CDN traffic report data", "start": end.Add(-5 * time.Minute), "end": end, "hasMoreRows": false, "links": []any{}}, "data": rows})
	case r.URL.Path == "/config-dns/v2/zones":
		var names []string
		for n := range f.zones {
			names = append(names, n)
		}
		slices.Sort(names)
		var items []map[string]any
		for _, n := range fakePage(r, names) {
			z := f.zones[n]
			t := z.Type
			if t == "" {
				t = "PRIMARY"
			}
			items = append(items, map[string]any{"zone": n, "type": t, "versionId": "v" + strconv.Itoa(z.version), "lastModifiedDate": time.Now().UTC().Truncate(time.Hour), "activationState": "ACTIVE"})
		}
		out(map[string]any{"metadata": map[string]any{"totalElements": len(names)}, "zones": items})
	case len(parts) == 5 && parts[0] == "config-dns" && parts[4] == "recordsets":
		z := f.zones[parts[3]]
		if z == nil {
			http.NotFound(w, r)
			return
		}
		all := append([]RecordSet{{Name: parts[3], Type: "SOA", TTL: 86400, RData: []string{"a1-1.akam.net. hostmaster." + parts[3] + ". " + strconv.Itoa(2026111400+z.version) + " 3600 600 604800 300"}}}, z.Records...)
		out(map[string]any{"metadata": map[string]any{"totalElements": len(all)}, "recordsets": fakePage(r, all)})
	default:
		http.NotFound(w, r)
	}
}

// fakePage is the page of items a request asks for (page, pageSize).
func fakePage[T any](r *http.Request, items []T) []T {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 25
	}
	from := min((page-1)*size, len(items))
	return items[from:min(from+size, len(items))]
}

// fakeGroupID makes a group's id from its name.
func fakeGroupID(name string) string {
	var h uint32
	for _, c := range name {
		h = h*31 + uint32(c)
	}
	return "grp_" + strconv.Itoa(int(h%100000))
}
