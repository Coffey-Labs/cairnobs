package state

import (
	"testing"
	"time"
)

func TestStatusHistory(t *testing.T) {
	db, _ := open(t)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []Change{
		{"svc/mail", t0.Add(-48 * time.Hour), "good"},
		{"svc/mail", t0.Add(-24 * time.Hour), "critical"},
		{"svc/mail", t0.Add(-23 * time.Hour), "good"},
		{"svc/git", t0.Add(2 * time.Hour), "good"},
		{"svc/mail", t0.Add(3 * time.Hour), "critical"},
		{"svc/mail", t0.Add(4 * time.Hour), "good"},
		{"svc/mail", t0.Add(30 * 24 * time.Hour), "warning"}, // after the period
	} {
		if err := db.LogStatus(c.Entity, c.Status, c.At); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.StatusChanges(t0, t0.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, c := range got {
		s = append(s, c.Entity+"@"+c.At.UTC().Format("02T15")+"="+c.Status)
	}
	want := "svc/mail@30T01=good svc/git@01T02=good svc/mail@01T03=critical svc/mail@01T04=good"
	if j := join(s); j != want {
		t.Errorf("changes = %s\nwant      %s", j, want)
	}
	latest, _ := db.LatestStatuses()
	if latest["svc/mail"] != "warning" || latest["svc/git"] != "good" {
		t.Errorf("latest = %v", latest)
	}
	if !db.HistoryStart().Equal(t0.Add(-48 * time.Hour)) {
		t.Errorf("start = %s", db.HistoryStart())
	}
	// Pruning keeps each entity's last change.
	if err := db.Prune(t0.Add(60 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = db.StatusChanges(t0.Add(61*24*time.Hour), t0.Add(62*24*time.Hour))
	if len(got) != 2 {
		t.Errorf("after prune: %+v", got)
	}
	// One kind of entity only; the prefix's _ is a letter, not a wildcard.
	_ = db.LogStatus("host/mail", "critical", t0.Add(61*24*time.Hour))
	_ = db.LogStatus("svcXmail", "critical", t0.Add(61*24*time.Hour))
	_ = db.LogStatus("svc_mail", "critical", t0.Add(61*24*time.Hour))
	got, _ = db.StatusChangesOf("svc/", t0.Add(61*24*time.Hour), t0.Add(62*24*time.Hour))
	if len(got) != 2 || got[0].Entity[:4] != "svc/" || got[1].Entity[:4] != "svc/" {
		t.Errorf("svc/ only: %+v", got)
	}
	got, _ = db.StatusChangesOf("svc_", t0, t0.Add(62*24*time.Hour))
	if len(got) != 1 || got[0].Entity != "svc_mail" {
		t.Errorf("svc_ only: %+v", got)
	}
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}

func TestHostHoursMailRoundsNotes(t *testing.T) {
	db, _ := open(t)
	h := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
	if err := db.PutHostHour(HostHour{Host: "node1", Hour: h, CPU: 3, CPUMax: 40, Mem: 50, MemMax: 55, Disks: []DiskSample{{Mount: "/", Used: 10, Total: 100}}}); err != nil {
		t.Fatal(err)
	}
	_ = db.PutHostHour(HostHour{Host: "node1", Hour: h, CPU: 4, CPUMax: 41, Mem: 51, MemMax: 56, Disks: []DiskSample{{Mount: "/", Used: 11, Total: 100}}}) // replaces
	hh, err := db.HostHours(h, h.Add(time.Hour))
	if err != nil || len(hh) != 1 || hh[0].CPU != 4 || hh[0].Disks[0].Used != 11 {
		t.Fatalf("%+v %v", hh, err)
	}
	_ = db.PutMailRound(MailRound{At: h, Seconds: 0.6, Queue: 0})
	_ = db.PutMailRound(MailRound{At: h.Add(5 * time.Minute), Seconds: -1, Queue: -1})
	mr, _ := db.MailRounds(h, h.Add(time.Hour))
	if len(mr) != 2 || mr[1].Seconds != -1 {
		t.Errorf("%+v", mr)
	}
	_ = db.SetNote("a1", Note{Text: "disk full on node2", By: "John", At: h})
	n, _ := db.Notes([]string{"a1", "a2"})
	if len(n) != 1 || n["a1"].Text != "disk full on node2" {
		t.Errorf("%+v", n)
	}
	_ = db.SetNote("a1", Note{})
	if n, _ := db.Notes([]string{"a1"}); len(n) != 0 {
		t.Errorf("note not removed: %+v", n)
	}
	_ = db.PutIncident(Incident{ID: "i1", Key: "k", Severity: "warning", Opened: h})
	_ = db.PutIncident(Incident{ID: "i2", Key: "k", Severity: "warning", Opened: h.Add(48 * time.Hour)})
	in, _ := db.IncidentsBetween(h, h.Add(24*time.Hour))
	if len(in) != 1 || in[0].ID != "i1" {
		t.Errorf("%+v", in)
	}
}

func TestWorkloadUsageAndHours(t *testing.T) {
	db, _ := open(t)
	at := time.Date(2026, 11, 13, 10, 0, 30, 0, time.UTC)
	if err := db.PutWorkloadUsage("prod", at, []byte(`[{"id":"n1","cpu":1}]`)); err != nil {
		t.Fatal(err)
	}
	_ = db.PutWorkloadUsage("prod", at.Add(-time.Minute), []byte(`[{"id":"old"}]`)) // older: kept out
	times, err := db.WorkloadUsageTimes()
	if err != nil || !times["prod"].Equal(at) {
		t.Fatalf("times %v %v", times, err)
	}
	if got, u, err := db.WorkloadUsage("prod"); err != nil || !got.Equal(at) || string(u) != `[{"id":"n1","cpu":1}]` {
		t.Errorf("usage %v %s %v", got, u, err)
	}
	if got, u, err := db.WorkloadUsage("none"); err != nil || !got.IsZero() || u != nil {
		t.Errorf("no usage: %v %s %v", got, u, err)
	}
	_ = db.DeleteWorkloadSource("prod")
	if times, _ := db.WorkloadUsageTimes(); len(times) != 0 {
		t.Errorf("after the cluster went: %v", times)
	}

	h := at.Truncate(time.Hour)
	hs := []WorkloadHour{
		{Source: "prod", Workload: "shop/deployment/web", Hour: h, Kind: "controller", CPU: 0.3, CPUMax: 0.5, CPUOf: 1, Mem: 600 << 20, MemMax: 700 << 20},
		{Source: "prod", Workload: "shop/deployment/web", Hour: h.Add(time.Hour), Kind: "controller", CPU: 0.4, CPUMax: 0.6, Mem: 610 << 20, MemMax: 710 << 20},
		{Source: "prod", Workload: "worker-1", Hour: h, Kind: "node", CPU: 2, CPUMax: 3, CPUOf: 4, Mem: 8 << 30, MemMax: 9 << 30, MemOf: 16 << 30},
		{Source: "mgmt1:fa-01", Workload: "fa-01", Hour: h, Kind: "cluster", ReadOps: 4e4, WriteOps: 2e4, ReadUs: 310, ReadUsMax: 900, WriteUs: 140,
			WriteUsMax: 600, ReadBps: 2e9, WriteBps: 1e9, Used: 400 << 40, UsedOf: 500 << 40, Leases: 120, LeasesMax: 150},
	}
	if err := db.PutWorkloadHours(hs); err != nil {
		t.Fatal(err)
	}
	hs[0].CPU = 0.35
	_ = db.PutWorkloadHours(hs[:1]) // another copy's: replaces
	got, err := db.WorkloadHours("prod", "shop/deployment/web", h, h.Add(2*time.Hour))
	if err != nil || len(got) != 2 || got[0].CPU != 0.35 || got[0].CPUOf != 1 || got[1].CPUOf != 0 || !got[1].Hour.Equal(h.Add(time.Hour)) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := db.WorkloadHours("mgmt1:fa-01", "fa-01", h, h.Add(time.Hour)); err != nil || len(got) != 1 || !got[0].Hour.Equal(h) {
		t.Errorf("an array's hour: %+v %v", got, err)
	} else if got[0].Hour = hs[3].Hour; got[0] != hs[3] {
		t.Errorf("an array's hour: %+v", got[0])
	}
	// Postgres prunes whole months, so an hour from two months back goes
	// and this month's stay.
	old := hs[0]
	old.Hour = time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	if err := db.PutWorkloadHours([]WorkloadHour{old}); err != nil {
		t.Fatal(err)
	}
	if err := db.Prune(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.WorkloadHours("prod", "shop/deployment/web", old.Hour, h.Add(2*time.Hour)); len(got) != 2 || !got[0].Hour.Equal(h) {
		t.Errorf("after pruning: %+v", got)
	}
}
