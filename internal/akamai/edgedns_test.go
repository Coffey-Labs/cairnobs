package akamai

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// Step 4: zones and record sets a page at a time, SOA left out, and a
// zone's record sets read again only when its version changes.
func TestEdgeDNS(t *testing.T) {
	c := testCreds()
	f := &Fake{Creds: c, NoGTM: true, NoCPS: true, NoPAPI: true}
	var many []RecordSet
	for i := range 1200 {
		many = append(many, RecordSet{Name: fmt.Sprintf("h%04d.big.example", i), Type: "A", TTL: 300, RData: []string{fmt.Sprintf("10.0.%d.%d", i/250, i%250)}})
	}
	f.SetZone("big.example", FakeZone{Records: many})
	f.SetZone("example.com", FakeZone{Records: []RecordSet{
		{Name: "www.example.com", Type: "CNAME", TTL: 300, RData: []string{"www.example.com.edgekey.net."}},
		{Name: "example.com", Type: "MX", TTL: 3600, RData: []string{"20 mx2.example.com.", "10 mx1.example.com."}},
	}})
	f.SetZone("alias.example", FakeZone{Type: "ALIAS"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, _ := New(Config{Edgerc: writeEdgerc(t, c), API: srv.URL})
	now := time.Now()
	w.Poll(context.Background(), now)
	s := w.Snapshot()
	if s.Error != "" || s.DNSError != "" || len(s.DNSZones) != 3 {
		t.Fatalf("%+v", s)
	}
	if z := s.DNSZones[0]; z.Name != "alias.example" || z.Records != nil {
		t.Errorf("alias: %+v", z)
	}
	if z := s.DNSZones[1]; len(z.Records) != 1200 || z.Error != "" {
		t.Errorf("big: %d records, %s", len(z.Records), z.Error)
	}
	ex := s.DNSZones[2]
	if len(ex.Records) != 2 || ex.Records[1].String() != "MX example.com → 10 mx1.example.com., 20 mx2.example.com. (TTL 3600)" {
		t.Errorf("example.com: %+v", ex.Records)
	}
	if n := f.Calls("/config-dns/v2/zones/big.example/recordsets"); n != 3 {
		t.Errorf("big.example read in %d pages", n)
	}

	f.SetZone("example.com", FakeZone{Records: ex.Records[:1]})
	w.Poll(context.Background(), now.Add(16*time.Minute))
	s = w.Snapshot()
	if f.Calls("/config-dns/v2/zones/big.example/recordsets") != 3 || f.Calls("/config-dns/v2/zones/example.com/recordsets") != 2 {
		t.Error("record sets read again for an unchanged zone, or not for a changed one")
	}
	if len(s.DNSZones[2].Records) != 1 || len(s.DNSZones[1].Records) != 1200 {
		t.Errorf("%d, %d", len(s.DNSZones[2].Records), len(s.DNSZones[1].Records))
	}
}
