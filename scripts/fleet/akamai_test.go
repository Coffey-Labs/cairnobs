package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/akamai"
)

// The fleet's fake Akamai reads whole: every product, at its scale.
func TestFleetAkamai(t *testing.T) {
	fleetSites = 20
	c := akamai.Creds{Host: "akab-fleet.luna.akamaiapis.net", ClientToken: "ct", ClientSecret: "cs", AccessToken: "at"}
	f := &akamai.Fake{Creds: c}
	fleetAkamai(f)
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := filepath.Join(t.TempDir(), ".edgerc")
	_ = os.WriteFile(p, []byte("[default]\nhost = "+c.Host+"\nclient_token = ct\nclient_secret = cs\naccess_token = at\n"), 0o600)
	w, err := akamai.New(akamai.Config{Edgerc: p, API: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	w.Poll(context.Background(), time.Now())
	s := w.Snapshot()
	t.Logf("read in %s", time.Since(start))
	if s.Error != "" || s.CPSError != "" || s.PAPIError != "" || s.DNSError != "" || s.TrafficError != "" || len(s.Absent) > 0 {
		t.Fatalf("%s %s %s %s %s %v", s.Error, s.CPSError, s.PAPIError, s.DNSError, s.TrafficError, s.Absent)
	}
	if len(s.Domains) != 8 || len(s.Enrollments) != 180 || len(s.PMProperties) != 320 || len(s.DNSZones) != 40 || len(s.Traffic) != 320 {
		t.Errorf("%d domains, %d enrollments, %d properties, %d zones, %d CP codes", len(s.Domains), len(s.Enrollments), len(s.PMProperties), len(s.DNSZones), len(s.Traffic))
	}
	failed := 0
	for _, p := range s.PMProperties {
		if p.LastProduction != nil && p.LastProduction.Status == "FAILED" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("%d failed activations", failed)
	}
}
