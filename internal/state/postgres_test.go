package state

import (
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/state/statetest"
)

func postgresOnly(t *testing.T) {
	t.Helper()
	if !statetest.Postgres() {
		t.Skipf("PostgreSQL only: set %s", statetest.Env)
	}
}

func (d *DB) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := d.queryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return n
}

// Copies that start together take turns migrating, and every migration runs
// once.
func TestMigrationsTakeTurns(t *testing.T) {
	postgresOnly(t)
	database, kf := statetest.Database(t, t.TempDir()), testKey(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(database, kf)
			if err == nil {
				db.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := Open(database, kf)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := db.count(t, "schema_migrations"); n != len(migrations) {
		t.Fatalf("%d migrations recorded, want %d", n, len(migrations))
	}
}

// History lands in a default partition until its month has one, moves in
// when it does, and retention drops whole months, keeping each entity's last
// status.
func TestPartitionsGrowAndShrink(t *testing.T) {
	postgresOnly(t)
	db, _ := open(t)
	jun := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	jul, aug := jun.AddDate(0, 1, 0), jun.AddDate(0, 2, 0)
	for _, c := range []Change{
		{"svc/quiet", jun, "good"}, // never changes again
		{"svc/busy", jun, "good"},
		{"svc/busy", jul, "critical"},
		{"svc/busy", aug, "good"},
	} {
		if err := db.LogStatus(c.Entity, c.Status, c.At); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Time{jun, jul, aug} {
		_ = db.PutHostHour(HostHour{Host: "h", Hour: at, Disks: []DiskSample{}})
		_ = db.PutMailRound(MailRound{At: at, Seconds: 1})
		_ = db.Audit(Entry{At: at, Who: "w", Action: "a"})
	}
	if n := db.count(t, "status_log_default"); n != 4 {
		t.Fatalf("%d rows in the default partition, want 4", n)
	}

	// New months: the rows move out of the default partitions.
	if err := db.EnsurePartitions(jun, aug); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"status_log_default": 0, "status_log_202606": 2, "status_log_202607": 1, "host_hours_202608": 1, "mail_rounds_default": 0, "audit_202607": 1} {
		if n := db.count(t, table); n != want {
			t.Errorf("%s has %d rows, want %d", table, n, want)
		}
	}
	if err := db.EnsurePartitions(jun, aug); err != nil { // and again changes nothing
		t.Fatal(err)
	}

	// Keep from 15 August: June and July go whole, August stays.
	if err := db.Prune(time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	months, _ := db.partitions("status_log")
	if len(months) == 0 || !months[0].Equal(monthOf(aug)) {
		t.Fatalf("status_log months after pruning: %v", months)
	}
	latest, _ := db.LatestStatuses()
	if latest["svc/quiet"] != "good" || latest["svc/busy"] != "good" {
		t.Fatalf("latest after pruning: %v", latest)
	}
	if n := db.count(t, "status_log"); n != 2 { // svc/quiet's June row, svc/busy's August one
		t.Fatalf("%d status rows after pruning, want 2", n)
	}
	if hh, _ := db.HostHours(jun, aug.Add(time.Hour)); len(hh) != 1 {
		t.Fatalf("host hours after pruning: %+v", hh)
	}
	if log, _ := db.AuditLog(10); len(log) != 3 {
		t.Fatalf("the audit log was pruned: %+v", log)
	}
	// Partitions are ready ahead of time.
	ahead := monthOf(time.Now()).AddDate(0, partitionsAhead, 0)
	months, _ = db.partitions("host_hours")
	if !months[len(months)-1].Equal(ahead) {
		t.Fatalf("newest host_hours month %s, want %s", months[len(months)-1], ahead)
	}
}

func TestPartitionNames(t *testing.T) {
	m := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if n := partitionName("status_log", m); n != "status_log_202611" {
		t.Fatal(n)
	}
	if got, ok := partitionMonth("status_log", "status_log_202611"); !ok || !got.Equal(m) {
		t.Fatal(got, ok)
	}
	if _, ok := partitionMonth("status_log", "status_log_default"); ok {
		t.Fatal("the default partition read as a month")
	}
	if !monthOf(time.Date(2026, 11, 30, 23, 0, 0, 0, time.FixedZone("x", -7*3600))).Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("months are UTC")
	}
}
