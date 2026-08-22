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
	default:
		return newRecord(h, h.service, t, logsv1.Severity_SEVERITY_INFO, "heartbeat", nil)
	}
}
