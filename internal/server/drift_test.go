package server

import (
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

func TestImageDrift(t *testing.T) {
	srv, _ := checkServer(t)
	site := func(img string) report.Report {
		return report.Report{V: report.Version, Containers: &report.Containers{List: []report.Container{
			{ID: "a", Name: "example-org", Image: img, State: "running", Status: "Up 1 hour"},
			{ID: "b", Name: "example-org-pre-clone-1", Image: "example-org:old", State: "exited", Status: "Exited (0) 1 hour ago"},
		}}}
	}
	at := func(t0 time.Time, newOn ...string) []Condition {
		for _, h := range []string{"host1", "host2", "host3"} {
			img := "example-org:20261015-1"
			for _, n := range newOn {
				if n == h {
					img = "example-org:20261016-2"
				}
			}
			srv.store.Put(h, site(img), t0)
		}
		hosts, _ := srv.view(t0)
		return srv.imageDrift(hosts, t0)
	}
	t0 := time.Now()
	if c := at(t0, "host1"); len(c) != 0 { // a deploy has just reached host1
		t.Fatalf("too early: %+v", c)
	}
	if c := at(t0.Add(10*time.Minute), "host1"); len(c) != 0 {
		t.Fatalf("10 min: %+v", c)
	}
	c := at(t0.Add(16*time.Minute), "host1")
	if len(c) != 1 || c[0].Key != "drift/example-org" || !strings.Contains(c[0].Detail, "host1 runs example-org:20261016-2; host2 runs example-org:20261015-1") {
		t.Fatalf("16 min: %+v", c)
	}
	if c := at(t0.Add(17*time.Minute), "host1", "host2", "host3"); len(c) != 0 { // the deploy finished
		t.Fatalf("after the deploy: %+v", c)
	}
	// It starts over: a new difference waits its 15 minutes again.
	if c := at(t0.Add(18*time.Minute), "host3"); len(c) != 0 {
		t.Fatalf("new drift too early: %+v", c)
	}
}
