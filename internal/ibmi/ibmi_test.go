package ibmi

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

	"github.com/cairnobs/cairnobs/internal/mapepire"
	"github.com/cairnobs/cairnobs/internal/report"
)

// partition is a fake partition behind a fake Mapepire, and a client for
// it that pins the fake's certificate.
func partition(t *testing.T) (*FakePartition, *mapepire.Fake, *Client) {
	t.Helper()
	p := NewFakePartition("prod1")
	f := &mapepire.Fake{User: "CAIRNOBS", Password: "pw\twith tab", Answer: p.Answer}
	srv := httptest.NewTLSServer(f.Handler())
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	pwf := filepath.Join(t.TempDir(), "password")
	os.WriteFile(pwf, []byte("pw\twith tab\n"), 0o600)
	c, err := New(Config{Address: strings.TrimPrefix(srv.URL, "https://"), User: "CAIRNOBS", PasswordFile: pwf, Fingerprint: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return p, f, c
}

func byID(recs []report.Workload) map[string]report.Workload {
	m := map[string]report.Workload{}
	for _, r := range recs {
		m[r.ID] = r
	}
	return m
}

func TestReadAndRecords(t *testing.T) {
	p, f, c := partition(t)
	ctx := context.Background()
	p.Elapsed, p.CPUUsed = 0, 0
	s, err := c.Read(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct != nil || Usage(s) != nil {
		t.Errorf("CPU on the first read: %v", *s.CPUPct)
	}
	// A minute on: 40% over it.
	p.Elapsed, p.CPUUsed = 60, 40
	if s, err = c.Read(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Two minutes on, 20% on average since the start: the second minute was 0%.
	p.Elapsed, p.CPUUsed = 120, 20
	s2, err := c.Read(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct == nil || *s.CPUPct != 40 || s2.CPUPct == nil || *s2.CPUPct != 0 {
		t.Errorf("CPU %v then %v", s.CPUPct, s2.CPUPct)
	}
	if s.Release != "7.5" || s.TR != 6 {
		t.Errorf("release %q TR %d", s.Release, s.TR)
	}
	u := Usage(s)
	if len(u) != 1 || u[0].CPU != 1 || u[0].CPUOf != 2.5 {
		t.Errorf("usage %+v", u)
	}
	recs := byID(Records(s))
	me := recs[report.ClusterSelf]
	if me.Name != "PROD1" || me.State != report.StateOK || me.Facts["version"] != "7.5 TR6" || me.Facts["model"] != "9105-42A" ||
		me.Facts["distribution"] != Distribution || me.Facts["serial"] != "78ABC12" {
		t.Errorf("self %+v", me)
	}
	sys := recs["asp/1"]
	if sys.Name != "asp/1" || sys.State != report.StateOK || sys.Facts["used_pct"] != 61.4 || sys.Word != "61% used of 2.4 TB" {
		t.Errorf("system ASP %+v", sys)
	}
	if ia := recs["asp/33"]; ia.Name != "asp/PRODIASP" || ia.State != report.StateOK {
		t.Errorf("IASP %+v", ia)
	}
	if b := recs["subsystem/QSYS/QINTER"]; b.Name != "subsystem/QINTER" || b.State != report.StateOK || b.Word != "Active, 41 jobs" {
		t.Errorf("QINTER %+v", b)
	}
	if b := recs["subsystem/QSYS/QSPL"]; b.State != report.StateStopped || b.Word != "Inactive" {
		t.Errorf("QSPL %+v", b)
	}
	if i := recs["interface/10.0.4.20"]; i.State != report.StateOK || i.Word != "Active on ETHLINE" {
		t.Errorf("interface %+v", i)
	}
	if i := recs["interface/10.0.9.20"]; i.State != report.StateStopped || i.Word != "Inactive on ETHBKP (not set to start with TCP/IP)" {
		t.Errorf("backup interface %+v", i)
	}
	if me.Facts["unanswered_inquiries"] != 0 {
		t.Errorf("an answered inquiry counted: %v", me.Facts)
	}
	if _, ok := recs["interface/127.0.0.1"]; ok || len(recs) != 1+2+6+2 {
		t.Errorf("loopback or link-local kept: %d records", len(recs))
	}
	// Every query was a SELECT of a view, and the connection was kept.
	for _, q := range f.Queries {
		if !strings.HasPrefix(q, "SELECT") || strings.Contains(q, "TABLE(") {
			t.Errorf("not a plain view query: %s", q)
		}
	}
}

func TestTrouble(t *testing.T) {
	p, _, c := partition(t)
	p.Attention, p.NoTR = true, true
	p.ASPs[0]["TOTAL_CAPACITY_AVAILABLE"] = 192_000.0 // 92% used
	p.ASPs[1]["ASP_STATE"] = "VARIED OFF"
	p.Disks[1]["HARDWARE_STATUS"] = "FAILED"
	p.Disks = append(p.Disks, mapepire.Row{"ASP_NUMBER": 1.0, "UNIT_NUMBER": 3.0, "RESOURCE_NAME": "DMP003", "HARDWARE_STATUS": "ACTIVE",
		"PROTECTION_STATUS": "ACTIVE", "SSD_PFA_WARNING": "YES"})
	p.Interfaces[1]["INTERFACE_STATUS"] = "RCYPND"
	// Newest first: a severity 99 message, two jobs waiting on the same
	// question, the old answered one, and a severity 80 one of two days ago.
	p.Messages = append([]mapepire.Row{
		Msg("0000B100", "", "CPF0907", "COMPLETION", "Serious storage condition may exist. Press HELP.", 99, "000000/QSYS/SCPF", 600),
		Msg("0000B000", "", "CPA0701", "INQUIRY", "MCH1211 received by procedure NIGHTLY. (C D I R)", 99, "123456/BATCH/NIGHTLY", 1800),
		Msg("0000AF00", "", "CPA0701", "INQUIRY", "MCH1211 received by procedure ORDERS. (C D I R)", 99, "123455/BATCH/ORDERS", 2400),
	}, append(p.Messages, Msg("00009000", "", "CPI0953", "INFORMATIONAL", "ASP storage limit exceeded.", 80, "000000/QSYS/SCPF", 2*86400))...)
	p.JobQueues = append(p.JobQueues, JobQ("QGPL", "NIGHTQ", true, 3, "QBATCH"), JobQ("ORDLIB", "ORDERSQ", false, 1, ""))
	p.Interfaces = append(p.Interfaces, iface("10.0.5.20", "ETHPROD2", "INACTIVE", true))
	p.ASPs = append(p.ASPs, mapepire.Row{"ASP_NUMBER": 2.0, "ASP_STATE": "NONE", "ASP_TYPE": "USER", "NUMBER_OF_DISK_UNITS": 2.0,
		"DISK_UNITS_PRESENT": "SOME", "TOTAL_CAPACITY": -2.0, "TOTAL_CAPACITY_AVAILABLE": -2.0, "STORAGE_THRESHOLD_PERCENTAGE": 90.0,
		"OVERFLOW_STORAGE": 1_500.0})
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	recs := byID(Records(s))
	if me := recs[report.ClusterSelf]; me.State != report.StateDegraded || !strings.Contains(me.Word, "attention light") || me.Facts["version"] != "7.5" {
		t.Errorf("self %+v", me)
	}
	if a := recs["asp/1"]; a.State != report.StateDegraded ||
		a.Word != "92% used, past IBM i's threshold of 90%; disk unit 2 (DMP002) failed; disk unit 3 (DMP003) predicts its own failure (SSD)" {
		t.Errorf("system ASP %+v", a)
	}
	if a := recs["asp/33"]; a.State != report.StateOK || a.Word != "Varied off" {
		t.Errorf("IASP %+v", a)
	}
	if a := recs["asp/2"]; a.State != report.StateDegraded || a.Word != "Some of its disk units weren't found; overflowed 2 GB into the system ASP" || a.Facts["used_pct"] != nil {
		t.Errorf("user ASP %+v", a)
	}
	if i := recs["interface/10.0.4.20"]; i.State != report.StateDown || i.Word != "Its line on ETHLINE is waiting for recovery (RCYPND)" {
		t.Errorf("interface %+v", i)
	}
	if i := recs["interface/10.0.5.20"]; i.State != report.StateDegraded || i.Word != "Inactive on ETHPROD2, though it's set to start with TCP/IP" {
		t.Errorf("second interface %+v", i)
	}
	if m := recs["message/0000B100"]; m.Name != "message/CPF0907 from 000000/QSYS/SCPF" || m.State != report.StateDown {
		t.Errorf("severe %+v", m)
	}
	q := recs["inquiry/0000B000"]
	if q.Name != "inquiry/CPA0701 from 123456/BATCH/NIGHTLY" || q.State != report.StateDegraded || q.Word != "MCH1211 received by procedure NIGHTLY. (C D I R)" ||
		q.Facts["job"] != "123456/BATCH/NIGHTLY" || q.Since.IsZero() {
		t.Errorf("inquiry %+v", q)
	}
	if _, ok := recs["inquiry/0000AF00"]; !ok {
		t.Error("the second job's inquiry")
	}
	for _, k := range []string{"inquiry/000098F0", "message/00009000", "jobqueue/QSYS/QBATCH"} {
		if _, ok := recs[k]; ok {
			t.Errorf("%s: answered, too old or fine, but listed", k)
		}
	}
	if j := recs["jobqueue/QGPL/NIGHTQ"]; j.Word != "Held, with 3 jobs waiting" {
		t.Errorf("held queue %+v", j)
	}
	if j := recs["jobqueue/ORDLIB/ORDERSQ"]; j.Word != "1 job waiting, and no active subsystem takes from it" {
		t.Errorf("orphan queue %+v", j)
	}
	if me := recs[report.ClusterSelf]; me.Facts["unanswered_inquiries"] != 2 || me.Facts["severe_messages"] != 1 {
		t.Errorf("counts %v", me.Facts)
	}
}

func TestUnreachable(t *testing.T) {
	_, _, c := partition(t)
	s, err := c.Read(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	recs := Unreachable("prod1", Records(s), errors.New("dial tcp: connection refused"))
	if recs[0].State != report.StateDown || recs[0].Name != "PROD1" || len(recs) != 11 {
		t.Errorf("%+v", recs)
	}
}

func TestBadConfig(t *testing.T) {
	if _, err := New(Config{Address: "prod1", User: "X", PasswordFile: "/nonexistent"}); err == nil {
		t.Error("no password file")
	}
	c := Config{Address: "Prod1.example.com:8076"}
	c.Defaults()
	if c.Name != "prod1.example.com" || c.Every != time.Minute {
		t.Errorf("%+v", c)
	}
}
