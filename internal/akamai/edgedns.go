package akamai

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Edge DNS (docs/akamai.md step 4): every zone's record sets, so
// obs-beacond can say when they drift from what was accepted, as
// Cloudflare's DNS. Field names from Akamai's Edge DNS (Config DNS v2) API
// reference. A zone's record sets are read again only when its version
// changed.

// DNSZone is an Edge DNS zone.
type DNSZone struct {
	Name     string      `json:"name"`
	Type     string      `json:"type"`    // PRIMARY, SECONDARY, ALIAS
	Version  string      `json:"version"` // changes with every change to the zone
	Modified time.Time   `json:"modified"`
	Records  []RecordSet `json:"records"` // nil: not read (an ALIAS zone, or an error)
	Count    int         `json:"count"`   // len(Records), for the page, which isn't sent them
	Error    string      `json:"error,omitempty"`
}

// RecordSet is one name and type's records.
type RecordSet struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	TTL   int      `json:"ttl"`
	RData []string `json:"rdata"`
}

// Key identifies the record set's whole setting.
func (r RecordSet) Key() string {
	rd := slices.Clone(r.RData)
	slices.Sort(rd)
	return r.Type + " " + r.Name + " ttl=" + strconv.Itoa(r.TTL) + " " + strings.Join(rd, " | ")
}

// String is how a person reads it.
func (r RecordSet) String() string {
	rd := slices.Clone(r.RData)
	slices.Sort(rd)
	return r.Type + " " + r.Name + " → " + strings.Join(rd, ", ") + " (TTL " + strconv.Itoa(r.TTL) + ")"
}

type dnsZones struct {
	Metadata struct {
		TotalElements int `json:"totalElements"`
	} `json:"metadata"`
	Zones []struct {
		Zone             string    `json:"zone"`
		Type             string    `json:"type"`
		VersionID        string    `json:"versionId"`
		LastModifiedDate time.Time `json:"lastModifiedDate"`
	} `json:"zones"`
}

type dnsRecordSets struct {
	Metadata struct {
		TotalElements int `json:"totalElements"`
	} `json:"metadata"`
	RecordSets []RecordSet `json:"recordsets"`
}

// dnsPage is how many zones or record sets are asked for at a time.
const dnsPage = 500

// readDNS lists every zone and reads the record sets of each whose
// version changed since prev.
func (w *Watcher) readDNS(ctx context.Context, c Creds, prev []DNSZone) ([]DNSZone, error) {
	was := map[string]DNSZone{}
	for _, z := range prev {
		was[z.Name] = z
	}
	var out []DNSZone
	for page := 1; ; page++ {
		var zs dnsZones
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(dnsPage)}}
		if err := w.get(ctx, c, "/config-dns/v2/zones?"+q.Encode(), &zs); err != nil {
			return nil, err
		}
		for _, z := range zs.Zones {
			out = append(out, DNSZone{Name: z.Zone, Type: z.Type, Version: z.VersionID, Modified: z.LastModifiedDate})
		}
		if len(zs.Zones) < dnsPage || len(out) >= zs.Metadata.TotalElements {
			break
		}
	}
	for i := range out {
		z := &out[i]
		if o, ok := was[z.Name]; ok && o.Version == z.Version && o.Error == "" {
			z.Records = o.Records
			continue
		}
		if z.Type == "ALIAS" {
			continue // its records are another zone's
		}
		z.Records, z.Error = w.recordSets(ctx, c, z.Name)
	}
	slices.SortFunc(out, func(a, b DNSZone) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// recordSets reads a zone's record sets, a page at a time. SOA is left
// out: its serial moves with every change.
func (w *Watcher) recordSets(ctx context.Context, c Creds, zone string) ([]RecordSet, string) {
	recs := []RecordSet{}
	for page, n := 1, 0; ; page++ {
		var rs dnsRecordSets
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(dnsPage)}, "sortBy": {"name,type"}}
		if err := w.get(ctx, c, "/config-dns/v2/zones/"+url.PathEscape(zone)+"/recordsets?"+q.Encode(), &rs); err != nil {
			return nil, err.Error()
		}
		n += len(rs.RecordSets)
		for _, r := range rs.RecordSets {
			if r.Type != "SOA" {
				recs = append(recs, r)
			}
		}
		if len(rs.RecordSets) < dnsPage || n >= rs.Metadata.TotalElements {
			return recs, ""
		}
	}
}
