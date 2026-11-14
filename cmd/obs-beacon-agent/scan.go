package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cairnobs/cairnobs/internal/backup"
	"github.com/cairnobs/cairnobs/internal/report"
)

// runScan is one backup scan, run by root from obs-beacon-scan.timer with no
// network. It writes the result where the unprivileged agent can read it.
func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cfg := fs.String("config", "/etc/obs-beacon/backups.yaml", "what to look for")
	host := fs.String("host", "", "this host's name in the file; default the agent's (from -agent-config)")
	agentCfg := fs.String("agent-config", "/etc/obs-beacon/agent.yaml", "the agent's config, for its name")
	out := fs.String("out", "/var/lib/obs-beacon-scan/backups.json", "where to write the scan")
	_ = fs.Parse(args)
	if *host == "" {
		c, err := loadConfig(*agentCfg)
		if err != nil {
			return fmt.Errorf("no -host, and the agent's config: %w", err)
		}
		*host = c.Host
	}
	f, err := backup.Load(*cfg)
	if err != nil {
		return err
	}
	h, ok := f.Hosts[*host]
	if !ok {
		return fmt.Errorf("%s has no section for %s", *cfg, *host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := json.Marshal(backup.Scan(ctx, h, time.Now()))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".scan-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), *out)
}

// backupScan is the latest scan, read again only when its file changes.
func (a *agent) backupScan() *report.Backups {
	if a.cfg.BackupScan == "" {
		return nil
	}
	st, err := os.Stat(a.cfg.BackupScan)
	if err != nil {
		a.once("scan:"+err.Error(), "backup scan: %v", err)
		return nil
	}
	if a.scan != nil && st.ModTime().Equal(a.scanTime) {
		return a.scan
	}
	b, err := os.ReadFile(a.cfg.BackupScan)
	if err != nil {
		a.once("scan:"+err.Error(), "backup scan: %v", err)
		return nil
	}
	var s report.Backups
	if err := json.Unmarshal(b, &s); err != nil {
		a.once("scan:"+err.Error(), "backup scan: %v", err)
		return nil
	}
	a.scan, a.scanTime = &s, st.ModTime()
	return a.scan
}
