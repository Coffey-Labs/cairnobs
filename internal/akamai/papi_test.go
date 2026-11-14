package akamai

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// Step 3: properties in each group's contracts, the newest activation on
// each network, and activations not read again for a settled property.
func TestPAPI(t *testing.T) {
	c := testCreds()
	f := &Fake{Creds: c, NoGTM: true, NoCPS: true}
	now := time.Now().UTC().Truncate(time.Second)
	f.SetPMProperty("prp_1", FakePMProperty{Name: "www.example.com", Group: "Web", Contract: "1-ABC", Latest: 12, Staging: 12, Production: 12,
		Activations: []FakeActivation{
			{Version: 11, Network: "PRODUCTION", Status: "ACTIVE", Submitted: now.Add(-48 * time.Hour)},
			{Version: 12, Network: "PRODUCTION", Status: "ACTIVE", Submitted: now.Add(-24 * time.Hour)},
			{Version: 12, Network: "STAGING", Status: "ACTIVE", Submitted: now.Add(-25 * time.Hour)},
		}})
	f.SetPMProperty("prp_2", FakePMProperty{Name: "api.example.com", Group: "Web", Contract: "1-ABC", Latest: 7, Staging: 7, Production: 6,
		Activations: []FakeActivation{
			{Version: 6, Network: "PRODUCTION", Status: "ACTIVE", Submitted: now.Add(-72 * time.Hour)},
			{Version: 7, Network: "PRODUCTION", Status: "FAILED", Submitted: now.Add(-2 * time.Hour), Note: "new origin"},
			{Version: 7, Network: "STAGING", Status: "ACTIVE", Submitted: now.Add(-3 * time.Hour)},
		}})
	f.SetPMProperty("prp_3", FakePMProperty{Name: "media.example.com", Group: "Media", Contract: "2-DEF", Latest: 3, Staging: 2,
		Activations: []FakeActivation{{Version: 2, Network: "STAGING", Status: "ACTIVE", Submitted: now.Add(-5 * time.Hour)},
			{Version: 3, Network: "STAGING", Status: "ZONE_2", Submitted: now.Add(-90 * time.Minute)}}})
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, _ := New(Config{Edgerc: writeEdgerc(t, c), API: srv.URL})
	w.Poll(context.Background(), now)
	s := w.Snapshot()
	if s.Error != "" || s.PAPIError != "" || len(s.PMProperties) != 3 {
		t.Fatalf("%+v", s)
	}
	by := map[string]PMProperty{}
	for _, p := range s.PMProperties {
		by[p.Name] = p
	}
	if p := by["www.example.com"]; p.Group != "Web" || p.Production != 12 || p.LastProduction == nil || p.LastProduction.Version != 12 || p.LastStaging.Status != "ACTIVE" {
		t.Errorf("www: %+v", p)
	}
	if p := by["api.example.com"]; p.LastProduction == nil || p.LastProduction.Status != "FAILED" || p.LastProduction.Note != "new origin" || p.Production != 6 {
		t.Errorf("api: %+v", p)
	}
	if p := by["media.example.com"]; p.Production != 0 || p.LastProduction != nil || !p.LastStaging.Pending() {
		t.Errorf("media: %+v", p)
	}

	// 16 minutes on: the settled one isn't read again; the others are.
	w.Poll(context.Background(), now.Add(16*time.Minute))
	if n := f.Calls("/papi/v1/properties/prp_1/activations"); n != 1 {
		t.Errorf("settled property read %d times", n)
	}
	if f.Calls("/papi/v1/properties/prp_2/activations") != 2 || f.Calls("/papi/v1/properties/prp_3/activations") != 2 {
		t.Error("unsettled properties not read again")
	}
	if p := w.Snapshot().PMProperties[2]; p.Name != "www.example.com" || p.LastProduction == nil || p.Read != now {
		t.Errorf("settled property's reading not carried: %+v", p)
	}
	// Its versions moved: read again.
	f.SetPMProperty("prp_1", FakePMProperty{Name: "www.example.com", Group: "Web", Contract: "1-ABC", Latest: 13, Staging: 12, Production: 12})
	w.Poll(context.Background(), now.Add(32*time.Minute))
	if n := f.Calls("/papi/v1/properties/prp_1/activations"); n != 2 {
		t.Errorf("moved property read %d times", n)
	}

	f.NoPAPI = true
	w.Poll(context.Background(), now.Add(48*time.Minute))
	if s := w.Snapshot(); s.PMProperties != nil || len(s.Absent) != 3 {
		t.Errorf("without PAPI: %+v", s.Absent)
	}
}
