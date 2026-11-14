package akamai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Step 5: hits and 5xx by CP code from the one POST query, Akamai's
// status, and no other POST allowed.
func TestTraffic(t *testing.T) {
	c := testCreds()
	f := &Fake{Creds: c, NoGTM: true, NoCPS: true, NoPAPI: true, NoDNS: true, Status: "minor"}
	f.SetTraffic(1001, 100000, 50)
	f.SetTraffic(1002, 2000, 600)
	f.SetTraffic(1003, 0, 0)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w, _ := New(Config{Edgerc: writeEdgerc(t, c), API: srv.URL})
	w.Poll(context.Background(), time.Now())
	s := w.Snapshot()
	if s.Error != "" || s.TrafficError != "" || len(s.Traffic) != 3 {
		t.Fatalf("%+v", s)
	}
	if a, b := s.Traffic[0], s.Traffic[1]; a.CPCode != 1001 || a.Hits != 100000 || a.Errors != 50 || b.CPCode != 1002 || b.Pct() != 30 {
		t.Errorf("%+v", s.Traffic)
	}
	if s.TrafficEnd.Sub(s.TrafficStart) != 5*time.Minute || s.Status.Indicator != "minor" {
		t.Errorf("window %v–%v, status %+v", s.TrafficStart, s.TrafficEnd, s.Status)
	}
	cr, _ := ReadEdgerc(w.cfg.Edgerc, "default")
	if err := w.do(context.Background(), cr, http.MethodPost, "/papi/v1/properties", "application/json", []byte("{}"), nil); err == nil {
		t.Error("a POST other than the traffic query was sent")
	}

	f.NoTraffic = true
	w.Poll(context.Background(), time.Now())
	if s := w.Snapshot(); s.Traffic != nil || s.Absent[len(s.Absent)-1] != "dns" || s.Absent[0] != "gtm" {
		t.Errorf("without traffic: %v %+v", s.Absent, s.Traffic)
	}
}
