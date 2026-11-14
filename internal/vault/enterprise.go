package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Vault Enterprise (docs/vault.md, step 4): replication, read when
// sys/health says a mode is on, and the license, read when it says
// Enterprise. Open-source Vault and OpenBao have neither, and nothing is
// asked of them.

// heartbeatLate is how old a replication heartbeat may be: they come
// every few seconds.
const heartbeatLate = 2 * time.Minute

// Replication is one kind's status: DR or performance.
type Replication struct {
	Mode               string   `json:"mode"`  // primary, secondary, bootstrapping, disabled
	State              string   `json:"state"` // running (a primary); stream-wals, merkle-diff, merkle-sync, idle (a secondary)
	ConnectionState    string   `json:"connection_state"`
	KnownSecondaries   []string `json:"known_secondaries"`
	Secondaries        []Peer   `json:"secondaries"`
	Primaries          []Peer   `json:"primaries"`
	LastWAL            int64    `json:"last_wal"`
	LastRemoteWAL      int64    `json:"last_remote_wal"`
	PrimaryClusterAddr string   `json:"primary_cluster_addr"`
	Corrupted          bool     `json:"corrupted_merkle_tree"`
}

// Peer is a secondary a primary streams to, or a secondary's primary.
type Peer struct {
	NodeID           string `json:"node_id"`
	APIAddress       string `json:"api_address"`
	ConnectionState  string `json:"connection_state"`  // a secondary's: ready, idle, connecting, transient_failure, shutdown
	ConnectionStatus string `json:"connection_status"` // a primary's: connected, disconnected
	LastHeartbeat    string `json:"last_heartbeat"`
}

// License is sys/license/status: the license in use.
type License struct {
	ID         string    `json:"license_id"`
	Expires    time.Time `json:"expiration_time"`
	Terminates time.Time `json:"termination_time"`
}

// readEnterprise reads replication and the license, when they're there.
func (c *Client) readEnterprise(ctx context.Context, s *Snapshot) {
	if on(s.Health.ReplDR) || on(s.Health.ReplPerf) {
		var raw json.RawMessage
		if err := c.get(ctx, "sys/replication/status", &raw); err != nil {
			if !notThere(err) {
				s.Problems["its replication"] = err.Error()
			}
		} else {
			var r struct {
				DR          *Replication `json:"dr"`
				Performance *Replication `json:"performance"`
			}
			if err := unwrap(raw, &r); err != nil {
				s.Problems["its replication"] = err.Error()
			} else {
				s.Repl = map[string]*Replication{}
				if r.DR != nil && on(r.DR.Mode) {
					s.Repl["dr"] = r.DR
				}
				if r.Performance != nil && on(r.Performance.Mode) {
					s.Repl["performance"] = r.Performance
				}
			}
		}
	}
	if s.Health.Enterprise {
		var raw json.RawMessage
		if err := c.get(ctx, "sys/license/status", &raw); err != nil {
			if !notThere(err) {
				s.Problems["its license"] = err.Error()
			}
			return
		}
		var l struct {
			Autoloaded *License `json:"autoloaded"`
			Persisted  *License `json:"persisted_autoload"`
		}
		if err := unwrap(raw, &l); err != nil {
			s.Problems["its license"] = err.Error()
			return
		}
		switch {
		case l.Autoloaded != nil && !l.Autoloaded.Expires.IsZero():
			s.License = l.Autoloaded
		case l.Persisted != nil && !l.Persisted.Expires.IsZero():
			s.License = l.Persisted
		}
	}
}

func on(mode string) bool { return mode != "" && mode != "disabled" }

// notThere is a path open-source Vault or OpenBao doesn't have.
func notThere(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	m := strings.ToLower(se.Msg)
	return se.Code == 404 || strings.Contains(m, "unsupported path") || strings.Contains(m, "enterprise-only")
}

// replicationRecords are a part per replication kind that's on.
func replicationRecords(s *Snapshot) []report.Workload {
	kinds := make([]string, 0, len(s.Repl))
	for k := range s.Repl {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var out []report.Workload
	for _, k := range kinds {
		out = append(out, replicationRecord(k, s.Repl[k], s.At))
	}
	return out
}

func replicationRecord(kind string, r *Replication, now time.Time) report.Workload {
	w := report.Workload{Kind: report.KindCluster, ID: "replication/" + kind, Name: "replication/" + kind, State: report.StateOK,
		Facts: map[string]any{"mode": r.Mode, "state": r.State}}
	if r.LastWAL > 0 {
		w.Facts["last_wal"] = r.LastWAL
	}
	switch {
	case r.Corrupted:
		w.State, w.Word = report.StateDown, "Its Merkle tree is corrupt: reindex it"
	case strings.HasPrefix(r.Mode, "primary"):
		w.Facts["secondaries"] = len(r.Secondaries)
		var bad []string
		for _, p := range r.Secondaries {
			if why := peerBad(p.ConnectionState, "ready", p.LastHeartbeat, now); why != "" {
				bad = append(bad, or(p.NodeID, p.APIAddress)+" ("+why+")")
			}
		}
		sort.Strings(bad)
		switch {
		case len(r.Secondaries) == 0 && len(r.KnownSecondaries) > 0:
			w.State, w.Word = report.StateDown, "Primary: none of its "+plural(len(r.KnownSecondaries), "secondary is", "secondaries are")+" connected"
		case len(r.Secondaries) == 0:
			w.Word = "Primary, with no secondaries yet"
		case len(bad) == len(r.Secondaries):
			w.State, w.Word = report.StateDown, "Primary: no secondary is streaming: "+strings.Join(bad, ", ")
		case len(bad) > 0:
			w.State, w.Word = report.StateDegraded, "Primary: "+strings.Join(bad, ", ")
		default:
			w.Word = "Primary, streaming to " + plural(len(r.Secondaries), "secondary", "secondaries")
		}
	case strings.HasPrefix(r.Mode, "secondary"):
		var bad []string
		for _, p := range r.Primaries {
			if why := peerBad(p.ConnectionStatus, "connected", p.LastHeartbeat, now); why != "" {
				bad = append(bad, or(p.APIAddress, p.NodeID)+" ("+why+")")
			}
		}
		switch {
		case r.State == "stream-wals" && len(bad) == 0:
			w.Word = "Secondary, streaming from its primary"
		case r.State == "merkle-diff" || r.State == "merkle-sync":
			w.State, w.Word = report.StateDegraded, "Secondary, catching up with its primary ("+r.State+")"
		case len(bad) > 0:
			w.State, w.Word = report.StateDown, "Secondary, not connected to its primary: "+strings.Join(bad, ", ")
		default:
			w.State, w.Word = report.StateDown, "Secondary, "+or(r.State, "not streaming")+": not following its primary"
		}
	case r.Mode == "bootstrapping":
		w.State, w.Word = report.StateDegraded, "Bootstrapping"
	default:
		w.Word = upper(r.Mode)
	}
	return w
}

// peerBad says what's wrong with a replication peer, or "".
func peerBad(state, good, heartbeat string, now time.Time) string {
	if state != "" && state != good {
		return strings.ReplaceAll(state, "_", " ")
	}
	if t, err := time.Parse(time.RFC3339Nano, heartbeat); err == nil && now.Sub(t) > heartbeatLate {
		return "last heard " + human(now.Sub(t)) + " ago"
	}
	return ""
}

// LicensePart is the license's record name: a part of the cluster.
const LicensePart = "license/vault"

// licenseRecord is the license's end: 21 and 7 days, as certificates.
// Vault Enterprise keeps running past expiration until termination, when
// it seals.
func licenseRecord(s *Snapshot) report.Workload {
	l := s.License
	r := report.Workload{Kind: report.KindCluster, ID: "license", Name: LicensePart, State: report.StateOK,
		Facts: map[string]any{"ends": l.Expires.UTC().Format(time.RFC3339)}}
	if l.ID != "" {
		r.Facts["license_id"] = l.ID
	}
	if !l.Terminates.IsZero() {
		r.Facts["terminates"] = l.Terminates.UTC().Format(time.RFC3339)
	}
	left := l.Expires.Sub(s.At)
	date := l.Expires.UTC().Format("2006-01-02")
	switch {
	case left <= 0:
		r.State, r.Word = report.StateDown, "Expired "+ago(-left)+" ("+date+")"
		if !l.Terminates.IsZero() {
			r.Word += fmt.Sprintf("; Vault seals when it terminates, %s", l.Terminates.UTC().Format("2006-01-02"))
		}
	case left < criticalBefore:
		r.State, r.Word = report.StateDown, "Expires in "+days(left)+" ("+date+")"
	case left < warnBefore:
		r.State, r.Word = report.StateDegraded, "Expires in "+days(left)+" ("+date+")"
	default:
		r.Word = "Expires in " + days(left) + " (" + date + ")"
	}
	return r
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return days(d)
}
