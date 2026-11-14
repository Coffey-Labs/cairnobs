package libvirt

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// fakeDaemon answers the five calls as libvirtd would, from domains, and
// records what it was asked.
type fakeDaemon struct {
	mu      sync.Mutex
	procs   []int32
	uri     string
	flags   uint32
	polkit  bool // ask for polkit
	refuse  string
	domains []fakeDomain
}

type fakeDomain struct {
	dom       domain
	params    []any // field, value pairs; values typed as libvirt sends them
	autostart bool
}

func (f *fakeDaemon) serve(t *testing.T, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go f.conn(t, c)
	}
}

func (f *fakeDaemon) conn(t *testing.T, c net.Conn) {
	defer c.Close()
	for {
		var lb [4]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(lb[:])-4)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		d := &decoder{b: body}
		prog, vers, proc, typ, serial, _ := d.u32(), d.u32(), d.i32(), d.i32(), d.u32(), d.i32()
		if prog != program || vers != version || typ != msgCall {
			t.Errorf("a call of %x/%d type %d", prog, vers, typ)
			return
		}
		f.mu.Lock()
		f.procs = append(f.procs, proc)
		f.mu.Unlock()
		var out encoder
		status := int32(statusOK)
		switch proc {
		case procAuthList:
			if f.polkit {
				out.u32(1)
				out.i32(authPolkit)
			} else {
				out.u32(0)
			}
		case procAuthPolkit:
			out.i32(1)
		case procConnectOpen:
			uri, _ := d.optString()
			f.mu.Lock()
			f.uri, f.flags = uri, d.u32()
			f.mu.Unlock()
		case procConnectClose:
		case procGetAllDomainStats:
			if f.refuse != "" {
				status = statusError
				out.i32(55) // VIR_ERR_OPERATION_INVALID
				out.i32(10)
				out.optString(f.refuse)
				out.i32(2)
				break
			}
			if n, groups := d.u32(), d.u32(); n != 0 || groups != statGroups {
				t.Errorf("stats for %d domains, groups %d", n, groups)
			}
			out.u32(uint32(len(f.domains)))
			for _, fd := range f.domains {
				out.domain(fd.dom)
				out.u32(uint32(len(fd.params) / 2))
				for i := 0; i < len(fd.params); i += 2 {
					out.string(fd.params[i].(string))
					switch v := fd.params[i+1].(type) {
					case int32:
						out.i32(1)
						out.i32(v)
					case uint32:
						out.i32(2)
						out.u32(v)
					case uint64:
						out.i32(4)
						out.u32(uint32(v >> 32))
						out.u32(uint32(v))
					case float64:
						out.i32(5)
						b := math.Float64bits(v)
						out.u32(uint32(b >> 32))
						out.u32(uint32(b))
					case bool:
						out.i32(6)
						if v {
							out.i32(1)
						} else {
							out.i32(0)
						}
					case string:
						out.i32(7)
						out.string(v)
					}
				}
			}
		case procDomainGetAutostart:
			name := d.domain().name
			v := int32(0)
			for _, fd := range f.domains {
				if fd.dom.name == name && fd.autostart {
					v = 1
				}
			}
			out.i32(v)
		default:
			t.Errorf("asked for procedure %d", proc)
			return
		}
		var h encoder
		h.u32(0)
		h.u32(program)
		h.u32(version)
		h.i32(proc)
		h.i32(msgReply)
		h.u32(serial)
		h.i32(status)
		msg := append(h.b, out.b...)
		binary.BigEndian.PutUint32(msg, uint32(len(msg)))
		if _, err := c.Write(msg); err != nil {
			return
		}
	}
}

func listen(t *testing.T, f *fakeDaemon) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "libvirt-sock-ro")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go f.serve(t, ln)
	return sock
}

var webUUID = [16]byte{0x23, 0xa0, 0x18, 0x93, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, 0, 0x01}

func TestRead(t *testing.T) {
	f := &fakeDaemon{polkit: true, domains: []fakeDomain{
		{dom: domain{name: "web", uuid: webUUID, id: 3}, autostart: true, params: []any{
			"state.state", int32(1), "state.reason", int32(1), "cpu.time", uint64(90e9),
			"balloon.current", uint64(4 << 20), "balloon.maximum", uint64(4 << 20), "balloon.rss", uint64(1 << 20),
			"vcpu.current", uint32(2), "vcpu.maximum", uint32(4), "vcpu.0.state", int32(1), "vcpu.0.time", uint64(45e9),
			"balloon.swap_in", uint64(0), "some.double", 1.5, "some.flag", true, "some.text", "odd length",
		}},
		{dom: domain{name: "spare", uuid: [16]byte{1: 1}, id: -1}, params: []any{
			"state.state", int32(5), "state.reason", int32(1), "balloon.current", uint64(2 << 20), "balloon.maximum", uint64(2 << 20),
			"vcpu.current", uint32(1),
		}},
	}}
	missing := filepath.Join(t.TempDir(), "nothing-here")
	sock := listen(t, f)
	doms, err := Read([]string{missing, sock}, "qemu:///system", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if f.uri != "qemu:///system" || f.flags != connectRO {
		t.Errorf("opened %q with flags %d", f.uri, f.flags)
	}
	want := []int32{procAuthList, procAuthPolkit, procConnectOpen, procGetAllDomainStats, procDomainGetAutostart, procDomainGetAutostart, procConnectClose}
	if len(f.procs) != len(want) {
		t.Fatalf("calls %v, want %v", f.procs, want)
	}
	for i := range want {
		if f.procs[i] != want[i] {
			t.Fatalf("calls %v, want %v", f.procs, want)
		}
	}
	if len(doms) != 2 {
		t.Fatalf("%+v", doms)
	}
	w := doms[0]
	if w.Name != "web" || w.UUID != "23a01893-0000-4000-8000-000000000001" || w.State != 1 || !w.Autostart || w.VCPUs != 2 ||
		w.MaxKiB != 4<<20 || w.RSSKiB != 1<<20 || w.CPUNs != 90e9 || w.Read == 0 {
		t.Errorf("web: %+v", w)
	}
	if s := doms[1]; s.Name != "spare" || s.State != 5 || s.Autostart || s.CPUNs != 0 {
		t.Errorf("spare: %+v", s)
	}

	// A minute later, web used 30 s of CPU: half a core.
	later := []Domain{w, doms[1]}
	later[0].CPUNs += 30e9
	later[0].Read += 60_000
	recs, use := Records(later, map[string]Domain{w.UUID: w, doms[1].UUID: doms[1]})
	if len(recs) != 2 || recs[0].Kind != report.KindVM || recs[0].ID != w.UUID || recs[0].State != report.StateOK ||
		recs[0].Facts["firmware_uuid"] != w.UUID || recs[0].Facts["memory_gib"] != 4.0 || recs[0].Facts["autostart"] != true {
		t.Errorf("records %+v", recs)
	}
	if recs[1].State != report.StateStopped || recs[1].Word != "Shut off" {
		t.Errorf("spare: %+v", recs[1])
	}
	if len(use) != 1 || use[0] != (report.Usage{ID: w.UUID, CPU: 0.5, Mem: 1 << 30, CPUOf: 2, MemOf: 4 << 30}) {
		t.Errorf("usage %+v", use)
	}
	if _, use := Records(later, nil); use != nil {
		t.Errorf("usage with nothing to compare: %+v", use)
	}
}

func TestReadErrors(t *testing.T) {
	if _, err := Read([]string{filepath.Join(t.TempDir(), "none")}, "qemu:///system", time.Second); !errors.Is(err, ErrAbsent) {
		t.Errorf("no socket: %v", err)
	}
	sock := listen(t, &fakeDaemon{refuse: "the hypervisor isn't answering"})
	_, err := Read([]string{sock}, "qemu:///system", 5*time.Second)
	var le *Error
	if !errors.As(err, &le) || !strings.Contains(err.Error(), "the hypervisor isn't answering") || le.Code != 55 {
		t.Errorf("refused: %v", err)
	}
}

func TestStateOf(t *testing.T) {
	for _, c := range []struct {
		state, reason int
		want          string
	}{
		{1, 1, report.StateOK}, {2, 0, report.StateOK}, {3, 1, report.StateDegraded}, {3, 2, report.StateUpdating},
		{3, 5, report.StateDown}, {4, 1, report.StateStopped}, {5, 1, report.StateStopped}, {5, 3, report.StateDown},
		{5, 6, report.StateDown}, {6, 1, report.StateDown}, {7, 1, report.StateStopped}, {0, 0, report.StateUnknown},
	} {
		if got, _ := stateOf(c.state, c.reason); got != c.want {
			t.Errorf("%d/%d: %s, want %s", c.state, c.reason, got, c.want)
		}
	}
}

// A reply cut short, or claiming a huge array, is an error, not a panic.
func TestDecoderBounds(t *testing.T) {
	d := &decoder{b: []byte{0, 0, 0, 9, 'a'}}
	if s := d.string(); s != "" || d.err == nil {
		t.Errorf("short string: %q %v", s, d.err)
	}
	d = &decoder{b: []byte{0xff, 0xff, 0xff, 0xff}}
	if n := d.count(16384); n != 0 || d.err == nil {
		t.Errorf("huge count: %d %v", n, d.err)
	}
	d = &decoder{b: []byte{0, 0, 0, 9}}
	if v := d.typedValue(); v != nil || d.err == nil {
		t.Errorf("unknown type: %v %v", v, d.err)
	}
}
