package main

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

// One generator per service. Each returns a record whose *message* reads
// like the real thing that service writes (combined-log-format nginx
// lines, Postgres duration/statement lines, Stalwart-shaped SMTP events,
// sshd/UFW journald lines) and whose *attributes* carry the structured
// fields those messages contain. Both halves matter: the message is what
// free-text search (`message:"connection refused"`) matches, the
// attributes are what `where status>=500` and `stats ... by path`
// aggregate over, and a demo that only had one of them would leave half
// the query language with nothing to show.

var (
	// Two IP pools, deliberately distinct: legitimate client traffic in
	// documentation ranges, and a small set of "attacker" addresses that
	// recur across sshd failures and UFW blocks, so a viewer who spots
	// one in the Security dashboard can pivot on it and find the rest.
	clientIPs = []string{
		"203.0.113.14", "203.0.113.72", "203.0.113.109", "198.51.100.7",
		"198.51.100.42", "192.0.2.28", "192.0.2.155", "203.0.113.201",
	}
	attackerIPs = []string{
		"45.155.205.233", "185.191.171.12", "89.248.165.74", "141.98.11.60",
	}

	userAgents = []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:135.0) Gecko/20100101 Firefox/135.0",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_3 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Mobile/15E148 Safari/604.1",
		"curl/8.11.1",
		"shop-mobile-android/4.12.0 (okhttp/4.12.0)",
		"Googlebot/2.1 (+http://www.google.com/bot.html)",
	}

	// Routes are shared between the nginx and api generators on purpose:
	// the same request shows up in the edge tier's access log and the
	// application tier's own log, which is exactly what makes a
	// "requests by path" panel comparable across services.
	routes = []struct {
		method string
		path   string
		route  string
		weight int
		slow   bool
	}{
		{"GET", "/", "/", 14, false},
		{"GET", "/products", "/products", 12, false},
		{"GET", "/products/%d", "/products/:id", 16, false},
		{"GET", "/api/v1/cart", "/api/v1/cart", 9, false},
		{"POST", "/api/v1/cart/items", "/api/v1/cart/items", 7, false},
		{"POST", "/api/v1/checkout", "/api/v1/checkout", 5, true},
		{"GET", "/api/v1/orders", "/api/v1/orders", 6, false},
		{"GET", "/api/v1/orders/%d", "/api/v1/orders/:id", 5, false},
		{"POST", "/api/v1/auth/login", "/api/v1/auth/login", 6, false},
		{"GET", "/api/v1/search", "/api/v1/search", 8, true},
		{"GET", "/static/app.%s.js", "/static/*", 10, false},
		{"GET", "/healthz", "/healthz", 4, false},
		{"POST", "/api/v1/webhooks/stripe", "/api/v1/webhooks/stripe", 3, false},
	}
	routeWeightTotal int

	regions   = []string{"us-east-1", "us-west-2", "eu-west-1"}
	appVerson = "shop-api@2026.8.3"
)

func init() {
	for _, r := range routes {
		routeWeightTotal += r.weight
	}
}

func pickRoute(r *rand.Rand) (method, path, route string, slow bool) {
	n := r.Intn(routeWeightTotal)
	for _, rt := range routes {
		if n -= rt.weight; n < 0 {
			p := rt.path
			switch {
			case strings.Contains(p, "%d"):
				p = fmt.Sprintf(p, 1000+r.Intn(9000))
			case strings.Contains(p, "%s"):
				p = fmt.Sprintf(p, hexString(r, 8))
			}
			return rt.method, p, rt.route, rt.slow
		}
	}
	return "GET", "/", "/", false
}

func hexString(r *rand.Rand, n int) string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigits[r.Intn(16)]
	}
	return string(b)
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

func newRecord(h *host, service string, t time.Time, sev logsv1.Severity, msg string, attrs map[string]string) *logsv1.LogRecord {
	return &logsv1.LogRecord{
		TimestampUnixNano: t.UnixNano(),
		Host:              h.name,
		Service:           service,
		Severity:          sev,
		Message:           msg,
		Attributes:        attrs,
	}
}

// severityForStatus keeps the severity column and the status attribute
// telling the same story -- a 500 that logged at INFO would make
// `severity=ERROR` and `where status>=500` disagree, and a demo where
// two obvious queries contradict each other is worse than one with less
// data.
func severityForStatus(status int) logsv1.Severity {
	switch {
	case status >= 500:
		return logsv1.Severity_SEVERITY_ERROR
	case status >= 400:
		return logsv1.Severity_SEVERITY_WARN
	default:
		return logsv1.Severity_SEVERITY_INFO
	}
}

func nginxRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	method, path, route, slow := pickRoute(r)
	clientIP := pick(r, clientIPs)

	// The edge tier mirrors whatever the API tier is doing: during the
	// outage window a good share of upstream requests come back 5xx here
	// too. Outside one it still isn't zero -- a real edge always returns
	// the occasional 502 from an upstream restarting or a slow request
	// tripping proxy_read_timeout, and a demo whose 5xx count is exactly
	// zero for six days straight makes every 5xx panel and rule look
	// broken rather than healthy.
	upstreamErrRate := c.apiErrorRate
	if upstreamErrRate == 0 {
		upstreamErrRate = 0.008
	}
	status := 200
	switch {
	case r.Float64() < upstreamErrRate:
		status = pick(r, []int{502, 503, 504})
	case r.Float64() < 0.04:
		status = pick(r, []int{404, 401, 403, 429})
	case r.Float64() < 0.06:
		status = 301
	}

	base := 40 + r.Float64()*180
	if slow {
		base *= 3
	}
	latency := base * c.latencyMult * (0.6 + r.Float64()*0.9)
	bytes := 400 + r.Intn(60000)
	referrer := "-"
	if r.Float64() < 0.55 {
		referrer = "https://shop.example.com" + pick(r, []string{"/", "/products", "/cart"})
	}
	ua := pick(r, userAgents)

	msg := fmt.Sprintf(`%s - - [%s] "%s %s HTTP/1.1" %d %d %q %q %.3f`,
		clientIP, t.UTC().Format("02/Jan/2006:15:04:05 -0700"),
		method, path, status, bytes, referrer, ua, latency/1000)

	attrs := map[string]string{
		"remote_addr": clientIP,
		"method":      method,
		"path":        path,
		"route":       route,
		"status":      strconv.Itoa(status),
		"bytes":       strconv.Itoa(bytes),
		"latency_ms":  fmt.Sprintf("%.1f", latency),
		"referrer":    referrer,
		"user_agent":  ua,
		"vhost":       "shop.example.com",
	}

	// A slice of edge-tier traffic is error-log lines rather than
	// access-log ones -- the same thing a real nginx host ships from two
	// files under one service.
	if status >= 500 && r.Float64() < 0.5 {
		upstream := fmt.Sprintf("10.0.2.%d:8080", 21+r.Intn(3))
		msg = fmt.Sprintf(`%s [error] %d#0: *%d connect() failed (111: Connection refused) while connecting to upstream, client: %s, server: shop.example.com, request: "%s %s HTTP/1.1", upstream: "http://%s%s"`,
			t.UTC().Format("2006/01/02 15:04:05"), 1000+r.Intn(900), r.Intn(90000), clientIP, method, path, upstream, path)
		attrs["upstream_addr"] = upstream
		attrs["log_kind"] = "error"
	} else {
		attrs["log_kind"] = "access"
	}

	return newRecord(h, "nginx", t, severityForStatus(status), msg, attrs)
}

var apiErrors = []struct {
	code   string
	detail string
}{
	{"db_pool_exhausted", "could not acquire a database connection: pool exhausted after 5000ms"},
	{"upstream_timeout", "payment provider request timed out after 30s"},
	{"null_reference", "unhandled exception in OrderService.finalize: nil pointer dereference"},
	{"serialization_failure", "could not serialize access due to concurrent update"},
	{"rate_limited_upstream", "inventory service returned 429, giving up after 3 retries"},
}

func apiRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	method, path, route, slow := pickRoute(r)
	traceID := hexString(r, 16)
	userID := fmt.Sprintf("u_%d", 1000+r.Intn(4000))
	region := pick(r, regions)

	errRate := c.apiErrorRate
	if errRate == 0 {
		errRate = 0.012 // the healthy baseline: a real service is never at exactly zero
	}

	status := 200
	switch {
	case r.Float64() < errRate:
		status = pick(r, []int{500, 503})
	case r.Float64() < 0.05:
		status = pick(r, []int{400, 401, 404, 422, 429})
	case method == "POST" && r.Float64() < 0.3:
		status = 201
	}

	base := 25 + r.Float64()*120
	if slow {
		base *= 3.5
	}
	latency := base * c.latencyMult * (0.6 + r.Float64()*0.8)
	dbTime := latency * (0.2 + r.Float64()*0.5)

	attrs := map[string]string{
		"method":     method,
		"path":       path,
		"route":      route,
		"status":     strconv.Itoa(status),
		"latency_ms": fmt.Sprintf("%.1f", latency),
		"db_time_ms": fmt.Sprintf("%.1f", dbTime),
		"trace_id":   traceID,
		"user_id":    userID,
		"region":     region,
		"version":    appVerson,
	}

	var msg string
	if status >= 500 {
		e := pick(r, apiErrors)
		attrs["error_code"] = e.code
		msg = fmt.Sprintf("%s %s -> %d in %.0fms trace=%s: %s", method, path, status, latency, traceID, e.detail)
	} else {
		msg = fmt.Sprintf("%s %s -> %d in %.0fms trace=%s user=%s region=%s", method, path, status, latency, traceID, userID, region)
	}
	return newRecord(h, "api", t, severityForStatus(status), msg, attrs)
}

var workerJobs = []struct {
	name  string
	queue string
	msMin int
	msMax int
}{
	{"order.confirmation_email", "email", 120, 900},
	{"report.daily_sales", "reports", 4000, 22000},
	{"inventory.reconcile", "inventory", 800, 6000},
	{"image.thumbnail", "media", 200, 2500},
	{"search.reindex", "search", 3000, 30000},
	{"webhook.retry", "webhooks", 100, 1500},
}

func workerRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	j := pick(r, workerJobs)
	dur := float64(j.msMin+r.Intn(j.msMax-j.msMin)) * c.latencyMult
	queueDepth := r.Intn(40)
	attempt := 1
	if r.Float64() < 0.12 {
		attempt = 2 + r.Intn(2)
	}

	attrs := map[string]string{
		"job":         j.name,
		"queue":       j.queue,
		"duration_ms": fmt.Sprintf("%.0f", dur),
		"attempt":     strconv.Itoa(attempt),
		"queue_depth": strconv.Itoa(queueDepth),
	}

	failRate := c.jobFailureRate
	if failRate == 0 {
		failRate = 0.05 // normal operation: retries exist because jobs do fail
	}

	switch {
	case r.Float64() < failRate:
		attrs["result"] = "failed"
		reason := pick(r, []string{
			"SMTP connection refused by relay",
			"request timeout after 30s calling inventory-service",
			"deadlock detected while updating orders",
		})
		return newRecord(h, "worker", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("job %s failed after %d attempts in %.0fms: %s", j.name, attempt, dur, reason), attrs)
	case queueDepth > 32:
		attrs["result"] = "ok"
		return newRecord(h, "worker", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("job %s completed in %.0fms but queue %s is backing up (depth=%d)", j.name, dur, j.queue, queueDepth), attrs)
	default:
		attrs["result"] = "ok"
		return newRecord(h, "worker", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("job %s completed in %.0fms (queue=%s attempt=%d)", j.name, dur, j.queue, attempt), attrs)
	}
}

var pgStatements = []struct {
	kind  string
	table string
	sql   string
	msMin int
	msMax int
}{
	{"SELECT", "orders", "SELECT o.*, c.email FROM orders o JOIN customers c ON c.id = o.customer_id WHERE o.customer_id = $1 ORDER BY o.created_at DESC LIMIT 50", 4, 120},
	{"SELECT", "products", "SELECT * FROM products WHERE tsv @@ plainto_tsquery($1) LIMIT 100", 30, 900},
	{"INSERT", "order_items", "INSERT INTO order_items (order_id, product_id, qty, price_cents) VALUES ($1, $2, $3, $4)", 2, 40},
	{"UPDATE", "inventory", "UPDATE inventory SET on_hand = on_hand - $1 WHERE sku = $2", 3, 250},
	{"SELECT", "sessions", "SELECT * FROM sessions WHERE token = $1", 1, 20},
	{"DELETE", "carts", "DELETE FROM carts WHERE updated_at < now() - interval '30 days'", 200, 4000},
}

func postgresRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	// A minority of Postgres log volume is lifecycle/connection noise
	// rather than statement logging, same as the real thing.
	if r.Float64() < 0.18 {
		switch r.Intn(3) {
		case 0:
			conns := 20 + r.Intn(90)
			return newRecord(h, "postgres", t, logsv1.Severity_SEVERITY_INFO,
				fmt.Sprintf("connection authorized: user=shop_app database=shop application_name=%s SSL enabled", appVerson),
				map[string]string{"db": "shop", "db_user": "shop_app", "connections": strconv.Itoa(conns), "query_kind": "connect"})
		case 1:
			return newRecord(h, "postgres", t, logsv1.Severity_SEVERITY_INFO,
				fmt.Sprintf("checkpoint complete: wrote %d buffers (%.1f%%); %d WAL file(s) added; sync=%.3f s, total=%.3f s",
					800+r.Intn(4000), r.Float64()*8, r.Intn(4), r.Float64(), 1+r.Float64()*6),
				map[string]string{"db": "shop", "query_kind": "checkpoint"})
		default:
			return newRecord(h, "postgres", t, logsv1.Severity_SEVERITY_WARN,
				fmt.Sprintf("could not receive data from client: Connection reset by peer (pid=%d)", 2000+r.Intn(8000)),
				map[string]string{"db": "shop", "query_kind": "connection_error", "pid": strconv.Itoa(2000 + r.Intn(8000))})
		}
	}

	s := pick(r, pgStatements)
	dur := float64(s.msMin+r.Intn(s.msMax-s.msMin)) * c.latencyMult
	attrs := map[string]string{
		"db":          "shop",
		"db_user":     "shop_app",
		"query_kind":  s.kind,
		"table":       s.table,
		"duration_ms": fmt.Sprintf("%.1f", dur),
		"rows":        strconv.Itoa(r.Intn(500)),
		"pid":         strconv.Itoa(2000 + r.Intn(8000)),
	}
	sev := logsv1.Severity_SEVERITY_DEBUG
	if dur > 1000 {
		sev = logsv1.Severity_SEVERITY_WARN
	}
	return newRecord(h, "postgres", t, sev,
		fmt.Sprintf("duration: %.3f ms  statement: %s", dur, s.sql), attrs)
}

func redisRecord(h *host, t time.Time, r *rand.Rand, _ conditions) *logsv1.LogRecord {
	used := int64(3<<30) + r.Int63n(2<<30)
	clients := 40 + r.Intn(160)
	attrs := map[string]string{
		"used_memory_bytes": strconv.FormatInt(used, 10),
		"connected_clients": strconv.Itoa(clients),
	}
	switch n := r.Intn(10); {
	case n < 4:
		attrs["op"] = "bgsave"
		return newRecord(h, "redis", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Background saving terminated with success (%d changes in %d seconds)", 10000+r.Intn(50000), 60), attrs)
	case n < 6:
		evicted := 200 + r.Intn(4000)
		attrs["op"] = "evict"
		attrs["keys_evicted"] = strconv.Itoa(evicted)
		return newRecord(h, "redis", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("Evicted %d keys to stay under maxmemory (used_memory=%.1fGB)", evicted, float64(used)/float64(1<<30)), attrs)
	case n < 8:
		attrs["op"] = "client"
		return newRecord(h, "redis", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Accepted %s:%d (connected_clients=%d)", pick(r, []string{"10.0.2.21", "10.0.2.22", "10.0.2.23", "10.0.3.31"}), 40000+r.Intn(20000), clients), attrs)
	case n < 9:
		attrs["op"] = "replication"
		return newRecord(h, "redis", t, logsv1.Severity_SEVERITY_INFO,
			"Synchronization with replica 10.0.4.52:6379 succeeded", attrs)
	default:
		attrs["op"] = "slowlog"
		micros := 12000 + r.Intn(90000)
		attrs["latency_ms"] = fmt.Sprintf("%.1f", float64(micros)/1000)
		return newRecord(h, "redis", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("slowlog entry: KEYS session:* took %d usec", micros), attrs)
	}
}

var mailDomains = []string{"example.com", "example.net", "mail.example.org", "shop.example.com", "gmail.com", "outlook.com"}

func smtpRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	queueID := hexString(r, 12)
	remote := pick(r, clientIPs)
	if c.spamWave || r.Float64() < 0.15 {
		remote = pick(r, attackerIPs)
	}
	sender := pick(r, mailDomains)
	rcpt := pick(r, []string{"shop.example.com", "cairnobs.example.com"})
	size := 2000 + r.Intn(400000)

	authFailRate := 0.06
	spamRate := 0.08
	if c.spamWave {
		authFailRate = 0.45
		spamRate = 0.35
	}

	attrs := map[string]string{
		"queue_id":      queueID,
		"remote_addr":   remote,
		"sender_domain": sender,
		"rcpt_domain":   rcpt,
		"size_bytes":    strconv.Itoa(size),
	}

	switch {
	case r.Float64() < authFailRate:
		user := pick(r, []string{"admin", "postmaster", "info", "sales", "test"})
		attrs["result"] = "auth_failed"
		attrs["auth_user"] = user
		return newRecord(h, "smtp", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("auth-not-allowed: authentication failed for user %q from [%s] (mechanism=PLAIN)", user, remote), attrs)
	case r.Float64() < spamRate:
		score := 6 + r.Float64()*12
		attrs["result"] = "spam_reject"
		attrs["spam_score"] = fmt.Sprintf("%.1f", score)
		return newRecord(h, "smtp", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("spam-reject: message <%s@%s> from [%s] rejected, score %.1f above threshold 5.0", queueID, sender, remote, score), attrs)
	case r.Float64() < 0.1:
		attrs["result"] = "deferred"
		return newRecord(h, "smtp", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("deferred: <%s> to @%s temporarily rejected (450 4.2.1 mailbox busy), retry in 15m", queueID, rcpt), attrs)
	default:
		attrs["result"] = "delivered"
		return newRecord(h, "smtp", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("delivered: <%s> from @%s to @%s size=%d in %.2fs", queueID, sender, rcpt, size, 0.2+r.Float64()*3), attrs)
	}
}

// internetFacing reports whether a host takes connections straight off
// the internet. Only these see a probe window: an internal host behind
// the edge tier keeps its ordinary background noise either way, and
// pretending otherwise would show a brute-force burst arriving
// simultaneously on hosts that aren't reachable at all.
func internetFacing(h *host) bool { return internetFacingName(h.name) }

func internetFacingName(name string) bool {
	return name == "mail-01" || strings.HasPrefix(name, "edge-")
}

// systemRecord is the journald stream every Linux host ships: sshd, UFW,
// systemd units, and the occasional kernel message. This is the stream
// the Security dashboard and the brute-force/UFW alert rules read.
func systemRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	pid := strconv.Itoa(400 + r.Intn(30000))

	// During a probe window an internet-facing host's system stream is
	// dominated by failed logins and firewall blocks.
	probing := c.bruteForce && internetFacing(h)
	roll := r.Float64()
	if probing {
		roll *= 0.35
	}

	switch {
	case roll < 0.22:
		src := pick(r, attackerIPs)
		user := pick(r, []string{"admin", "root", "ubuntu", "oracle", "postgres", "git", "test"})
		port := 40000 + r.Intn(20000)
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("sshd[%s]: Failed password for invalid user %s from %s port %d ssh2", pid, user, src, port),
			map[string]string{
				"unit": "ssh.service", "pid": pid, "remote_addr": src,
				"ssh_user": user, "src_port": strconv.Itoa(port), "auth_result": "failed",
			})
	case roll < 0.4:
		src := pick(r, attackerIPs)
		dport := pick(r, []int{22, 23, 445, 3389, 5432, 6379, 8080, 3306})
		sport := 40000 + r.Intn(20000)
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("kernel: [UFW BLOCK] IN=eth0 OUT= MAC=00:16:3e:%s SRC=%s DST=%s LEN=60 TOS=0x00 PREC=0x00 TTL=52 ID=%d PROTO=TCP SPT=%d DPT=%d WINDOW=1024 SYN",
				hexString(r, 2)+":"+hexString(r, 2)+":"+hexString(r, 2), src, h.ipv4, r.Intn(65000), sport, dport),
			map[string]string{
				"unit": "kernel", "remote_addr": src, "ufw_action": "BLOCK",
				"dst_port": strconv.Itoa(dport), "src_port": strconv.Itoa(sport), "proto": "TCP",
			})
	case roll < 0.52:
		user := pick(r, []string{"john", "deploy", "ansible"})
		src := pick(r, []string{"203.0.113.5", "10.0.0.9", "198.51.100.7"})
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("sshd[%s]: Accepted publickey for %s from %s port %d ssh2: ED25519 SHA256:%s", pid, user, src, 40000+r.Intn(20000), hexString(r, 20)),
			map[string]string{
				"unit": "ssh.service", "pid": pid, "remote_addr": src,
				"ssh_user": user, "auth_result": "accepted",
			})
	case roll < 0.62:
		user := pick(r, []string{"john", "deploy"})
		cmd := pick(r, []string{"/usr/bin/systemctl restart shop-api.service", "/usr/bin/apt-get update", "/usr/bin/journalctl -u shop-worker -n 200"})
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("sudo: %s : TTY=pts/0 ; PWD=/home/%s ; USER=root ; COMMAND=%s", user, user, cmd),
			map[string]string{"unit": "sudo", "ssh_user": user, "command": cmd})
	case roll < 0.8:
		unit := pick(r, []string{"logrotate.service", "apt-daily.service", "systemd-tmpfiles-clean.service", "fstrim.service"})
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("systemd[1]: %s: Deactivated successfully.", unit),
			map[string]string{"unit": unit, "pid": "1"})
	case roll < 0.92:
		unit := pick(r, []string{"shop-api.service", "shop-worker.service", "nginx.service", "cairnobs-agent.service"})
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("systemd[1]: Reloaded %s.", unit),
			map[string]string{"unit": unit, "pid": "1"})
	case roll < 0.97:
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("kernel: TCP: request_sock_TCP: Possible SYN flooding on port 443. Sending cookies. Check SNMP counters."),
			map[string]string{"unit": "kernel", "dst_port": "443"})
	default:
		proc := pick(r, []string{"python3", "node", "ruby"})
		return newRecord(h, "system", t, logsv1.Severity_SEVERITY_FATAL,
			fmt.Sprintf("kernel: Out of memory: Killed process %s (%s) total-vm:%dkB, anon-rss:%dkB", pid, proc, 2000000+r.Intn(4000000), 1000000+r.Intn(3000000)),
			map[string]string{"unit": "kernel", "pid": pid, "process": proc})
	}
}

var winEvents = []struct {
	id       string
	provider string
	channel  string
	sev      logsv1.Severity
	message  string
	weight   int
}{
	{"4624", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_INFO, "An account was successfully logged on.", 20},
	{"4625", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_WARN, "An account failed to log on.", 10},
	{"4634", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_INFO, "An account was logged off.", 14},
	{"4688", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_INFO, "A new process has been created.", 12},
	{"4720", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_WARN, "A user account was created.", 2},
	{"4740", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_WARN, "A user account was locked out.", 3},
	{"7036", "Service Control Manager", "System", logsv1.Severity_SEVERITY_INFO, "The Windows Update service entered the running state.", 16},
	{"7031", "Service Control Manager", "System", logsv1.Severity_SEVERITY_ERROR, "The SQL Server (MSSQLSERVER) service terminated unexpectedly.", 3},
	{"1000", "Application Error", "Application", logsv1.Severity_SEVERITY_ERROR, "Faulting application name: ShopSync.exe, version 4.2.1.0, exception code 0xc0000005", 5},
	{"6008", "EventLog", "System", logsv1.Severity_SEVERITY_ERROR, "The previous system shutdown was unexpected.", 1},
	{"41", "Microsoft-Windows-Kernel-Power", "System", logsv1.Severity_SEVERITY_FATAL, "The system has rebooted without cleanly shutting down first.", 1},
}

var winWeightTotal int

func init() {
	for _, e := range winEvents {
		winWeightTotal += e.weight
	}
}

// eventlogRecord mirrors /hack/windows-fixture's attribute contract
// (winevt.* keys) so a query written against one works against the
// other -- that fixture stays the small correctness check it was built
// as; this one supplies demo volume.
func eventlogRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	n := r.Intn(winWeightTotal)
	e := winEvents[0]
	for _, cand := range winEvents {
		if n -= cand.weight; n < 0 {
			e = cand
			break
		}
	}
	// A probe window reaches the Windows tier as failed logons too.
	if c.bruteForce && r.Float64() < 0.5 {
		e = winEvents[1]
	}

	attrs := map[string]string{
		"winevt.event_id":      e.id,
		"winevt.provider":      e.provider,
		"winevt.channel":       e.channel,
		"winevt.computer":      h.name,
		"winevt.record_number": strconv.Itoa(100000 + r.Intn(900000)),
	}
	msg := e.message
	switch e.id {
	case "4624", "4625", "4634":
		user := pick(r, []string{"SHOP\\svc_sync", "SHOP\\jcoffey", "SHOP\\administrator", "SHOP\\backup"})
		logonType := pick(r, []string{"3", "10", "2"})
		attrs["winevt.target_user"] = user
		attrs["winevt.logon_type"] = logonType
		src := pick(r, clientIPs)
		if e.id == "4625" {
			src = pick(r, attackerIPs)
			attrs["winevt.status"] = "0xC000006D"
		}
		attrs["remote_addr"] = src
		msg = fmt.Sprintf("%s Account: %s  Logon Type: %s  Source Network Address: %s", e.message, user, logonType, src)
	case "4688":
		proc := pick(r, []string{"C:\\Windows\\System32\\cmd.exe", "C:\\Program Files\\ShopSync\\ShopSync.exe", "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe"})
		attrs["winevt.process"] = proc
		msg = fmt.Sprintf("%s New Process Name: %s", e.message, proc)
	case "4740":
		user := pick(r, []string{"SHOP\\jcoffey", "SHOP\\svc_sync"})
		attrs["winevt.target_user"] = user
		msg = fmt.Sprintf("%s Account Name: %s  Caller Computer Name: %s", e.message, user, h.name)
	}
	return newRecord(h, "eventlog", t, e.sev, msg, attrs)
}

// primaryRecord dispatches to whichever generator matches this host's
// role. Kept as one switch rather than a func field on host so fleet.go
// stays pure data.
func primaryRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	switch h.service {
	case "nginx":
		return nginxRecord(h, t, r, c)
	case "api":
		return apiRecord(h, t, r, c)
	case "worker":
		return workerRecord(h, t, r, c)
	case "postgres":
		return postgresRecord(h, t, r, c)
	case "redis":
		return redisRecord(h, t, r, c)
	case "smtp":
		return smtpRecord(h, t, r, c)
	case "eventlog":
		return eventlogRecord(h, t, r, c)
	case "haproxy":
		return haproxyRecord(h, t, r, c)
	case "mysql":
		return mysqlRecord(h, t, r, c)
	case "rabbitmq":
		return rabbitRecord(h, t, r, c)
	case "elasticsearch":
		return elasticRecord(h, t, r, c)
	case "kubelet":
		return kubeletRecord(h, t, r, c)
	case "jenkins":
		return jenkinsRecord(h, t, r, c)
	case "vault":
		return vaultRecord(h, t, r, c)
	case "openldap":
		return ldapRecord(h, t, r, c)
	case "bind":
		return bindRecord(h, t, r, c)
	case "squid":
		return squidRecord(h, t, r, c)
	case "backup":
		return backupRecord(h, t, r, c)
	case "iis":
		return iisRecord(h, t, r, c)
	case "mssql":
		return mssqlRecord(h, t, r, c)
	case "exchange":
		return exchangeRecord(h, t, r, c)
	case "smb":
		return smbRecord(h, t, r, c)
	case "magento":
		return magentoRecord(h, t, r, c)
	case "woocommerce":
		return wooRecord(h, t, r, c)
	case "payments":
		return paymentsRecord(h, t, r, c)
	default:
		return newRecord(h, h.service, t, logsv1.Severity_SEVERITY_INFO, "heartbeat", nil)
	}
}

// ---------------------------------------------------------------------
// Enterprise service generators.
//
// Added when the demo fleet grew from twelve hosts to fifty. The first
// six services were the ones a shop runs; these are the ones an
// enterprise runs, and they exist so a visitor sees their own estate
// rather than somebody's side project -- a load balancer in front, a
// directory and DNS underneath, a queue and a search cluster beside the
// database, CI and secrets off to one side, and a Windows tier that is a
// real tier rather than one box.
//
// Same contract as the six above: the message reads like the line the
// real daemon writes, and the attributes carry the fields inside it.
// ---------------------------------------------------------------------

var (
	haproxyBackends = []string{"api_pool", "web_pool", "static_pool", "grpc_pool"}

	mysqlStatements = []struct {
		kind, table, sql string
		msMin, msMax     int
	}{
		{"select", "orders", "SELECT id, total, status FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 50", 2, 40},
		{"select", "inventory", "SELECT sku, on_hand FROM inventory WHERE warehouse_id = ?", 1, 25},
		{"update", "inventory", "UPDATE inventory SET on_hand = on_hand - ? WHERE sku = ?", 3, 60},
		{"insert", "audit_log", "INSERT INTO audit_log (actor, action, target) VALUES (?, ?, ?)", 1, 12},
		{"select", "reports", "SELECT DATE(created_at) d, SUM(total) FROM orders GROUP BY d ORDER BY d DESC", 400, 4200},
	}

	esIndices = []string{"logs-app-000042", "logs-app-000043", "catalogue-v7", "customers-v3"}

	k8sPods = []string{
		"checkout-7d9f8b6c4-x2k9p", "catalogue-5c8b7d9f6-m4n2q", "cart-6b9d8c7f5-t8w3r",
		"payments-8f7c6b5d4-j1h5g", "search-api-9d8c7b6a5-p9l2k",
	}

	jenkinsJobs = []string{"shop-api/main", "shop-web/main", "platform-terraform/apply", "agent-rust/release", "nightly-integration"}

	ldapBinds = []string{
		"uid=svc_sync,ou=services,dc=shop,dc=example", "uid=jcoffey,ou=people,dc=shop,dc=example",
		"uid=backup,ou=services,dc=shop,dc=example", "cn=admin,dc=shop,dc=example",
	}

	dnsQueries = []struct{ name, qtype string }{
		{"api.shop.example", "A"}, {"cdn.shop.example", "AAAA"}, {"shop.example", "MX"},
		{"_ldap._tcp.shop.example", "SRV"}, {"checkout.shop.example", "A"}, {"unknown.shop.example", "A"},
	}

	vaultPaths = []string{"secret/data/shop/db", "secret/data/shop/stripe", "pki/issue/internal", "auth/kubernetes/login"}

	iisSites = []string{"Default Web Site", "ShopIntranet", "ReportingPortal"}

	mssqlDatabases = []string{"ShopERP", "ShopWarehouse", "ReportingDW", "msdb"}

	shares = []string{"\\\\FS-01\\Finance", "\\\\FS-01\\Engineering", "\\\\FS-02\\Archive", "\\\\FS-02\\Profiles"}
)

// proxyStatus is the status code a proxy tier returns: the edge and IIS
// both mirror whatever the API behind them is doing, the same way
// nginxRecord does, so a 5xx panel agrees across all three tiers rather
// than showing an outage at one hop and health at the next.
func proxyStatus(r *rand.Rand, c conditions, slow bool) int {
	errRate := c.apiErrorRate
	if errRate == 0 {
		errRate = 0.008
	}
	if slow {
		errRate *= 1.6
	}
	switch {
	case r.Float64() < errRate:
		return pick(r, []int{502, 503, 504})
	case r.Float64() < 0.04:
		return pick(r, []int{404, 401, 403, 429})
	case r.Float64() < 0.05:
		return 301
	}
	return 200
}

func haproxyRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	method, path, route, slow := pickRoute(r)
	backend := pick(r, haproxyBackends)
	status := proxyStatus(r, c, slow)
	// HAProxy's timing quintuple is the thing operators actually read.
	tq, tw, tc := r.Intn(4), r.Intn(3), r.Intn(6)
	tr := int(float64(8+r.Intn(180)) * c.latencyMult)
	tt := tq + tw + tc + tr
	srvConn, beConn := r.Intn(12), r.Intn(40)
	sev := logsv1.Severity_SEVERITY_INFO
	if status >= 500 {
		sev = logsv1.Severity_SEVERITY_ERROR
	}
	src := pick(r, clientIPs)
	return newRecord(h, "haproxy", t, sev,
		fmt.Sprintf("%s:%d [%s] https-in~ %s/srv%d %d/%d/%d/%d/%d %d %d - - ---- %d/%d/%d/%d/0 0/0 \"%s %s HTTP/2.0\"",
			src, 40000+r.Intn(20000), t.Format("02/Jan/2006:15:04:05.000"),
			backend, 1+r.Intn(4), tq, tw, tc, tr, tt, status, 200+r.Intn(9000),
			beConn, srvConn, r.Intn(3), r.Intn(2), method, path),
		map[string]string{
			"backend": backend, "status": strconv.Itoa(status), "method": method,
			"route": route, "duration_ms": strconv.Itoa(tt), "remote_addr": src,
			"be_conn": strconv.Itoa(beConn), "srv_conn": strconv.Itoa(srvConn),
		})
}

func mysqlRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	if r.Float64() < 0.15 {
		return newRecord(h, "mysql", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("[Note] Aborted connection %d to db: 'shop' user: 'shop_app' host: '%s' (Got timeout reading communication packets)",
				100000+r.Intn(900000), pick(r, clientIPs)),
			map[string]string{"db": "shop", "query_kind": "connection_error"})
	}
	s := pick(r, mysqlStatements)
	dur := float64(s.msMin+r.Intn(s.msMax-s.msMin)) * c.latencyMult
	sev := logsv1.Severity_SEVERITY_DEBUG
	if dur > 1000 {
		sev = logsv1.Severity_SEVERITY_WARN
	}
	return newRecord(h, "mysql", t, sev,
		fmt.Sprintf("# Query_time: %.6f  Lock_time: %.6f Rows_sent: %d  Rows_examined: %d\n%s",
			dur/1000, r.Float64()/500, r.Intn(200), r.Intn(20000), s.sql),
		map[string]string{
			"db": "shop", "db_user": "shop_app", "query_kind": s.kind, "table": s.table,
			"duration_ms": fmt.Sprintf("%.1f", dur), "rows": strconv.Itoa(r.Intn(200)),
		})
}

func rabbitRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	queue := pick(r, []string{"orders.process", "mail.outbound", "search.index", "invoice.render"})
	depth := r.Intn(400)
	// A queue backs up when the workers draining it are failing, which is
	// what the outage window already models -- rather than inventing a
	// second condition that says the same thing.
	if c.jobFailureRate > 0.2 {
		depth += 2000 + r.Intn(6000)
	}
	switch n := r.Intn(10); {
	case n < 6:
		return newRecord(h, "rabbitmq", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("accepting AMQP connection <0.%d.0> (%s:%d -> 10.0.4.21:5672)", 1000+r.Intn(9000), pick(r, clientIPs), 40000+r.Intn(20000)),
			map[string]string{"queue": queue, "queue_depth": strconv.Itoa(depth), "event_kind": "connect"})
	case n < 9:
		return newRecord(h, "rabbitmq", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Queue '%s' in vhost '/': %d messages ready, %d unacknowledged", queue, depth, r.Intn(40)),
			map[string]string{"queue": queue, "queue_depth": strconv.Itoa(depth), "event_kind": "depth"})
	default:
		return newRecord(h, "rabbitmq", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("memory resource limit alarm set on node rabbit@%s -- publishers will be blocked", h.name),
			map[string]string{"queue": queue, "queue_depth": strconv.Itoa(depth), "event_kind": "alarm"})
	}
}

func elasticRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	idx := pick(r, esIndices)
	took := int(float64(4+r.Intn(300)) * c.latencyMult)
	switch n := r.Intn(10); {
	case n < 7:
		return newRecord(h, "elasticsearch", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("[%s] search completed in %dms, hits=%d, shards=[3 total, 3 successful]", idx, took, r.Intn(4000)),
			map[string]string{"index": idx, "duration_ms": strconv.Itoa(took), "event_kind": "search"})
	case n < 9:
		return newRecord(h, "elasticsearch", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("[%s] now throttling indexing: numMergesInFlight=%d, maxNumMerges=%d", idx, 4+r.Intn(4), 5),
			map[string]string{"index": idx, "event_kind": "throttle"})
	default:
		return newRecord(h, "elasticsearch", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("[gc][old][%d][%d] duration [%dms], collections [1]/[%ds]", r.Intn(90000), r.Intn(400), 400+r.Intn(2200), 1+r.Intn(3)),
			map[string]string{"index": idx, "event_kind": "gc"})
	}
}

func kubeletRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	pod := pick(r, k8sPods)
	ns := "shop"
	switch n := r.Intn(12); {
	case n < 6:
		return newRecord(h, "kubelet", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("SyncLoop (PLEG): %q, event: &pod.LifecycleEvent{ID:%q, Type:\"ContainerStarted\"}", ns+"/"+pod, pod),
			map[string]string{"pod": pod, "namespace": ns, "event_kind": "sync"})
	case n < 9:
		return newRecord(h, "kubelet", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Probe succeeded for pod %q container %q: HTTP 200 in %dms", ns+"/"+pod, strings.Split(pod, "-")[0], 2+r.Intn(40)),
			map[string]string{"pod": pod, "namespace": ns, "event_kind": "probe"})
	case n < 11:
		return newRecord(h, "kubelet", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("Readiness probe failed for pod %q: Get \"http://10.42.0.%d:8080/healthz\": context deadline exceeded", ns+"/"+pod, r.Intn(250)),
			map[string]string{"pod": pod, "namespace": ns, "event_kind": "probe_failed"})
	default:
		return newRecord(h, "kubelet", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("Failed to pull image \"registry.shop.example/%s:v1.4.%d\": rpc error: code = DeadlineExceeded", strings.Split(pod, "-")[0], r.Intn(40)),
			map[string]string{"pod": pod, "namespace": ns, "event_kind": "image_pull_failed"})
	}
}

func jenkinsRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	job := pick(r, jenkinsJobs)
	build := 400 + r.Intn(900)
	dur := 40 + r.Intn(900)
	if r.Float64() < 0.12 {
		return newRecord(h, "jenkins", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("%s #%d completed: FAILURE after %ds", job, build, dur),
			map[string]string{"job": job, "build": strconv.Itoa(build), "result": "FAILURE", "duration_s": strconv.Itoa(dur)})
	}
	return newRecord(h, "jenkins", t, logsv1.Severity_SEVERITY_INFO,
		fmt.Sprintf("%s #%d completed: SUCCESS after %ds", job, build, dur),
		map[string]string{"job": job, "build": strconv.Itoa(build), "result": "SUCCESS", "duration_s": strconv.Itoa(dur)})
}

func vaultRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	path := pick(r, vaultPaths)
	if c.bruteForce && r.Float64() < 0.25 {
		return newRecord(h, "vault", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("authentication failed: path=%s remote_address=%s error=\"permission denied\"", path, pick(r, attackerIPs)),
			map[string]string{"vault_path": path, "event_kind": "auth_failed", "remote_addr": pick(r, attackerIPs)})
	}
	return newRecord(h, "vault", t, logsv1.Severity_SEVERITY_INFO,
		fmt.Sprintf("request: operation=read path=%s remote_address=10.0.2.%d ttl=%dm", path, r.Intn(250), 30+r.Intn(700)),
		map[string]string{"vault_path": path, "event_kind": "read"})
}

func ldapRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	dn := pick(r, ldapBinds)
	if c.bruteForce && r.Float64() < 0.3 {
		return newRecord(h, "openldap", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("conn=%d op=1 RESULT tag=97 err=49 text=Invalid credentials dn=%q", 10000+r.Intn(90000), dn),
			map[string]string{"bind_dn": dn, "ldap_err": "49", "event_kind": "bind_failed", "remote_addr": pick(r, attackerIPs)})
	}
	switch r.Intn(3) {
	case 0:
		return newRecord(h, "openldap", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("conn=%d op=0 BIND dn=%q method=128 mech=SIMPLE ssf=256", 10000+r.Intn(90000), dn),
			map[string]string{"bind_dn": dn, "event_kind": "bind"})
	default:
		return newRecord(h, "openldap", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("conn=%d op=2 SEARCH RESULT tag=101 err=0 nentries=%d text=", 10000+r.Intn(90000), r.Intn(60)),
			map[string]string{"bind_dn": dn, "event_kind": "search"})
	}
}

func bindRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	q := dnsQueries[r.Intn(len(dnsQueries))]
	src := pick(r, clientIPs)
	if q.name == "unknown.shop.example" {
		return newRecord(h, "bind", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("client @0x%x %s#%d (%s): query: %s IN %s + (10.0.5.2) NXDOMAIN", r.Int63(), src, 30000+r.Intn(30000), q.name, q.name, q.qtype),
			map[string]string{"dns_name": q.name, "dns_type": q.qtype, "dns_rcode": "NXDOMAIN", "remote_addr": src})
	}
	return newRecord(h, "bind", t, logsv1.Severity_SEVERITY_INFO,
		fmt.Sprintf("client @0x%x %s#%d (%s): query: %s IN %s + (10.0.5.2)", r.Int63(), src, 30000+r.Intn(30000), q.name, q.name, q.qtype),
		map[string]string{"dns_name": q.name, "dns_type": q.qtype, "dns_rcode": "NOERROR", "remote_addr": src})
}

func squidRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	sites := []string{"github.com", "registry.npmjs.org", "download.docker.com", "ubuntu.com", "ads.example.net"}
	site := pick(r, sites)
	action, status := "TCP_MISS/200", 200
	sev := logsv1.Severity_SEVERITY_INFO
	if site == "ads.example.net" {
		action, status, sev = "TCP_DENIED/403", 403, logsv1.Severity_SEVERITY_WARN
	}
	src := "10.0.3." + strconv.Itoa(r.Intn(250))
	return newRecord(h, "squid", t, sev,
		fmt.Sprintf("%d.%03d %6d %s %s %d CONNECT %s:443 - HIER_DIRECT/%s -",
			t.Unix(), r.Intn(1000), r.Intn(4000), src, action, 500+r.Intn(90000), site, site),
		map[string]string{"proxy_action": action, "status": strconv.Itoa(status), "dest_host": site, "remote_addr": src})
}

func backupRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	job := pick(r, []string{"nightly-fileserver", "nightly-postgres", "weekly-archive", "hourly-mssql"})
	gb := 4 + r.Float64()*90
	if r.Float64() < 0.1 {
		return newRecord(h, "backup", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("Job %s terminated with errors: cannot open source \"%s\": permission denied", job, pick(r, shares)),
			map[string]string{"backup_job": job, "result": "error", "event_kind": "backup"})
	}
	return newRecord(h, "backup", t, logsv1.Severity_SEVERITY_INFO,
		fmt.Sprintf("Job %s OK: %.1f GB written in %d min, dedup ratio %.2fx", job, gb, 8+r.Intn(90), 1.8+r.Float64()*3),
		map[string]string{"backup_job": job, "result": "ok", "bytes_gb": fmt.Sprintf("%.1f", gb), "event_kind": "backup"})
}

func iisRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	method, path, route, slow := pickRoute(r)
	site := pick(r, iisSites)
	status := proxyStatus(r, c, slow)
	took := int(float64(6+r.Intn(400)) * c.latencyMult)
	src := pick(r, clientIPs)
	sev := logsv1.Severity_SEVERITY_INFO
	if status >= 500 {
		sev = logsv1.Severity_SEVERITY_ERROR
	}
	// W3C extended log format, which is what IIS actually writes.
	return newRecord(h, "iis", t, sev,
		fmt.Sprintf("%s %s %s %s - 443 - %s HTTP/2.0 %s - - %s %d 0 0 %d",
			t.Format("2006-01-02"), t.Format("15:04:05"), h.ipv4, method, src,
			pick(r, userAgents), path, status, took),
		map[string]string{
			"site": site, "status": strconv.Itoa(status), "method": method,
			"route": route, "duration_ms": strconv.Itoa(took), "remote_addr": src,
		})
}

func mssqlRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	db := pick(r, mssqlDatabases)
	switch n := r.Intn(12); {
	case n < 5:
		return newRecord(h, "mssql", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Login succeeded for user 'SHOP\\svc_erp'. Connection made using Windows authentication. [CLIENT: %s]", pick(r, clientIPs)),
			map[string]string{"db": db, "event_kind": "login"})
	case n < 8:
		ms := int(float64(200+r.Intn(9000)) * c.latencyMult)
		sev := logsv1.Severity_SEVERITY_INFO
		if ms > 5000 {
			sev = logsv1.Severity_SEVERITY_WARN
		}
		return newRecord(h, "mssql", t, sev,
			fmt.Sprintf("SQL Server has encountered %d occurrence(s) of I/O requests taking longer than %d seconds to complete on file [F:\\Data\\%s.mdf]", 1+r.Intn(4), 15, db),
			map[string]string{"db": db, "duration_ms": strconv.Itoa(ms), "event_kind": "io_stall"})
	case n < 10:
		return newRecord(h, "mssql", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Log was backed up. Database: %s, creation date(time): 2026/01/14(03:11:02), first LSN: %d:%d:1", db, 40+r.Intn(60), r.Intn(90000)),
			map[string]string{"db": db, "event_kind": "log_backup"})
	case n < 11:
		return newRecord(h, "mssql", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("Transaction (Process ID %d) was deadlocked on lock resources with another process and has been chosen as the deadlock victim in database %s", 50+r.Intn(200), db),
			map[string]string{"db": db, "event_kind": "deadlock"})
	default:
		return newRecord(h, "mssql", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("Login failed for user 'SHOP\\svc_report'. Reason: Password did not match that for the login provided. [CLIENT: %s]", pick(r, attackerIPs)),
			map[string]string{"db": db, "event_kind": "login_failed", "remote_addr": pick(r, attackerIPs)})
	}
}

func exchangeRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	box := pick(r, []string{"jcoffey@shop.example", "orders@shop.example", "support@shop.example", "hr@shop.example"})
	switch n := r.Intn(10); {
	case n < 5:
		return newRecord(h, "exchange", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("RECEIVE SMTP 250 2.6.0 message queued for delivery to %s, size %d bytes", box, 2000+r.Intn(200000)),
			map[string]string{"mailbox": box, "event_kind": "receive"})
	case n < 8:
		return newRecord(h, "exchange", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("SEND SMTP 250 2.0.0 delivered to remote host for %s in %dms", box, 40+r.Intn(2000)),
			map[string]string{"mailbox": box, "event_kind": "send"})
	case n < 9:
		return newRecord(h, "exchange", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("AGENT SpamFilter rejected message for %s: SCL 7 above threshold", box),
			map[string]string{"mailbox": box, "event_kind": "spam_reject"})
	default:
		return newRecord(h, "exchange", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("FAIL 550 5.1.1 User unknown: no mailbox by that name at shop.example (recipient %s)", box),
			map[string]string{"mailbox": box, "event_kind": "bounce"})
	}
}

func smbRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	share := pick(r, shares)
	user := pick(r, []string{"SHOP\\jcoffey", "SHOP\\svc_backup", "SHOP\\finance_ro", "SHOP\\eng_rw"})
	switch n := r.Intn(10); {
	case n < 7:
		return newRecord(h, "smb", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("A network share object was accessed. Share Name: %s  Account Name: %s  Access: ReadData", share, user),
			map[string]string{"share": share, "winevt.target_user": user, "event_kind": "share_access"})
	case n < 9:
		return newRecord(h, "smb", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("File written on share %s by %s (%d bytes)", share, user, 1000+r.Intn(9000000)),
			map[string]string{"share": share, "winevt.target_user": user, "event_kind": "share_write"})
	default:
		return newRecord(h, "smb", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("A network share object access was denied. Share Name: %s  Account Name: %s", share, user),
			map[string]string{"share": share, "winevt.target_user": user, "event_kind": "share_denied"})
	}
}

// ---------------------------------------------------------------------
// Commerce.
//
// Two storefronts and the gateway behind both, because the questions a
// business asks of its logs are not the questions an operator asks, and
// a demo that only answers the second one is only half a demo. Orders,
// revenue, average order value, where checkout is losing people and why
// a card was declined are all in the log line rather than in a separate
// metrics system -- which is the argument for keeping the two together.
//
// Two platforms rather than one on purpose: Magento and WooCommerce
// write differently about the same events, so a panel that groups by
// service rather than assuming one shape is the honest way to build one.
// ---------------------------------------------------------------------

var (
	storeViews = []string{"uk", "us", "de", "fr"}

	skus = []struct {
		sku, name string
		price     float64
	}{
		{"CH-1042", "Aeron-style task chair", 489.00},
		{"DK-2201", "Standing desk 160x80", 629.00},
		{"MN-3310", "27\" 4K monitor", 379.99},
		{"KB-4407", "Mechanical keyboard, tactile", 129.50},
		{"MS-5120", "Vertical ergonomic mouse", 74.95},
		{"LT-6003", "Desk lamp, warm CCT", 59.00},
		{"CB-7788", "Cable management tray", 24.99},
		{"HS-8890", "Noise-cancelling headset", 219.00},
	}

	checkoutSteps = []string{"cart", "shipping", "payment", "review", "placed"}

	gateways = []string{"stripe", "adyen", "paypal"}

	declineReasons = []struct {
		code, text string
		weight     int
	}{
		{"insufficient_funds", "Insufficient funds", 30},
		{"do_not_honor", "Do not honour", 22},
		{"expired_card", "Expired card", 14},
		{"incorrect_cvc", "Incorrect CVC", 12},
		{"lost_or_stolen", "Lost or stolen card", 6},
		{"3ds_failed", "3-D Secure authentication failed", 16},
	}
	declineWeightTotal int

	paymentMethods = []string{"card", "paypal", "apple_pay", "klarna"}
)

func init() {
	for _, d := range declineReasons {
		declineWeightTotal += d.weight
	}
}

func pickDecline(r *rand.Rand) (string, string) {
	n := r.Intn(declineWeightTotal)
	for _, d := range declineReasons {
		if n -= d.weight; n < 0 {
			return d.code, d.text
		}
	}
	return declineReasons[0].code, declineReasons[0].text
}

// orderTotal builds a basket rather than drawing a number, so average
// order value moves the way a real one does -- driven by what is in the
// cart, not by a distribution somebody chose.
func orderTotal(r *rand.Rand) (float64, int, string) {
	items := 1 + r.Intn(4)
	total := 0.0
	first := ""
	for i := 0; i < items; i++ {
		s := skus[r.Intn(len(skus))]
		qty := 1
		if r.Float64() < 0.18 {
			qty = 2
		}
		total += s.price * float64(qty)
		if i == 0 {
			first = s.sku
		}
	}
	return total, items, first
}

func magentoRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	store := pick(r, storeViews)
	switch n := r.Intn(100); {
	case n < 34:
		// Checkout progress. The funnel narrows towards `placed`, which is
		// what makes a "where are we losing people" panel say anything.
		step := checkoutSteps[0]
		switch f := r.Float64(); {
		case f < 0.34:
			step = "cart"
		case f < 0.58:
			step = "shipping"
		case f < 0.76:
			step = "payment"
		case f < 0.88:
			step = "review"
		default:
			step = "placed"
		}
		return newRecord(h, "magento", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("checkout step reached: %s quote_id=%d store=%s", step, 400000+r.Intn(99999), store),
			map[string]string{"checkout_step": step, "store_view": store, "event_kind": "checkout"})
	case n < 58:
		total, items, sku := orderTotal(r)
		return newRecord(h, "magento", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Order placed: increment_id=%d grand_total=%.2f items=%d store=%s method=%s",
				2000000000+r.Intn(99999999), total, items, store, pick(r, paymentMethods)),
			map[string]string{
				"event_kind": "order", "order_total": fmt.Sprintf("%.2f", total),
				"order_items": strconv.Itoa(items), "sku": sku, "store_view": store,
				"currency": "GBP", "payment_method": pick(r, paymentMethods),
			})
	case n < 72:
		s := skus[r.Intn(len(skus))]
		return newRecord(h, "magento", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Product viewed: sku=%s name=%q store=%s", s.sku, s.name, store),
			map[string]string{"event_kind": "product_view", "sku": s.sku, "store_view": store})
	case n < 82:
		idx := pick(r, []string{"catalog_product_price", "cataloginventory_stock", "catalogsearch_fulltext", "customer_grid"})
		dur := 4 + r.Intn(180)
		return newRecord(h, "magento", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Index %s has been rebuilt successfully in %02d:%02d:%02d", idx, 0, dur/60, dur%60),
			map[string]string{"event_kind": "reindex", "indexer": idx, "duration_ms": strconv.Itoa(dur * 1000)})
	case n < 90:
		return newRecord(h, "magento", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Cron group %s finished, %d jobs run", pick(r, []string{"default", "index", "consumers"}), 1+r.Intn(20)),
			map[string]string{"event_kind": "cron", "store_view": store})
	case n < 96:
		s := skus[r.Intn(len(skus))]
		return newRecord(h, "magento", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("Not enough items for sale: sku=%s requested=%d on_hand=%d", s.sku, 1+r.Intn(3), r.Intn(2)),
			map[string]string{"event_kind": "out_of_stock", "sku": s.sku, "store_view": store})
	default:
		return newRecord(h, "magento", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("main.CRITICAL: Uncaught TypeError in %s: Argument #1 must be of type Quote, null given",
				pick(r, []string{"Magento/Quote/Model/QuoteManagement.php", "Magento/Checkout/Model/Session.php", "Magento/Sales/Model/Order.php"})),
			map[string]string{"event_kind": "exception", "store_view": store})
	}
}

func wooRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	switch n := r.Intn(100); {
	case n < 38:
		total, items, sku := orderTotal(r)
		status := pick(r, []string{"processing", "completed", "on-hold"})
		return newRecord(h, "woocommerce", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Order #%d status changed to %s (total %.2f, %d items)", 30000+r.Intn(9999), status, total, items),
			map[string]string{
				"event_kind": "order", "order_status": status, "order_total": fmt.Sprintf("%.2f", total),
				"order_items": strconv.Itoa(items), "sku": sku, "currency": "GBP",
				"payment_method": pick(r, paymentMethods),
			})
	case n < 58:
		return newRecord(h, "woocommerce", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("REST API request: GET /wp-json/wc/v3/products?per_page=%d served in %dms", 10+r.Intn(90), 20+r.Intn(600)),
			map[string]string{"event_kind": "api", "duration_ms": strconv.Itoa(20 + r.Intn(600))})
	case n < 74:
		return newRecord(h, "woocommerce", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Scheduled action completed: %s", pick(r, []string{"woocommerce_cleanup_sessions", "wc_admin_unsnooze_admin_notes", "woocommerce_scheduled_sales"})),
			map[string]string{"event_kind": "cron"})
	case n < 84:
		s := skus[r.Intn(len(skus))]
		return newRecord(h, "woocommerce", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("Stock reduced for %s: %d -> %d", s.sku, 5+r.Intn(40), r.Intn(5)),
			map[string]string{"event_kind": "stock", "sku": s.sku})
	case n < 93:
		return newRecord(h, "woocommerce", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("Checkout error: %s", pick(r, []string{
				"Invalid billing postcode", "Coupon \"WELCOME10\" has expired",
				"Shipping method not available for this address", "Session expired before payment",
			})),
			map[string]string{"event_kind": "checkout_error"})
	default:
		return newRecord(h, "woocommerce", t, logsv1.Severity_SEVERITY_ERROR,
			"PHP Fatal error: Allowed memory size of 268435456 bytes exhausted in class-wc-order.php",
			map[string]string{"event_kind": "exception"})
	}
}

func paymentsRecord(h *host, t time.Time, r *rand.Rand, c conditions) *logsv1.LogRecord {
	gw := pick(r, gateways)
	total, _, _ := orderTotal(r)
	took := int(float64(90+r.Intn(900)) * c.latencyMult)

	// Declines rise with the outage window: the same dependency trouble
	// that fails API requests fails authorisations, which is what makes
	// the decline-rate rule true at the same time as the 5xx one.
	declineRate := 0.075
	if c.apiErrorRate > 0 {
		declineRate = 0.28
	}
	switch {
	case r.Float64() < declineRate:
		code, text := pickDecline(r)
		return newRecord(h, "payments", t, logsv1.Severity_SEVERITY_WARN,
			fmt.Sprintf("authorization declined gateway=%s amount=%.2f currency=GBP reason=%s (%s) latency=%dms", gw, total, code, text, took),
			map[string]string{
				"event_kind": "authorization", "auth_result": "declined", "gateway": gw,
				"decline_reason": code, "amount": fmt.Sprintf("%.2f", total),
				"currency": "GBP", "duration_ms": strconv.Itoa(took),
			})
	case r.Float64() < 0.05:
		return newRecord(h, "payments", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("refund issued gateway=%s amount=%.2f currency=GBP reason=%s", gw, total/2, pick(r, []string{"customer_request", "item_returned", "duplicate_charge"})),
			map[string]string{"event_kind": "refund", "gateway": gw, "amount": fmt.Sprintf("%.2f", total/2), "currency": "GBP"})
	case r.Float64() < 0.02:
		return newRecord(h, "payments", t, logsv1.Severity_SEVERITY_ERROR,
			fmt.Sprintf("chargeback received gateway=%s amount=%.2f currency=GBP network_reason=fraud", gw, total),
			map[string]string{"event_kind": "chargeback", "gateway": gw, "amount": fmt.Sprintf("%.2f", total), "currency": "GBP"})
	case r.Float64() < 0.10:
		return newRecord(h, "payments", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("3-D Secure challenge issued gateway=%s amount=%.2f currency=GBP", gw, total),
			map[string]string{"event_kind": "3ds_challenge", "gateway": gw, "amount": fmt.Sprintf("%.2f", total), "currency": "GBP"})
	default:
		return newRecord(h, "payments", t, logsv1.Severity_SEVERITY_INFO,
			fmt.Sprintf("authorization approved gateway=%s amount=%.2f currency=GBP latency=%dms", gw, total, took),
			map[string]string{
				"event_kind": "authorization", "auth_result": "approved", "gateway": gw,
				"amount": fmt.Sprintf("%.2f", total), "currency": "GBP",
				"duration_ms": strconv.Itoa(took),
			})
	}
}
