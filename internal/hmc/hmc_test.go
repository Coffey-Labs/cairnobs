package hmc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// hmc is a fake HMC with two servers, and a client for it that pins the
// fake's certificate.
func hmc(t *testing.T) (*Fake, *Client) {
	t.Helper()
	f := &Fake{User: "cairnobs", Password: "pw <&> with tab\t", Console: "hmc1",
		Servers: []Server{NewFakeServer("p10-a", "78ABC12", 1), NewFakeServer("p10-b", "78ABC34", 2)}}
	srv := httptest.NewTLSServer(f.Handler())
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	pwf := filepath.Join(t.TempDir(), "password")
	os.WriteFile(pwf, []byte(f.Password+"\n"), 0o600)
	c, err := New(Config{URL: srv.URL, Username: "cairnobs", PasswordFile: pwf, Fingerprint: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return f, c
}

func byName(recs []report.Workload) map[string]report.Workload {
	m := map[string]report.Workload{}
	for _, r := range recs {
		if r.ID == report.ClusterSelf {
			m["self"] = r
			continue
		}
		m[r.Name] = r
	}
	return m
}

func TestReadAndRecords(t *testing.T) {
	f, c := hmc(t)
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Console != "hmc1" || len(s.Servers) != 2 || len(s.Servers[0].Partitions) != 4 || len(s.Problems) != 0 {
		t.Fatalf("snapshot %+v", s)
	}
	recs := Records(s)
	m := byName(recs)
	if me := m["self"]; me.Name != "hmc1" || me.State != report.StateOK || me.Facts["distribution"] != Distribution || me.Facts["servers"] != 2 {
		t.Errorf("self %+v", me)
	}
	a := m["p10-a"]
	if a.Kind != report.KindNode || a.State != report.StateOK || a.Word != "Operating, 4 of 4 partitions running" ||
		a.Facts["model"] != "9105-42A" || a.Facts["serial"] != "78ABC12" || a.Facts["memory_gib"] != 1024.0 || a.Facts["processor_units"] != 20.0 {
		t.Errorf("server %+v", a)
	}
	// Two servers each have a PROD1 and an aixdb1: the server goes after the name.
	p := m["PROD1 (p10-a)"]
	if p.Kind != report.KindVM || p.Node != "p10-a" || p.State != report.StateOK || p.Facts["os"] != "IBM i" || p.Facts["partition_id"] != 3 ||
		p.Facts["server_serial"] != "78ABC12" || p.Facts["vcpus"] != 4 || p.Facts["processors"] != "dedicated" || p.Facts["memory_gib"] != 256.0 ||
		p.Facts["reference_code"] != nil || p.Facts["rmc"] != nil || p.Facts["hypervisor"] != Hypervisor {
		t.Errorf("IBM i partition %+v", p)
	}
	if x := m["aixdb1 (p10-b)"]; x.Facts["os"] != "AIX" || x.Facts["processing_units"] != 2.5 || x.Facts["vcpus"] != 4 ||
		x.Facts["guest_os"] != "AIX 7.3 7300-03-00-2446" || x.Facts["rmc"] != "active" {
		t.Errorf("AIX partition %+v", x)
	}
	if v := m["p10-a-vios1"]; v.Facts["os"] != "VIOS" || v.State != report.StateOK {
		t.Errorf("VIOS %+v", v)
	}
	if len(recs) != 1+2+8 {
		t.Errorf("%d records", len(recs))
	}
	// One logon, and nothing else but GETs.
	for _, call := range f.Calls {
		if !strings.HasPrefix(call, "GET /rest/api/uom/") && call != "PUT /rest/api/web/Logon" {
			t.Errorf("call %s", call)
		}
		if strings.HasPrefix(call, "GET") && !strings.HasSuffix(call, "?group=None") {
			t.Errorf("not group=None: %s", call)
		}
	}
	// The session is kept between reads, and made again once it lapses.
	if _, err := c.Read(context.Background(), time.Now()); err != nil || f.Logons != 1 {
		t.Errorf("second read: %v, %d logons", err, f.Logons)
	}
	f.Expire()
	if _, err := c.Read(context.Background(), time.Now()); err != nil || f.Logons != 2 {
		t.Errorf("after the session lapsed: %v, %d logons", err, f.Logons)
	}
	c.Close()
	if last := f.Calls[len(f.Calls)-1]; last != "DELETE /rest/api/web/Logon" {
		t.Errorf("no logoff: %s", last)
	}
}

func TestTrouble(t *testing.T) {
	f, c := hmc(t)
	f.mu.Lock()
	a, b := &f.Servers[0], &f.Servers[1]
	a.AttentionLED = "true"
	a.Partitions[1].State = "not activated"                              // vios2
	a.Partitions[3].State, a.Partitions[3].RefCode = "error", "B2008105" // aixdb1
	a.Partitions[0].RMC = "inactive"                                     // vios1
	a.Partitions = append(a.Partitions, Partition{UUID: "AB-1", ID: 9, Name: "lnx1", State: "open firmware", Type: "AIX/Linux",
		OS: "Linux/Red Hat 5.14.0-427.el9.ppc64le 9.4", Dedicated: "false", ProcUnits: 0.5, VirtProcs: 1, MemMB: 8192},
		Partition{UUID: "AB-2", ID: 10, Name: "test1", State: "not activated", Type: "AIX/Linux", OS: "Unknown"},
		Partition{UUID: "AB-3", ID: 11, Name: "lpm1", State: "migrating running", Type: "AIX/Linux"})
	b.State = "no connection"
	f.mu.Unlock()
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 0 {
		t.Errorf("a server with no connection is a problem of the HMC's: %v", s.Problems)
	}
	m := byName(Records(s))
	want := map[string][2]string{
		"p10-a":       {report.StateDegraded, "Its attention light is on: a serviceable event is open on the HMC"},
		"p10-a-vios1": {report.StateDegraded, "Running, but the HMC can't reach it over RMC (inactive)"},
		"p10-a-vios2": {report.StateDegraded, "Not activated: the partitions it serves have one path less"},
		"aixdb1":      {report.StateDown, "Error, reference code B2008105"},
		"lnx1":        {report.StateDown, "Stopped in Open Firmware: its operating system isn't running"},
		"test1":       {report.StateStopped, "Not activated"},
		"lpm1":        {report.StateUpdating, "Moving to another server (Live Partition Mobility)"},
		"p10-b":       {report.StateDown, "The HMC has no connection to its service processor"},
	}
	for name, w := range want {
		if r := m[name]; r.State != w[0] || r.Word != w[1] {
			t.Errorf("%s: %s %q, want %s %q", name, r.State, r.Word, w[0], w[1])
		}
	}
	if x := m["lnx1"]; x.Facts["os"] != "Linux" || x.Facts["guest_os"] != "Linux/Red Hat 5.14.0-427.el9.ppc64le 9.4" {
		t.Errorf("Linux %+v", x)
	}
	if _, ok := m["PROD1 (p10-b)"]; ok {
		t.Error("partitions of a server with no connection, read")
	}
}

func TestUnreachable(t *testing.T) {
	_, c := hmc(t)
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	recs := Unreachable("hmc1.example.org", Records(s), errors.New("dial tcp: connection refused"))
	if recs[0].State != report.StateDown || recs[0].Name != "hmc1" || len(recs) != 11 {
		t.Errorf("%+v", recs[0])
	}
}

func TestBadLogon(t *testing.T) {
	f, c := hmc(t)
	f.mu.Lock()
	f.Password = "changed"
	f.mu.Unlock()
	_, err := c.Read(context.Background(), time.Now())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 401 || !strings.Contains(err.Error(), "User name or password is not valid") {
		t.Errorf("%v", err)
	}
}

func TestConfig(t *testing.T) {
	if _, err := New(Config{URL: "http://hmc1", Username: "x", PasswordFile: "/dev/null"}); err == nil {
		t.Error("http")
	}
	if _, err := New(Config{URL: "https://hmc1", PasswordFile: "/dev/null"}); err == nil {
		t.Error("no user")
	}
	c := Config{URL: "https://HMC1.example.org"}
	c.Defaults()
	if c.Name != "hmc1.example.org" || c.Every != time.Minute {
		t.Errorf("%+v", c)
	}
	cl, err := New(Config{URL: "https://hmc1.example.org", Username: "x", PasswordFile: "/dev/null"})
	if err != nil || cl.base != "https://hmc1.example.org:12443/rest/api" {
		t.Errorf("%v %+v", err, cl)
	}
}
