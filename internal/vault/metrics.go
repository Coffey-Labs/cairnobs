package vault

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Metrics: the lease count and audit failures, from sys/metrics in
// Prometheus' text format (docs/vault.md, step 3). Counters there are
// totals since the server started, so a failure is a total going up
// between reads.

// auditHold is how long an audit failure keeps the audit part critical
// after the count last went up.
const auditHold = 15 * time.Minute

// Metrics is what's read from sys/metrics.
type Metrics struct {
	Leases      int64 // vault.expire.num_leases: every lease, all mounts
	Irrevocable int64 // leases Vault tried and failed to revoke
	HasLeases   bool  // only the active server keeps the count
	// AuditFailures is the total of requests and responses an audit
	// device couldn't log; HasAudit is false with no audit device.
	AuditFailures float64
	HasAudit      bool
}

// metricNames are the series read; every other line is skipped.
var metricNames = map[string]bool{
	"vault_expire_num_leases":             true,
	"vault_expire_num_irrevocable_leases": true,
	"vault_audit_log_request_failure":     true,
	"vault_audit_log_response_failure":    true,
}

// parseMetrics reads the series in metricNames from Prometheus' text
// format, summing a series' label sets.
func parseMetrics(r io.Reader) (Metrics, error) {
	var m Metrics
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, rest, _ := strings.Cut(line, " ")
		if i := strings.IndexByte(name, '{'); i >= 0 {
			// Labels may hold spaces: the value is after the closing brace.
			j := strings.LastIndexByte(line, '}')
			if j < 0 {
				continue
			}
			name, rest = line[:i], strings.TrimSpace(line[j+1:])
		}
		if !metricNames[name] {
			continue
		}
		val, _, _ := strings.Cut(strings.TrimSpace(rest), " ") // a timestamp may follow
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		switch name {
		case "vault_expire_num_leases":
			m.Leases += int64(v)
			m.HasLeases = true
		case "vault_expire_num_irrevocable_leases":
			m.Irrevocable += int64(v)
		default:
			m.AuditFailures += v
			m.HasAudit = true
		}
	}
	return m, sc.Err()
}

// readMetrics reads sys/metrics, and keeps when the audit failures last
// went up: a lower total is a restarted server, not a failure.
func (c *Client) readMetrics(ctx context.Context, s *Snapshot) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/sys/metrics?format=prometheus", nil)
	if err != nil {
		s.Problems["its metrics"] = err.Error()
		return
	}
	t, err := c.readToken()
	if err != nil {
		s.Problems["its metrics"] = err.Error()
		return
	}
	req.Header.Set("X-Vault-Token", t)
	resp, err := c.hc.Do(req)
	if err != nil {
		s.Problems["its metrics"] = err.Error()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return // not Vault's answer: a fake, or a proxy that doesn't pass it
	}
	if resp.StatusCode != http.StatusOK {
		err := &StatusError{Code: resp.StatusCode, Msg: apiError(resp)}
		s.Problems["its metrics"] = err.Error()
		if strings.Contains(strings.ToLower(err.Msg), "prometheus is not enabled") {
			s.Problems["its metrics"] = "Prometheus metrics are off: set telemetry's prometheus_retention_time"
		}
		return
	}
	m, err := parseMetrics(io.LimitReader(resp.Body, 32<<20))
	if err != nil && !errors.Is(err, io.EOF) {
		s.Problems["its metrics"] = err.Error()
		return
	}
	s.Metrics = &m
	if m.HasAudit {
		if c.auditSeen && m.AuditFailures > c.auditTotal {
			c.auditFailed = s.At
			c.auditNew += m.AuditFailures - c.auditTotal
		}
		if s.At.Sub(c.auditFailed) > auditHold {
			c.auditNew = 0
		}
		c.auditTotal, c.auditSeen = m.AuditFailures, true
		s.AuditFailed, s.AuditNew = c.auditFailed, c.auditNew
	}
}
