// Package backup is Cairn OBS's backup check: backups.yaml says what each host
// should receive from the others, what it should make and which jobs make
// it; Scan looks (as root, on a timer, with no network) and obs-beacond judges.
// It took over from hosts-baseline's backup-check.py.
package backup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/report"
)

// File is backups.yaml. The same file goes to every host; each host's scan
// reads its own section, and obs-beacond judges every section.
type File struct {
	MaxAge   time.Duration   `yaml:"max_age"`   // newest copy older than this; default 26h
	Shrink   float64         `yaml:"shrink"`    // newest under this share of the one before; default 0.5
	DiskWarn float64         `yaml:"disk_warn"` // a backup disk fuller than this share; default 0.9
	Hosts    map[string]Host `yaml:"hosts"`
}

// Host is what one host receives, makes and runs.
type Host struct {
	Received []Source `yaml:"received" json:"received"`
	Made     []Made   `yaml:"made" json:"made"`
	Jobs     []string `yaml:"jobs" json:"jobs"` // systemd services, e.g. gitea-backup.service
}

// Source is one sending host's directory: a subdirectory per backup, each
// holding dated copies.
type Source struct {
	From   string        `yaml:"from" json:"from"`
	Root   string        `yaml:"root" json:"root"`
	Expect []string      `yaml:"expect" json:"expect"`
	MaxAge time.Duration `yaml:"max_age" json:"max_age_ns"` // default: the file's
}

// Made is a backup whichever host is in charge makes (the database dump
// comes from the PostgreSQL leader): the newest file over every pattern.
type Made struct {
	Name   string        `yaml:"name" json:"name"`
	Paths  []string      `yaml:"paths" json:"paths"` // glob patterns
	MaxAge time.Duration `yaml:"max_age" json:"max_age_ns"`
}

// Load reads and checks backups.yaml.
func Load(p string) (*File, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if f.MaxAge == 0 {
		f.MaxAge = 26 * time.Hour
	}
	if f.Shrink == 0 {
		f.Shrink = 0.5
	}
	if f.DiskWarn == 0 {
		f.DiskWarn = 0.9
	}
	for name, h := range f.Hosts {
		for i := range h.Received {
			s := &h.Received[i]
			if s.From == "" || !filepath.IsAbs(s.Root) || len(s.Expect) == 0 {
				return nil, fmt.Errorf("%s: %s: each received entry needs from, an absolute root and expect", p, name)
			}
			if s.MaxAge == 0 {
				s.MaxAge = f.MaxAge
			}
		}
		for i := range h.Made {
			m := &h.Made[i]
			if m.Name == "" || len(m.Paths) == 0 {
				return nil, fmt.Errorf("%s: %s: each made entry needs a name and paths", p, name)
			}
			for _, g := range m.Paths {
				if _, err := filepath.Match(g, ""); err != nil || !filepath.IsAbs(g) {
					return nil, fmt.Errorf("%s: %s: %s: not an absolute glob pattern", p, name, g)
				}
			}
			if m.MaxAge == 0 {
				m.MaxAge = f.MaxAge
			}
		}
		f.Hosts[name] = h
	}
	return &f, nil
}

// Scan looks at one host. It only reads: directory listings and file
// sizes, never contents.
func Scan(ctx context.Context, h Host, now time.Time) report.Backups {
	out := report.Backups{At: now.UTC(), Received: []report.BackupDir{}, Made: []report.BackupDir{},
		Roots: []report.BackupRoot{}, Jobs: []report.BackupJob{}, RAID: []report.RAIDArray{}, SMART: []report.DiskHealth{}}
	fail := func(err error) { out.Errors = append(out.Errors, err.Error()) }
	scanFiles(h, &out)
	for _, u := range h.Jobs {
		j, err := job(ctx, u)
		if err != nil {
			fail(err)
		}
		out.Jobs = append(out.Jobs, j)
	}
	raid, err := mdstat("/proc/mdstat")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fail(err)
	}
	out.RAID = append(out.RAID, raid...)
	smart, err := smartAll(ctx)
	if err != nil {
		fail(err)
	}
	out.SMART = append(out.SMART, smart...)
	return out
}

// scanFiles fills in the received and made backups and their disks.
func scanFiles(h Host, out *report.Backups) {
	fail := func(err error) { out.Errors = append(out.Errors, err.Error()) }
	for _, s := range h.Received {
		for _, name := range s.Expect {
			d := newest(filepath.Join(s.Root, name))
			d.Name, d.From = name, s.From
			out.Received = append(out.Received, d)
		}
		if pct, err := usedPct(s.Root); err != nil {
			fail(fmt.Errorf("%s: %w", s.Root, err))
		} else if pct >= 0 {
			out.Roots = append(out.Roots, report.BackupRoot{Path: s.Root, UsedPct: pct})
		}
	}
	for _, m := range h.Made {
		var files []string
		for _, g := range m.Paths {
			got, _ := filepath.Glob(g)
			files = append(files, got...)
		}
		d := newestOf(files)
		d.Name = m.Name
		out.Made = append(out.Made, d)
	}
}

// newest is the newest regular file in dir; hidden files are skipped
// (rsync's temporaries start with a dot).
func newest(dir string) report.BackupDir {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return report.BackupDir{Path: dir}
	}
	var files []string
	for _, e := range ents {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	d := newestOf(files)
	if d.Count == 0 {
		d.Path = dir
	}
	return d
}

func newestOf(files []string) report.BackupDir {
	type f struct {
		path string
		size int64
		mod  time.Time
	}
	var list []f
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		list = append(list, f{p, st.Size(), st.ModTime()})
	}
	if len(list) == 0 {
		return report.BackupDir{}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	d := report.BackupDir{Path: list[0].path, Size: list[0].size, Modified: list[0].mod.UTC(), Count: len(list)}
	if len(list) > 1 {
		d.PrevSize = list[1].size
	}
	return d
}

func job(ctx context.Context, unit string) (report.BackupJob, error) {
	j := report.BackupJob{Unit: unit}
	out, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--timestamp=unix",
		"-p", "Result", "-p", "ActiveState", "-p", "LoadState", "-p", "InactiveEnterTimestamp").Output()
	if err != nil {
		return j, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	p := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			p[k] = v
		}
	}
	if p["LoadState"] == "not-found" {
		return j, nil // Result stays empty: obs-beacond says the job is missing
	}
	j.Result, j.Active = p["Result"], p["ActiveState"]
	if s, ok := strings.CutPrefix(p["InactiveEnterTimestamp"], "@"); ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			j.Finished = time.Unix(n, 0).UTC()
		}
	}
	return j, nil
}

// mdstat reads each array's member state: "[UU]" is healthy, an
// underscore is a missing member.
func mdstat(p string) ([]report.RAIDArray, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []report.RAIDArray
	dev := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "md") {
			dev, _, _ = strings.Cut(l, " ")
			continue
		}
		if dev != "" && strings.Contains(l, "blocks") {
			if i := strings.LastIndex(l, "["); i >= 0 && strings.HasSuffix(strings.TrimSpace(l), "]") {
				out = append(out, report.RAIDArray{Dev: dev, State: strings.TrimSpace(l[i:])})
				dev = ""
			}
		}
	}
	return out, sc.Err()
}

func smartAll(ctx context.Context) ([]report.DiskHealth, error) {
	b, err := exec.CommandContext(ctx, "lsblk", "-dnJ", "-o", "NAME,TYPE").Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var l struct {
		Devices []struct{ Name, Type string } `json:"blockdevices"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var out []report.DiskHealth
	for _, d := range l.Devices {
		if d.Type == "disk" {
			out = append(out, smart(ctx, d.Name))
		}
	}
	return out, nil
}

// smart asks smartctl for a disk's overall health. Its exit status is a
// bit mask that's often non-zero on healthy disks, so the JSON decides.
func smart(ctx context.Context, disk string) report.DiskHealth {
	h := report.DiskHealth{Disk: disk}
	b, err := exec.CommandContext(ctx, "smartctl", "-H", "-j", "/dev/"+disk).Output()
	var r struct {
		Status *struct {
			Passed bool `json:"passed"`
		} `json:"smart_status"`
		Smartctl struct {
			Messages []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
	}
	if jerr := json.Unmarshal(b, &r); jerr != nil || r.Status == nil {
		h.Error = "smartctl gave no verdict"
		if len(r.Smartctl.Messages) > 0 {
			h.Error = r.Smartctl.Messages[0].String
		} else if err != nil {
			h.Error = err.Error()
		}
		return h
	}
	h.OK = r.Status.Passed
	return h
}
