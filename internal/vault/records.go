package vault

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Thresholds for what ends: the same as the site checks' certificates
// (docs/vault.md).
const (
	warnBefore     = 21 * 24 * time.Hour
	criticalBefore = 7 * 24 * time.Hour
)

// TokenPart is the monitoring token's record name: a part of the cluster.
const TokenPart = "token/monitoring"

// Records makes a snapshot into the records a cluster agent would send:
// the cluster itself, its Raft servers as nodes, and the monitoring token
// as a part of the cluster; each PKI mount as a part, with its CAs and
// the newest certificate for each set of names it issued. Vault raises no alerts of its own, so these
// are judged here, by the rules in docs/vault.md.
func Records(s *Snapshot) []report.Workload {
	recs := []report.Workload{clusterRecord(s)}
	if s.Raft != nil {
		ids := make([]string, 0, len(s.Raft.Servers))
		for id := range s.Raft.Servers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			recs = append(recs, serverRecord(s.Raft.Servers[id]))
		}
	}
	if s.Health.Initialized && !s.Health.Sealed {
		recs = append(recs, tokenRecord(s))
		if m := s.Metrics; m != nil && m.HasAudit {
			recs = append(recs, auditRecord(s))
		}
		recs = append(recs, replicationRecords(s)...)
		if s.License != nil {
			recs = append(recs, licenseRecord(s))
		}
		recs = append(recs, pkiRecords(s)...)
	}
	return recs
}

func clusterRecord(s *Snapshot) report.Workload {
	h := s.Health
	name := h.ClusterName
	if name == "" {
		name = s.Name
	}
	r := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, State: report.StateOK, Word: "Unsealed, answering",
		Facts: map[string]any{"distribution": h.Product()}}
	if h.Version != "" {
		r.Facts["version"] = h.Version
	}
	switch {
	case !h.Initialized:
		r.State, r.Word = report.StateDown, "Not initialized: nothing can use it until it is"
		return r
	case h.Sealed:
		r.State, r.Word = report.StateDown, "Sealed: nothing can read a secret until it's unsealed"
		if s.Seal != nil && s.Seal.T > 0 && (s.Seal.Type == "shamir" || s.Seal.Type == "") {
			r.Word += fmt.Sprintf(" (%d of %d unseal keys entered)", s.Seal.Progress, s.Seal.T)
		}
		return r
	}
	var why []string
	if l := s.Leader; l != nil && l.HAEnabled {
		r.Facts["ha"] = true
		if l.LeaderAddress == "" {
			r.State, r.Word = report.StateDown, "No leader: none of its servers is active, so nothing is answered"
			return r
		}
		r.Facts["leader"] = l.LeaderAddress
	}
	if m := s.Metrics; m != nil && m.HasLeases {
		r.Facts["leases"] = m.Leases
		if m.Irrevocable > 0 {
			r.Facts["irrevocable_leases"] = m.Irrevocable
		}
	}
	if a := s.Raft; a != nil {
		r.Facts["storage"] = "raft"
		r.Facts["voters"] = len(a.Voters)
		r.Facts["failure_tolerance"] = a.FailureTolerance
		var sick []string
		for _, sv := range a.Servers {
			if !sv.Healthy || sv.NodeStatus != "alive" {
				sick = append(sick, or(sv.Name, sv.ID))
			}
		}
		sort.Strings(sick)
		switch {
		case !a.Healthy && len(sick) > 0:
			why = append(why, "its Raft isn't healthy: "+strings.Join(sick, ", "))
		case !a.Healthy:
			why = append(why, "its Raft isn't healthy")
		}
		if a.FailureTolerance == 0 && len(a.Voters) >= 3 {
			why = append(why, "it can't lose another server and keep answering")
		}
	}
	if s.RaftErr != "" {
		why = append(why, "can't read Raft's state: "+short(s.RaftErr, 80))
	}
	keys := make([]string, 0, len(s.Problems))
	for k := range s.Problems {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		why = append(why, "can't read "+k+": "+short(s.Problems[k], 80))
	}
	if len(why) > 0 {
		r.State, r.Word = report.StateDegraded, upper(strings.Join(why, "; "))
	}
	return r
}

func serverRecord(sv Server) report.Workload {
	r := report.Workload{Kind: report.KindNode, ID: "raft/" + sv.ID, Name: or(sv.Name, sv.ID), State: report.StateOK,
		Facts: map[string]any{"status": sv.Status}}
	if sv.Address != "" {
		r.Facts["address"] = sv.Address
	}
	if sv.Version != "" {
		r.Facts["version"] = sv.Version
	}
	if sv.LastContact != "" {
		r.Facts["last_contact"] = sv.LastContact
	}
	if t, err := time.Parse(time.RFC3339Nano, sv.StableSince); err == nil && t.Year() > 2000 {
		r.Since = t.UTC()
	}
	switch {
	case sv.NodeStatus != "" && sv.NodeStatus != "alive":
		r.State, r.Word = report.StateDown, upper(sv.NodeStatus)+": it isn't taking part"
	case !sv.Healthy:
		r.State, r.Word = report.StateDegraded, "Unhealthy"
		if sv.LastContact != "" {
			r.Word += ": last heard from " + sv.LastContact + " ago"
		}
	case sv.Status == "leader":
		r.Word = "Leader"
	case sv.Status == "voter":
		r.Word = "Voter"
	case sv.Status == "non-voter":
		r.Word = "Non-voter: a copy that doesn't vote"
	default:
		r.Word = upper(or(sv.Status, "healthy"))
	}
	return r
}

// tokenRecord is the monitoring token: when it ends, and whether it
// renews itself. A token nobody notices ending is a monitor gone blind.
func tokenRecord(s *Snapshot) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: "token", Name: TokenPart, State: report.StateOK, Facts: map[string]any{}}
	t := s.Token
	if t == nil {
		r.State, r.Word = report.StateDown, "Refused: "+short(s.TokenErr, 120)
		return r
	}
	if t.DisplayName != "" {
		r.Facts["display_name"] = t.DisplayName
	}
	if len(t.Policies) > 0 {
		r.Facts["policies"] = strings.Join(t.Policies, ", ")
	}
	r.Facts["renewable"] = t.Renewable
	ends := s.TokenEnds()
	if ends.IsZero() {
		r.Word = "Never ends"
		return r
	}
	r.Facts["ends"] = ends.UTC().Format(time.RFC3339)
	left := ends.Sub(s.At)
	how := "; it renews itself"
	if !t.Renewable {
		how = "; it can't be renewed: make a new one before then"
	}
	if s.RenewErr != "" {
		how = "; renewing it failed: " + short(s.RenewErr, 80)
	}
	r.Word = "Ends in " + days(left) + " (" + ends.UTC().Format("2006-01-02") + ")" + how
	// A token that renews itself in full isn't judged by its days: a
	// one-hour periodic token is fine. One that can't, or whose renewals
	// come back shorter (its max TTL near), is.
	if t.Renewable && s.RenewErr == "" && t.CreationTTL > 0 && t.TTL >= t.CreationTTL/2 {
		return r
	}
	switch {
	case left < criticalBefore:
		r.State = report.StateDown
	case left < warnBefore:
		r.State = report.StateDegraded
	}
	return r
}

// AuditPart is the audit log's record name: a part of the cluster.
const AuditPart = "audit/log"

// auditRecord is whether Vault could write its audit log. With every
// audit device failing Vault refuses requests, so a failure is critical,
// for auditHold after the count last went up.
func auditRecord(s *Snapshot) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: "audit", Name: AuditPart, State: report.StateOK, Word: "Written without failures",
		Facts: map[string]any{"failures": int64(s.Metrics.AuditFailures)}}
	if s.AuditNew > 0 && s.At.Sub(s.AuditFailed) <= auditHold {
		r.State = report.StateDown
		r.Word = fmt.Sprintf("Writing to it failed %s, last at %s UTC: Vault refuses what it can't audit", plural(int(s.AuditNew), "time", "times"), s.AuditFailed.UTC().Format("15:04"))
		r.Since = s.AuditFailed
	}
	return r
}

// Usage is what the cluster's history keeps: its lease count, hourly.
func (s *Snapshot) Usage() []report.Usage {
	if s.Metrics == nil || !s.Metrics.HasLeases {
		return nil
	}
	n := s.Metrics.Leases
	return []report.Usage{{ID: report.ClusterSelf, Leases: &n}}
}

// Unreachable is the last records with the cluster's own record saying
// its API doesn't answer: what was seen stays, as last seen.
func Unreachable(name string, last []report.Workload, err error) []report.Workload {
	out := make([]report.Workload, 0, max(1, len(last)))
	self := report.Workload{Kind: report.KindCluster, ID: report.ClusterSelf, Name: name, Facts: map[string]any{"distribution": "Vault"}}
	for _, r := range last {
		if r.ID == report.ClusterSelf {
			self = r
			continue
		}
		out = append(out, r)
	}
	self.State, self.Word = report.StateDown, "Can't reach its API: "+short(err.Error(), 160)
	return append([]report.Workload{self}, out...)
}

func days(d time.Duration) string {
	switch n := int(d.Hours() / 24); {
	case d < 0:
		return "the past"
	case n == 0:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case n == 1:
		return "1 day"
	default:
		return fmt.Sprintf("%d days", n)
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
