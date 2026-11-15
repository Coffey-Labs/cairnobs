package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cairnobs/cairnobs/internal/akamai"
	"github.com/cairnobs/cairnobs/internal/cloudflare"
	"github.com/cairnobs/cairnobs/internal/mailmon"
	"github.com/cairnobs/cairnobs/internal/maint"
	"github.com/cairnobs/cairnobs/internal/state"
	"github.com/cairnobs/cairnobs/internal/vault"
)

// Config is obs-beacond's config file.
type Config struct {
	Listen      string         `yaml:"listen"`       // the web UI and browser API, e.g. 127.0.0.1:8150 behind nginx
	AgentListen string         `yaml:"agent_listen"` // agents only, e.g. :8461; TLS with obs-beacond's own CA (agent_tls). Empty: agents use listen, with tokens
	AgentTLS    AgentTLSConfig `yaml:"agent_tls"`
	Join        JoinConfig     `yaml:"join"` // adding hosts from the UI
	// The emergency door: a loopback-only listener, reached through an SSH
	// tunnel, where emergency accounts (and only they) sign in. Every use
	// alerts all channels. Needs the local directory.
	EmergencyListen string        `yaml:"emergency_listen"`
	DevSignin       bool          `yaml:"dev_signin"`  // local development only; refused off loopback
	StaleAfter      time.Duration `yaml:"stale_after"` // a host is silent after this long without a report
	Expected        string        `yaml:"expected"`    // expected-containers file, optional
	Checks          string        `yaml:"checks"`      // active checks file (checks.yaml), optional
	Topology        string        `yaml:"topology"`    // corrections to the maps (topology.yaml), optional
	Backups         string        `yaml:"backups"`     // what each host's backups should be (backups.yaml), optional

	// Cairn OBS's own database (sessions, incidents, audit log). Without it
	// everything lives in memory and a restart forgets it.
	Database             string        `yaml:"database"`               // a SQLite file, e.g. /var/lib/obs-beacon/beacon.db, or a postgres:// URL
	DatabasePasswordFile string        `yaml:"database_password_file"` // PostgreSQL's password, kept out of the URL
	DataDir              string        `yaml:"data_dir"`               // obs-beacond's own files (agent CA, ACME cache); default: the SQLite file's folder
	StateKeyFile         string        `yaml:"state_key_file"`         // 32 random bytes, base64: seals provider tokens at rest
	Snapshot             string        `yaml:"snapshot"`               // a consistent copy for backups, e.g. /var/lib/obs-beacon/backup/beacon.db (SQLite)
	SnapshotEvery        time.Duration `yaml:"snapshot_every"`         // default 6h
	// Stopping (SIGTERM): /readyz turns 503 at once; after shutdown_delay
	// (so a load balancer or Kubernetes stops sending new requests) the
	// listeners close, and requests in flight get shutdown_grace to finish.
	ShutdownDelay time.Duration `yaml:"shutdown_delay"` // default 0; 5s on Kubernetes
	ShutdownGrace time.Duration `yaml:"shutdown_grace"` // default 30s
	TLS           struct {
		Cert string      `yaml:"cert"`
		Key  string      `yaml:"key"`
		ACME *ACMEConfig `yaml:"acme"` // get and renew the certificate itself
	} `yaml:"tls"`
	Agents []AgentConfig `yaml:"agents"`
	Signin SigninConfig  `yaml:"signin"`
	Alerts AlertsConfig  `yaml:"alerts"`
	// The data layer page: PostgreSQL via Patroni, and status pages agents read.
	DataLayer *DataLayerConfig `yaml:"datalayer"`
	// Daily windows (the nightly auto-reboots) when a host's alerts are held.
	Maintenance []maint.Window `yaml:"maintenance"`
	// Cloudflare checks with a read-only API token; off when left out.
	Cloudflare *cloudflare.Config `yaml:"cloudflare"`
	Akamai     *akamai.Config     `yaml:"akamai"` // docs/akamai.md
	// Mail as a person uses it, with a monitor account; off when left out.
	Mail *mailmon.Config `yaml:"mail"`
	// Reports' timezone and the weekly digest.
	Reports *ReportsConfig `yaml:"reports"`
	// Vault and OpenBao clusters obs-beacond reads itself (docs/vault.md).
	Vault []vault.Config `yaml:"vault"`
}

// AgentConfig names one agent and the SHA-256 of its token. obs-beacond never
// stores the token itself; `obs-beacond token` prints a new pair.
//
// A workstation (a desk machine, not a server) is watched but never pages:
// none of its conditions go above warning, its going silent included (it is
// switched off, or the office internet drops). It runs only the checks and
// probes that name it in `from`, so it never votes on whether a service is
// down, and its containers aren't judged, drawn or compared for drift.
type AgentConfig struct {
	Name        string `yaml:"name"`
	TokenSHA256 string `yaml:"token_sha256"`
	Workstation bool   `yaml:"workstation"`
	// A cluster agent (docs/cluster-watcher.md): it sends workloads, not
	// host reports, and isn't a host.
	Cluster bool `yaml:"cluster"`
}

// workstation reports whether the named agent is a workstation.
func (c *Config) workstation(name string) bool {
	for _, a := range c.Agents {
		if a.Name == name {
			return a.Workstation
		}
	}
	return false
}

// LoadConfig reads and checks a config file.
func LoadConfig(p string) (*Config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8150"
	}
	if c.Database != "" && c.StateKeyFile == "" {
		return nil, fmt.Errorf("%s: database needs state_key_file", p)
	}
	if state.IsPostgres(c.Database) {
		if c.Snapshot != "" {
			return nil, fmt.Errorf("%s: snapshot is for SQLite; back PostgreSQL up with its own tools (docs/formats.md, Database)", p)
		}
	} else if c.DatabasePasswordFile != "" {
		return nil, fmt.Errorf("%s: database_password_file is for a PostgreSQL database", p)
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = 30 * time.Second
	}
	if c.ShutdownDelay < 0 || c.ShutdownGrace < 0 {
		return nil, fmt.Errorf("%s: shutdown_delay and shutdown_grace can't be negative", p)
	}
	if c.SnapshotEvery == 0 {
		c.SnapshotEvery = 6 * time.Hour
	}
	if c.StaleAfter == 0 {
		c.StaleAfter = 60 * time.Second
	}
	if c.EmergencyListen != "" && !loopback(c.EmergencyListen) {
		return nil, fmt.Errorf("%s: emergency_listen must be a loopback address (it's reached through an SSH tunnel), not %s", p, c.EmergencyListen)
	}
	if c.DevSignin && !loopback(c.Listen) {
		return nil, fmt.Errorf("%s: dev_signin is only allowed on a loopback address, not %s", p, c.Listen)
	}
	c.Alerts.PublicURL = strings.TrimRight(c.Alerts.PublicURL, "/")
	if c.TLS.ACME != nil {
		if err := c.TLS.ACME.check(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if c.Alerts.PublicURL == "" {
			c.Alerts.PublicURL = publicFromACME(&c)
		}
	}
	ids := map[string]bool{}
	for i := range c.Signin.Providers {
		pc := &c.Signin.Providers[i]
		if err := pc.check(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if ids[pc.ID] {
			return nil, fmt.Errorf("%s: two sign-in providers called %q", p, pc.ID)
		}
		ids[pc.ID] = true
	}
	// Names of agents are checked in New, which knows the joined ones too.
	for i := range c.Maintenance {
		w := &c.Maintenance[i]
		if err := w.Check(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if len(w.Hosts) == 0 {
			return nil, fmt.Errorf("%s: maintenance window %q names no hosts", p, w.Name)
		}
	}
	if c.Reports != nil {
		if err := c.Reports.check(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if c.DataLayer != nil {
		if err := c.DataLayer.check(nil); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	vaults := map[string]bool{}
	for i := range c.Vault {
		v := &c.Vault[i]
		v.Defaults()
		if err := v.Check(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if !sourceName.MatchString(v.Name) || vaults[v.Name] {
			return nil, fmt.Errorf("%s: vault %d: name %q is missing, taken, or not a host name (a-z, 0-9, dots and dashes)", p, i+1, v.Name)
		}
		vaults[v.Name] = true
	}
	seen := map[string]bool{}
	for i, a := range c.Agents {
		a.TokenSHA256 = strings.ToLower(strings.TrimSpace(a.TokenSHA256))
		if a.Name == "" || seen[a.Name] {
			return nil, fmt.Errorf("%s: agent %d: missing or duplicate name %q", p, i+1, a.Name)
		}
		if a.Name == serverSource {
			return nil, fmt.Errorf("%s: agent %d: %q is obs-beacond's own name for what it reads itself", p, i+1, a.Name)
		}
		if b, err := hex.DecodeString(a.TokenSHA256); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("%s: agent %s: token_sha256 must be 64 hex characters", p, a.Name)
		}
		if a.Cluster && a.Workstation {
			return nil, fmt.Errorf("%s: agent %s: a cluster agent can't be a workstation", p, a.Name)
		}
		seen[a.Name] = true
		c.Agents[i] = a
	}
	return &c, nil
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return loopbackHost(host)
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DefaultDataDir is data_dir with a PostgreSQL database, unless set.
const DefaultDataDir = "/var/lib/obs-beacon"

// Dir is where obs-beacond keeps its own files (the agent CA's certificate,
// the ACME cache): data_dir, or else the SQLite file's folder. "" without a
// database.
func (c *Config) Dir() string {
	switch {
	case c.DataDir != "":
		return c.DataDir
	case c.Database == "":
		return ""
	case state.IsPostgres(c.Database):
		return DefaultDataDir
	}
	return filepath.Dir(c.Database)
}
