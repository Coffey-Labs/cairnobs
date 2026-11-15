package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

func TestBackupConditions(t *testing.T) {
	srv, _ := testServer(t, true)
	p := filepath.Join(t.TempDir(), "backups.yaml")
	os.WriteFile(p, []byte(`
hosts:
  node1:
    received:
      - {from: node2, root: /var/backups/node2, expect: [wiki-db]}
    jobs: [gitea-backup.service]
  node2:
    received:
      - {from: node1, root: /srv/backups/node1, expect: [gitea-db]}
`), 0o600)
	srv.backups.path = p
	now := time.Now()
	srv.store.Put("node1", report.Report{Backups: &report.Backups{At: now,
		Received: []report.BackupDir{{Name: "wiki-db", From: "node2", Count: 2, Size: 10, Modified: now.Add(-40 * time.Hour)}},
		Jobs:     []report.BackupJob{{Unit: "gitea-backup.service", Result: "success"}},
		RAID:     []report.RAIDArray{{Dev: "md2", State: "[_U]"}}}}, now)
	// node2 never reported: its own silence alerts, not its backups.
	got := keys(srv.backupConditions(now))
	if got != "serious:backup/node1/received/node2/wiki-db critical:backup/node1/raid/md2" {
		t.Errorf("got %q", got)
	}
	for _, c := range srv.backupConditions(now) {
		if c.Host != "node1" || c.Href != "/backups" {
			t.Errorf("%s: host %q href %q", c.Key, c.Host, c.Href)
		}
	}
	// The page lists both hosts, node2 as silent.
	rec := httptest.NewRecorder()
	srv.handleBackups(rec, httptest.NewRequest("GET", "/api/v1/backups", nil))
	var v struct {
		Hosts []BackupHostView `json:"hosts"`
	}
	json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Hosts) != 2 || v.Hosts[0].Host != "node1" || len(v.Hosts[0].Findings) != 2 || !v.Hosts[1].Silent || v.Hosts[1].Scan != nil {
		t.Errorf("page = %s", rec.Body.String())
	}
	// A file naming an unknown host is refused.
	os.WriteFile(p, []byte("hosts:\n  node9: {}\n"), 0o600)
	if _, err := loadBackups(p, srv.cfg.Agents); err == nil || !strings.Contains(err.Error(), "node9") {
		t.Errorf("err = %v", err)
	}
}
