package report

import "time"

// Map is what a host can say about where it sits and what it talks to, for
// the maps (docs/maps.md). The root helper reads it; the agent sends it.
// Nothing here is a payload, a command line or an Internet client's address.
type Map struct {
	Interfaces []Interface `json:"interfaces,omitempty"`
	Gateway    string      `json:"gateway,omitempty"` // the IPv4 default route's next hop
	WireGuard  []WGDevice  `json:"wireguard,omitempty"`
	Listen     []Listener  `json:"listen,omitempty"`
	Conns      []Conn      `json:"conns,omitempty"`
	// Cut says the socket sample was longer than the caps and was cut,
	// keeping the busiest lines.
	Cut   bool   `json:"cut,omitempty"`
	Error string `json:"error,omitempty"` // the sample couldn't be read (in part)
}

// Interface is one of the host's network interfaces with an address.
type Interface struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`  // physical, bridge, vlan, bond, wireguard, tun, virtual
	Addrs []string `json:"addrs"` // CIDR, e.g. 10.0.0.2/24; link-local ones left out
	Up    bool     `json:"up"`
}

// WGDevice is a WireGuard interface and its peers. Keys are base64, as
// `wg` prints them; only public keys are ever read out.
type WGDevice struct {
	Name       string   `json:"name"`
	PublicKey  string   `json:"public_key"`
	ListenPort int      `json:"listen_port,omitempty"`
	Peers      []WGPeer `json:"peers"`
}

// WGPeer is one peer of a WireGuard interface.
type WGPeer struct {
	PublicKey  string    `json:"public_key"`
	Endpoint   string    `json:"endpoint,omitempty"` // where it was last heard from, host:port
	AllowedIPs []string  `json:"allowed_ips,omitempty"`
	Handshake  time.Time `json:"handshake,omitzero"` // the latest; zero: never
	Rx         uint64    `json:"rx"`
	Tx         uint64    `json:"tx"`
}

// Listener is a socket waiting for connections (TCP) or datagrams (UDP).
type Listener struct {
	Owner string `json:"owner"` // see Conn.Owner
	Proto string `json:"proto"` // tcp or udp
	Addr  string `json:"addr"`  // "*" for every address
	Port  int    `json:"port"`
	// Inside is true for a listener in a network namespace of its own (a
	// container's, say), not the host's: reachable at that namespace's
	// addresses or through a published port, never at the host's own.
	// Agents before 0.1.0-rc.3 don't send it.
	Inside bool `json:"inside,omitempty"`
}

// Conn is a group of established TCP connections with the same owner,
// direction, far end and port.
type Conn struct {
	// Owner is what holds the socket: "container:<id>" (the agent puts the
	// name in when it knows the container), "unit:<name>" for a systemd
	// unit, "process:<name>" (the kernel's short name, never the command
	// line) for anything else, "" when no process holds it.
	Owner string `json:"owner"`
	Dir   string `json:"dir"` // out: this end connected; in: this end accepted
	// Remote is the far end's address. It's empty for connections coming in
	// from public addresses: an Internet client's address stays on the host.
	Remote string `json:"remote,omitempty"`
	Port   int    `json:"port"` // out: the far end's port; in: our listening port
	Count  int    `json:"count"`
	// Inside: held in a network namespace of its own (see Listener.Inside),
	// so its loopback is that namespace's, not the host's.
	Inside bool `json:"inside,omitempty"`
}
