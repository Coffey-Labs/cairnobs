package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// The fleet's Windows servers (`fleet config -windows N`, up to four),
// agents named win-*, outside the sites of 50:
//
//	win-dc01   a domain controller, all well
//	win-fs01   a file server with a disk going bad: bad blocks, and every
//	           8 hours it crashes and comes back (Kernel-Power 41, critical)
//	win-sql01  SQL Server: its Agent service should start by itself, and
//	           now and then stops (scenario.go); its certificate ends in a
//	           few days
//	win-hv01   a Hyper-V host with twelve VMs: one saved, one failed
type winServer struct {
	name, os, kernel string
	cpus             int
	memGiB           uint64
	units            []string
}

var winServers = []winServer{
	{"win-dc01", "Windows Server 2022 Standard 21H2", "10.0.20348.4294", 4, 16, []string{"NTDS", "DNS", "Netlogon", "Kdc", "W32Time", "DFSR"}},
	{"win-fs01", "Windows Server 2019 Standard 1809", "10.0.17763.7922", 4, 16, []string{"LanmanServer", "DFSR", "Netlogon", "W32Time"}},
	{"win-sql01", "Windows Server 2022 Datacenter 21H2", "10.0.20348.4294", 8, 64, []string{"MSSQLSERVER", "SQLSERVERAGENT", "W32Time"}},
	{"win-hv01", "Windows Server 2025 Datacenter 24H2", "10.0.26100.6899", 64, 512, []string{"vmms", "vmcompute", "W32Time"}},
}

const winPrefix = "win-"

func winServerFor(name string) (winServer, bool) {
	for _, w := range winServers {
		if w.name == name {
			return w, true
		}
	}
	return winServer{}, false
}

// winHost is what a Windows server reports beside the usual.
type winHost struct {
	w winServer
}

func newWindowsHost(name string, interval time.Duration) *host {
	w, _ := winServerFor(name)
	h := &host{start: time.Now().Add(-time.Duration(9+len(name)) * 24 * time.Hour), win: &winHost{w}}
	h.r = report.Report{
		V: report.Version, Agent: "fleet", Host: name, Interval: int(interval / time.Second),
		Kernel: w.kernel, OS: w.os, CPUs: w.cpus,
		Mem: report.Mem{Total: w.memGiB << 30, SwapTotal: 4 << 30, SwapFree: 4 << 30},
		Disks: []report.Disk{
			{Mount: `C:\`, FS: "NTFS", Size: 127 << 30},
			{Mount: `D:\`, Device: "Data", FS: "NTFS", Size: 2 << 40},
		},
		Sources: []string{},
	}
	if name == "win-hv01" {
		h.vms = hyperVVMs(time.Now())
	}
	h.files = winCerts(name)
	return h
}

// stamp adds the services and the event log, as the Windows agent sends
// them.
func (wh *winHost) stamp(h *host, r *report.Report, now time.Time) {
	w := wh.w
	crashes, lastCrash := 0, time.Time{}
	if w.name == "win-fs01" {
		// It goes down without shutting down cleanly and is straight back:
		// up since the last time.
		lastCrash = spellFSCrash.began(now)
		for t := lastCrash; now.Sub(t) < 24*time.Hour; t = t.Add(-spellFSCrash.every) {
			crashes++
		}
		h.start = lastCrash
		r.Uptime = max(0, now.Sub(h.start).Seconds())
	}
	r.Units = nil
	for _, u := range w.units {
		un := report.Unit{Name: u + ".service", Load: "loaded", Active: "active", Sub: "running", Since: h.start.UTC().Add(time.Minute).Truncate(time.Second)}
		if w.name == "win-sql01" && u == "SQLSERVERAGENT" {
			if b := spellSQLAgent.began(now); spellSQLAgent.active(now) {
				un.Active, un.Sub, un.Since = "failed", "dead", time.Time{}
			} else if b.After(h.start) { // someone started it again
				un.Since = b.Add(spellSQLAgent.on).UTC().Truncate(time.Second)
			}
		}
		r.Units = append(r.Units, un)
	}
	ev := &report.Events{At: now.UTC(), Since: now.Add(-24 * time.Hour).UTC(), List: []report.EventGroup{}}
	group := func(log, source string, id uint32, level string, count int, first, last time.Duration, msg string) {
		ev.List = append(ev.List, report.EventGroup{Log: log, Source: source, ID: id, Level: level, Count: count,
			First: now.Add(-first).UTC().Truncate(time.Second), Last: now.Add(-last).UTC().Truncate(time.Second), Message: msg})
	}
	switch w.name {
	case "win-fs01":
		first := lastCrash.Add(-time.Duration(crashes-1) * spellFSCrash.every)
		group("System", "Microsoft-Windows-Kernel-Power", 41, "critical", crashes, now.Sub(first), now.Sub(h.start),
			"The system has rebooted without cleanly shutting down first. This error could be caused if the system stopped responding, crashed, or lost power unexpectedly.")
		group("System", "disk", 7, "error", 14, 19*time.Hour, 2*time.Hour, `The device, \Device\Harddisk1\DR1, has a bad block.`)
		group("System", "NETLOGON", 5719, "error", 2*crashes, now.Sub(first), now.Sub(h.start),
			"This computer was not able to set up a secure session with a domain controller in domain CORP due to the following: There are currently no logon servers available to service the logon request.")
	case "win-sql01":
		group("System", "Service Control Manager", 7034, "error", 3, 6*time.Hour, now.Sub(spellSQLAgent.began(now)),
			"The SQL Server Agent (MSSQLSERVER) service terminated unexpectedly. It has done this 3 time(s).")
	case "win-hv01":
		group("System", "Microsoft-Windows-Hyper-V-Worker", 18560, "critical", 1, 3*time.Hour, 3*time.Hour,
			"'build-win02' was reset because an unrecoverable error occurred on a virtual processor that caused a triple fault.")
	}
	r.Events = ev
}

var hyperVNames = [libvirtVMs]string{"rds01", "rds02", "print01", "wsus01", "ca01", "archive01", "build-win01", "build-win02", "test-win11", "mecm01", "jump01", "legacy-2012"}

// hyperVVMs are win-hv01's VMs: running, but the old 2012 one is saved
// and a build machine has failed.
func hyperVVMs(now time.Time) []report.Workload {
	out := make([]report.Workload, len(hyperVNames))
	for k, name := range hyperVNames {
		id := vmUUID("hyperv-win-hv01", k)
		gen := 2
		if name == "legacy-2012" {
			gen = 1
		}
		r := report.Workload{Kind: report.KindVM, ID: id, Name: name,
			State: report.StateOK, Word: "Running", Since: now.Add(-time.Duration(5+k) * 24 * time.Hour).Truncate(time.Second),
			Facts: map[string]any{"hypervisor": "Hyper-V", "autostart": name != "legacy-2012", "generation": gen, "firmware_uuid": id,
				"vcpus": 2 << (k % 3), "memory_gib": float64(int(4) << (k % 3))}}
		switch name {
		case "legacy-2012":
			r.State, r.Word, r.Since = report.StateStopped, "Saved", now.Add(-41*24*time.Hour).Truncate(time.Second)
		case "build-win02":
			r.State, r.Word, r.Since = report.StateDown, "Hyper-V reports a failure", now.Add(-3*time.Hour).Truncate(time.Second)
		}
		out[k] = r
	}
	return out
}

// winCerts are what a Windows server's LocalMachine\My store holds: a
// machine certificate from the domain's CA, which on win-sql01 ends in
// a few days.
func winCerts(name string) *report.CertFiles {
	fqdn := name + ".corp.example.com"
	begun := time.Duration(60+len(name)*9) * 24 * time.Hour
	if name == "win-sql01" {
		begun = 360 * 24 * time.Hour
	}
	c := fakeCert("win-"+name, fqdn, []string{fqdn}, "Example Corp Issuing CA 01", "RSA 2048", begun, 365*24*time.Hour)
	c.Sig = "SHA256-RSA"
	thumb := sha1.Sum([]byte("fleet-store:" + name))
	return &report.CertFiles{List: []report.CertFile{{
		Path: `Cert:\LocalMachine\My\` + strings.ToUpper(hex.EncodeToString(thumb[:])), Mod: c.NotBefore, Cert: c}}}
}

// winExpected is expected.yaml's lines for the Windows servers.
func winExpected(n int) string {
	var b strings.Builder
	for _, w := range winServers[:min(n, len(winServers))] {
		fmt.Fprintf(&b, "  %s:\n    units: [%s]\n", w.name, strings.Join(w.units, ", "))
	}
	return b.String()
}
