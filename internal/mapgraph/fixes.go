package mapgraph

// Corrections made on the page (docs/maps.md, Corrections): they key on
// the ids discovery gives (compose:<project>, unit:<name>, site:<prefix>,
// a part's host and owner), so they survive restarts and redeploys, and a
// later discovery never undoes them.

import (
	"net/netip"
	"path"
	"strings"
)

// EdgeRef names an edge between two services; port 0 is any port.
type EdgeRef struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
	Port int    `json:"port,omitempty" yaml:"port,omitempty"`
}

func (r EdgeRef) matches(e Edge) bool {
	return r.From == e.From && r.To == e.To && (r.Port == 0 || r.Port == e.Port)
}

// Outside is a service that isn't ours and can't be seen: a payment
// provider, a relay at another company. Connections to its addresses
// become edges to it; NeededBy adds edges no connection shows.
type Outside struct {
	ID       string   `json:"id" yaml:"id"` // outside:<slug>
	Name     string   `json:"name" yaml:"name"`
	Addrs    []string `json:"addrs,omitempty" yaml:"addrs,omitempty"` // address, address:port or prefix
	NeededBy []string `json:"needed_by,omitempty" yaml:"needed_by,omitempty"`
	Note     string   `json:"note,omitempty" yaml:"note,omitempty"`
}

// Matches says whether a connection to addr:port reaches it.
func (o Outside) Matches(addr netip.Addr, port int) bool {
	for _, a := range o.Addrs {
		if ap, err := netip.ParseAddrPort(a); err == nil {
			if ap.Addr() == addr && int(ap.Port()) == port {
				return true
			}
		} else if pf, err := netip.ParsePrefix(a); err == nil {
			if pf.Contains(addr) {
				return true
			}
		} else if x, err := netip.ParseAddr(a); err == nil && x == addr {
			return true
		}
	}
	return false
}

// ValidAddr says whether s is something Outside.Addrs can hold.
func ValidAddr(s string) bool {
	if _, err := netip.ParseAddrPort(s); err == nil {
		return true
	}
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}

// PartKey is how Moved names a part.
func PartKey(host, owner string) string { return host + " " + owner }

// Rejects says whether people said this edge isn't a dependency.
func (c Corrections) Rejects(e Edge) bool {
	for _, r := range c.Rejected {
		if r.matches(e) {
			return true
		}
	}
	return false
}

// MergedInto follows Merged to the service id finally in force, stopping
// at a loop.
func (c Corrections) MergedInto(id string) string {
	for range 8 {
		to, ok := c.Merged[id]
		if !ok || to == "" || to == id {
			return id
		}
		id = to
	}
	return id
}

// LabelOf is a service id's name when nothing better is known.
func LabelOf(id string) string {
	kind, rest, ok := strings.Cut(id, ":")
	if !ok {
		return id
	}
	if kind == "image" || kind == "k8s" {
		return path.Base(rest)
	}
	return rest
}

// sourceOf is where a service id comes from.
func (b *builder) sourceOf(id string) string {
	for _, d := range b.cor.Services {
		if d.ID == id {
			return "declared"
		}
	}
	kind, _, ok := strings.Cut(id, ":")
	switch {
	case ok && (kind == "compose" || kind == "unit" || kind == "process" || kind == "image" || kind == "outside"):
		return kind
	case ok && kind == "k8s":
		return "kubernetes"
	}
	return "page" // made on the page, by moving parts into it
}

// outsideFor is the outside service a connection to addr:port reaches.
func (b *builder) outsideFor(addr netip.Addr, port int) (*Service, bool) {
	for _, o := range b.cor.Outside {
		if o.Matches(addr, port) {
			return b.outsideService(o), true
		}
	}
	return nil, false
}

func (b *builder) outsideService(o Outside) *Service {
	s := b.svc[o.ID]
	if s == nil {
		s = &Service{ID: o.ID, Name: o.Name, Source: "outside", Kind: "outside", Declared: true}
		b.svc[o.ID] = s
	}
	return s
}
