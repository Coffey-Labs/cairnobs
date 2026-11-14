package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

func write(t *testing.T, p string, size int, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "backups.yaml")
	os.WriteFile(p, []byte(`
hosts:
  host1:
    received:
      - {from: host2, root: /var/backups/host2, expect: [mastodon-db]}
    made:
      - {name: inbuxa-pg, paths: ["/var/backups/host*/inbuxa-pg/*.dump"], max_age: 30h}
    jobs: [gitea-backup.service]
`), 0o600)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	h := f.Hosts["host1"]
	if f.MaxAge != 26*time.Hour || h.Received[0].MaxAge != 26*time.Hour || h.Made[0].MaxAge != 30*time.Hour || f.Shrink != 0.5 {
		t.Errorf("defaults not filled in: %+v", f)
	}
	os.WriteFile(p, []byte("hosts:\n  host1:\n    made:\n      - {name: x, paths: [relative/*.dump]}\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative glob: err = %v", err)
	}
	os.WriteFile(p, []byte("hosts: {}\nmax_agee: 3h\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Error("an unknown field should be refused")
	}
}

func TestScanFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	write(t, root+"/host2/mastodon-db/20261014.tgz", 1000, now.Add(-49*time.Hour))
	write(t, root+"/host2/mastodon-db/20261015.tgz", 1200, now.Add(-25*time.Hour))
	write(t, root+"/host2/mastodon-db/.20261016.tgz.partial", 50, now) // rsync's temporary: not a copy
	write(t, root+"/host1/inbuxa-pg/a.dump", 300, now.Add(-30*time.Hour))
	write(t, root+"/nightly/inbuxa-pg.dump", 400, now.Add(-2*time.Hour))
	h := Host{
		Received: []Source{{From: "host2", Root: root + "/host2", Expect: []string{"mastodon-db", "games-scores"}}},
		Made:     []Made{{Name: "inbuxa-pg", Paths: []string{root + "/host*/inbuxa-pg/*.dump", root + "/nightly/inbuxa-pg.dump"}}},
	}
	var out report.Backups
	scanFiles(h, &out)
	if len(out.Received) != 2 {
		t.Fatalf("received = %+v", out.Received)
	}
	db, games := out.Received[0], out.Received[1]
	if db.Name != "mastodon-db" || db.From != "host2" || db.Count != 2 || db.Size != 1200 || db.PrevSize != 1000 || !strings.HasSuffix(db.Path, "20261015.tgz") {
		t.Errorf("mastodon-db = %+v", db)
	}
	if games.Count != 0 || games.Path != root+"/host2/games-scores" {
		t.Errorf("games-scores = %+v, want nothing found", games)
	}
	if len(out.Made) != 1 || out.Made[0].Size != 400 || out.Made[0].Count != 2 {
		t.Errorf("made = %+v, want the nightly dump as newest of 2", out.Made)
	}
	if len(out.Roots) != 1 || out.Roots[0].UsedPct <= 0 {
		t.Errorf("roots = %+v", out.Roots)
	}
}

func TestMdstat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mdstat")
	os.WriteFile(p, []byte(`Personalities : [raid1]
md1 : active raid1 sda2[0] sdb2[1]
      1046528 blocks super 1.2 [2/2] [UU]

md2 : active raid1 sda3[0]
      1943943232 blocks super 1.2 [2/1] [U_]
      bitmap: 3/15 pages [12KB], 65536KB chunk

unused devices: <none>
`), 0o600)
	got, err := mdstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (report.RAIDArray{Dev: "md1", State: "[UU]"}) || got[1] != (report.RAIDArray{Dev: "md2", State: "[U_]"}) {
		t.Errorf("mdstat = %+v", got)
	}
}

func TestJudge(t *testing.T) {
	now := time.Now()
	f := &File{MaxAge: 26 * time.Hour, Shrink: 0.5, DiskWarn: 0.9, Hosts: map[string]Host{"host1": {
		Received: []Source{{From: "host2", Root: "/b/host2", Expect: []string{"fresh", "old", "empty", "shrunk", "none"}, MaxAge: 26 * time.Hour}},
		Made:     []Made{{Name: "inbuxa-pg", Paths: []string{"/x/*.dump"}, MaxAge: 26 * time.Hour}},
		Jobs:     []string{"a.service", "b.service", "c.service"},
	}}}
	b := &report.Backups{At: now,
		Received: []report.BackupDir{
			{Name: "fresh", From: "host2", Count: 3, Size: 100, PrevSize: 90, Modified: now.Add(-time.Hour)},
			{Name: "old", From: "host2", Count: 3, Size: 100, Modified: now.Add(-30 * time.Hour)},
			{Name: "empty", From: "host2", Count: 1, Modified: now},
			{Name: "shrunk", From: "host2", Count: 2, Size: 40, PrevSize: 100, Modified: now},
			{Name: "none", From: "host2", Path: "/b/host2/none"},
		},
		Made:  []report.BackupDir{{Name: "inbuxa-pg", Count: 1, Size: 5, Modified: now}},
		Roots: []report.BackupRoot{{Path: "/b/host2", UsedPct: 93}},
		Jobs:  []report.BackupJob{{Unit: "a.service", Result: "success"}, {Unit: "b.service", Result: "exit-code"}, {Unit: "c.service"}},
		RAID:  []report.RAIDArray{{Dev: "md1", State: "[UU]"}, {Dev: "md2", State: "[U_]"}},
		SMART: []report.DiskHealth{{Disk: "sda", OK: true}, {Disk: "sdb"}, {Disk: "nvme0n1", Error: "Permission denied"}},
	}
	var got []string
	for _, x := range f.Judge("host1", b, now, 15*time.Minute) {
		got = append(got, x.Severity+":"+x.Key)
	}
	want := "serious:received/host2/old serious:received/host2/empty warning:received/host2/shrunk serious:received/host2/none " +
		"warning:disk//b/host2 serious:job/b.service warning:job/c.service critical:raid/md2 critical:smart/sdb warning:smart/nvme0n1"
	if strings.Join(got, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
	if got := f.Judge("host1", nil, now, 15*time.Minute); len(got) != 1 || got[0].Key != "scan" {
		t.Errorf("no scan: %+v", got)
	}
	b.Received, b.Made, b.Roots, b.Jobs, b.RAID, b.SMART = nil, nil, nil, nil, nil, nil
	b.At = now.Add(-2 * time.Hour)
	if got := f.Judge("host1", b, now, 15*time.Minute); len(got) < 1 || got[0].Key != "scan" || !strings.Contains(got[0].Title, "2 hours old") {
		t.Errorf("stale scan: %+v", got)
	}
	if got := f.Judge("host9", b, now, 15*time.Minute); got != nil {
		t.Errorf("a host backups.yaml doesn't name: %+v", got)
	}
}
