package units

import (
	"testing"
	"time"
)

func TestSCMUnit(t *testing.T) {
	at := time.Unix(1763000000, 0).UTC()
	for _, c := range []struct {
		state, start, exit uint32
		active, sub        string
	}{
		{scmRunning, scmAutoStart, 0, "active", "running"},
		{scmStopped, scmAutoStart, 0, "failed", "dead"}, // should be running
		{scmStopped, 3, 1067, "failed", "dead"},         // manual, stopped with an error
		{scmStopped, 3, 0, "inactive", "dead"},          // manual, stopped cleanly
		{scmStopped, scmDisabledStart, 0, "inactive", "disabled"},
		{scmStartPending, scmAutoStart, 0, "activating", "start"},
		{scmPaused, scmAutoStart, 0, "active", "paused"},
	} {
		u := scmUnit("W32Time.service", c.state, c.start, c.exit, at)
		if u.Active != c.active || u.Sub != c.sub || u.Load != "loaded" {
			t.Errorf("%+v: got %s/%s", c, u.Active, u.Sub)
		}
		if (u.Active == "inactive" || u.Active == "failed") && !u.Since.IsZero() {
			t.Errorf("%+v: a stopped service has a since", c)
		}
	}
	if ServiceName("W32Time.service") != "W32Time" || ServiceName(Name("MSSQLSERVER")) != "MSSQLSERVER" {
		t.Fatal("names")
	}
}
