// Package libvirt reads a host's VMs from libvirt's read-only socket
// (docs/cluster-watcher.md, A VM is a VM, wherever it runs). It speaks
// libvirt's RPC protocol (src/remote/remote_protocol.x, XDR over a unix
// socket) itself, and only the five calls it needs: open, the
// authentication list and polkit, every domain's stats, and autostart.
// Nothing else can be asked, so nothing can be changed, whatever the
// socket would allow.
package libvirt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"time"
)

const (
	program = 0x20008086 // REMOTE_PROGRAM
	version = 1          // REMOTE_PROTOCOL_VERSION

	procConnectOpen        = 1
	procConnectClose       = 2
	procDomainGetAutostart = 15
	procAuthList           = 66
	procAuthPolkit         = 70
	procGetAllDomainStats  = 344

	msgCall  = 0
	msgReply = 1

	statusOK    = 0
	statusError = 1

	authPolkit = 2 // REMOTE_AUTH_POLKIT

	connectRO = 1 // VIR_CONNECT_RO

	maxMessage = 32 << 20 // libvirt's own ceiling is 32 MiB
)

// conn is one connection to libvirtd (or virtqemud).
type conn struct {
	c      net.Conn
	serial uint32
}

func dial(socket string, timeout time.Duration) (*conn, error) {
	c, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	return &conn{c: c}, nil
}

func (c *conn) close() error { return c.c.Close() }

// Error is libvirt's answer when a call fails.
type Error struct {
	Code    int32
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("libvirt: %s (code %d)", e.Message, e.Code) }

// call sends one call and reads its reply's body. Events (which we never
// ask for) and anything not ours are skipped.
func (c *conn) call(proc int32, args []byte) (*decoder, error) {
	c.serial++
	var h encoder
	h.u32(0) // the length, filled in below
	h.u32(program)
	h.u32(version)
	h.i32(proc)
	h.i32(msgCall)
	h.u32(c.serial)
	h.i32(statusOK)
	msg := append(h.b, args...)
	binary.BigEndian.PutUint32(msg, uint32(len(msg)))
	if _, err := c.c.Write(msg); err != nil {
		return nil, err
	}
	for {
		var lb [4]byte
		if _, err := io.ReadFull(c.c, lb[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(lb[:])
		if n < 28 || n > maxMessage {
			return nil, fmt.Errorf("libvirt: a reply of %d bytes", n)
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(c.c, body); err != nil {
			return nil, err
		}
		d := &decoder{b: body}
		prog, vers, p, typ, serial, status := d.u32(), d.u32(), d.i32(), d.i32(), d.u32(), d.i32()
		if prog != program || vers != version || typ != msgReply || serial != c.serial || p != proc {
			continue
		}
		if status == statusError {
			code, _ := d.i32(), d.i32() // code, domain
			e := &Error{Code: code}
			if s, ok := d.optString(); ok {
				e.Message = s
			}
			if d.err != nil && e.Message == "" {
				e.Message = "an error it didn't describe"
			}
			return nil, e
		}
		if status != statusOK {
			return nil, fmt.Errorf("libvirt: reply status %d", status)
		}
		return d, d.err
	}
}

// open authenticates if the daemon asks for polkit (the read-only socket's
// default; root is always allowed) and opens a read-only connection.
func (c *conn) open(uri string) error {
	d, err := c.call(procAuthList, nil)
	if err != nil {
		return err
	}
	n := d.u32()
	for i := uint32(0); i < n && d.err == nil; i++ {
		if d.i32() == authPolkit {
			if _, err := c.call(procAuthPolkit, nil); err != nil {
				return err
			}
		}
	}
	if d.err != nil {
		return d.err
	}
	var e encoder
	e.optString(uri)
	e.u32(connectRO)
	_, err = c.call(procConnectOpen, e.b)
	return err
}

// stats asks for every domain's stats, running or not.
func (c *conn) stats(groups uint32) ([]record, error) {
	var e encoder
	e.u32(0) // no domains named: all of them
	e.u32(groups)
	e.u32(0)
	d, err := c.call(procGetAllDomainStats, e.b)
	if err != nil {
		return nil, err
	}
	n := d.count(16384)
	out := make([]record, 0, n)
	for i := 0; i < n && d.err == nil; i++ {
		r := record{dom: d.domain(), params: map[string]any{}}
		m := d.count(262144)
		for j := 0; j < m && d.err == nil; j++ {
			field := d.string()
			r.params[field] = d.typedValue()
		}
		out = append(out, r)
	}
	return out, d.err
}

func (c *conn) autostart(dom domain) (bool, error) {
	var e encoder
	e.domain(dom)
	d, err := c.call(procDomainGetAutostart, e.b)
	if err != nil {
		return false, err
	}
	v := d.i32()
	return v != 0, d.err
}

type domain struct {
	name string
	uuid [16]byte
	id   int32
}

type record struct {
	dom    domain
	params map[string]any // int64, uint64, float64, bool or string
}

// encoder writes XDR: big-endian 4-byte words, strings padded to 4.
type encoder struct{ b []byte }

func (e *encoder) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *encoder) i32(v int32)  { e.u32(uint32(v)) }

func (e *encoder) string(s string) {
	e.u32(uint32(len(s)))
	e.b = append(e.b, s...)
	e.b = append(e.b, make([]byte, (4-len(s)%4)%4)...)
}

// optString is a remote_string: a pointer, so present-or-not first.
func (e *encoder) optString(s string) {
	e.u32(1)
	e.string(s)
}

func (e *encoder) domain(d domain) {
	e.string(d.name)
	e.b = append(e.b, d.uuid[:]...)
	e.i32(d.id)
}

// decoder reads XDR, keeping the first error and returning zeros after it.
type decoder struct {
	b   []byte
	err error
}

var errShort = errors.New("libvirt: a reply cut short")

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || n > len(d.b) {
		d.err = errShort
		d.b = nil
		return nil
	}
	out := d.b[:n]
	d.b = d.b[n:]
	return out
}

func (d *decoder) u32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (d *decoder) i32() int32 { return int32(d.u32()) }

func (d *decoder) u64() uint64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// count reads an array's length, refusing one over max.
func (d *decoder) count(max int) int {
	n := d.u32()
	if int64(n) > int64(max) {
		if d.err == nil {
			d.err = fmt.Errorf("libvirt: an array of %d", n)
		}
		return 0
	}
	return int(n)
}

func (d *decoder) string() string {
	n := d.count(maxMessage)
	b := d.take(n)
	d.take((4 - n%4) % 4)
	return string(b)
}

func (d *decoder) optString() (string, bool) {
	if d.u32() == 0 {
		return "", false
	}
	return d.string(), d.err == nil
}

func (d *decoder) domain() domain {
	var out domain
	out.name = d.string()
	copy(out.uuid[:], d.take(16))
	out.id = d.i32()
	return out
}

// typedValue reads a remote_typed_param_value.
func (d *decoder) typedValue() any {
	switch t := d.i32(); t {
	case 1: // int
		return int64(d.i32())
	case 2: // uint
		return uint64(d.u32())
	case 3: // llong
		return int64(d.u64())
	case 4: // ullong
		return d.u64()
	case 5: // double
		return math.Float64frombits(d.u64())
	case 6: // boolean
		return d.i32() != 0
	case 7: // string
		return d.string()
	default:
		if d.err == nil {
			d.err = fmt.Errorf("libvirt: a typed parameter of type %d", t)
		}
		return nil
	}
}

// uuidString is the canonical form, lower case.
func uuidString(u [16]byte) string {
	var b bytes.Buffer
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b.WriteByte('-')
		}
		fmt.Fprintf(&b, "%02x", c)
	}
	return b.String()
}
