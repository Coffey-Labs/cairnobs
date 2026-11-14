package vault

import (
	"fmt"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// endedFor is how long an ended leaf certificate stays, critical, when
// nothing newer for its names came: then it goes (docs/vault.md).
const endedFor = 7 * 24 * time.Hour

// PKIPart is the start of a PKI mount's record name: "pki/pki_int".
const PKIPart = "pki/"

// pkiRecords are each mount (a part of the cluster, with its counts), its
// issuers and the newest certificate for each set of names it issued.
func pkiRecords(s *Snapshot) []report.Workload {
	var out []report.Workload
	for _, m := range s.PKI {
		subs, short := m.Subjects(s.MinLife)
		ending := 0
		var leaves []report.Workload
		for _, sub := range subs {
			ct := sub.Newest
			if s.At.Sub(ct.NotAfter) > endedFor {
				continue
			}
			life := ct.Lifetime()
			r := certRecord(m.Path, subjectID(m.Path, sub.Names), ct, s.At, min(warnBefore, life/3), min(criticalBefore, life/10))
			r.Facts["names"] = sub.Names
			r.Facts["certificates"] = sub.Count
			if ct.NotAfter.Sub(s.At) < warnBefore {
				ending++
			}
			leaves = append(leaves, r)
		}
		out = append(out, mountRecord(m, len(leaves), ending, short))
		for _, is := range m.Issuers {
			r := certRecord(m.Path, clip("issuer/"+m.Path+"/"+is.ID, 128), is.Cert, s.At, warnBefore, criticalBefore)
			r.Facts["ca"] = true
			r.Facts["issuer_name"] = is.Name
			out = append(out, r)
		}
		out = append(out, leaves...)
	}
	return out
}

// certRecord is one certificate, judged by the time it has left.
func certRecord(mount, id string, ct Cert, now time.Time, warn, crit time.Duration) report.Workload {
	r := report.Workload{Kind: report.KindCert, ID: id, Name: clip(ct.Subject, 300), Namespace: mount, State: report.StateOK,
		Since: ct.NotBefore, Facts: map[string]any{"ends": ct.NotAfter.Format(time.RFC3339), "serial": ct.Serial}}
	for k, v := range map[string]string{"fingerprint": ct.Fingerprint, "key": ct.Key, "signature": ct.Sig} {
		if v != "" {
			r.Facts[k] = v
		}
	}
	if ct.Issuer != "" {
		r.Facts["issued_by"] = ct.Issuer
	}
	left := ct.NotAfter.Sub(now)
	date := ct.NotAfter.Format("2006-01-02")
	switch {
	case left <= 0:
		r.State, r.Word = report.StateDown, "Ended "+ago(-left)+" ("+date+")"
	case left < crit:
		r.State, r.Word = report.StateDown, "Ends in "+days(left)+" ("+date+")"
	case left < warn:
		r.State, r.Word = report.StateDegraded, "Ends in "+days(left)+" ("+date+")"
	default:
		r.Word = "Ends in " + days(left) + " (" + date + ")"
	}
	return r
}

// mountRecord is a PKI mount: what it issued, in numbers. It's degraded
// when what it issued can't be listed or read.
func mountRecord(m Mount, current, ending, shortLived int) report.Workload {
	r := report.Workload{Kind: report.KindCluster, ID: PKIPart + m.Path, Name: PKIPart + m.Path, State: report.StateOK,
		Facts: map[string]any{"issuers": len(m.Issuers), "listed": m.Listed, "current": current, "ending": ending,
			"short_lived": shortLived, "revoked": m.Revoked, "pending": m.Pending}}
	var say []string
	switch {
	case m.Listed == 0:
		say = append(say, "Nothing issued")
	default:
		say = append(say, plural(current, "name", "names")+" with a current certificate")
		if ending > 0 {
			say = append(say, fmt.Sprintf("%d end within %d days", ending, int(warnBefore.Hours()/24)))
		}
		if shortLived > 0 {
			say = append(say, plural(shortLived, "short-lived certificate", "short-lived certificates")+", not judged")
		}
		if m.Revoked > 0 {
			say = append(say, fmt.Sprintf("%d revoked", m.Revoked))
		}
	}
	if m.Pending > 0 {
		say = append(say, fmt.Sprintf("still reading %d of %d", m.Pending, m.Listed))
	}
	r.Word = strings.Join(say, "; ")
	switch {
	case m.CertsErr != "":
		r.State, r.Word = report.StateDegraded, "Can't list its certificates: "+short(m.CertsErr, 120)
	case m.Err != "":
		r.State, r.Word = report.StateDegraded, "Can't read "+short(m.Err, 120)+"; "+lowerFirst(r.Word)
	}
	return r
}

func ago(d time.Duration) string {
	if d < time.Hour {
		return "just now"
	}
	return days(d) + " ago"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%s %s", thousands(n), many)
}

// thousands writes n with commas: 12,400.
func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
