// Package server is obs-beacond: it takes reports from the agents, judges them,
// and serves the API and the web interface.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/cairnobs/cairnobs/internal/akamai"
	"github.com/cairnobs/cairnobs/internal/cloudflare"
	"github.com/cairnobs/cairnobs/internal/mailmon"
	"github.com/cairnobs/cairnobs/internal/state"
	"github.com/cairnobs/cairnobs/internal/version"
	"github.com/cairnobs/cairnobs/internal/webui"
)

// Server is a running obs-beacond.
type Server struct {
	cfg      *Config
	store    *Store
	expected *Expected // current expected.yaml; see currentExpected

	expMu     sync.Mutex
	expStat   os.FileInfo // what the loaded expected.yaml looked like on disk
	sessions  sessions
	started   time.Time
	alerts    *alerter
	db        *state.DB         // nil when no database is configured
	atls      *agentTLS         // nil when the agent listener is plain HTTP
	acme      *autocert.Manager // nil without tls.acme
	joinedMu  sync.RWMutex
	joined    map[string]state.JoinedAgent // agents that joined with a token, by name
	cf        *cloudflare.Watcher
	ak        *akamai.Watcher
	mail      *mailmon.Watcher
	local     *localDir   // the local directory, when signin.providers has one
	dns       dnsBaseline // the DNS records last accepted, per zone
	akdns     dnsBaseline // the same for Akamai's Edge DNS zones
	checks    checksFile
	maint     maintWindows // maintenance windows in force (settings.go)
	uiChans   uiChannels   // alert channels set in the UI (channels.go)
	backups   backupsFile
	topology  topologyFile
	patroni   patroniWatch
	wl        workloadStore          // from cluster agents (workloads.go)
	seen      readingsSeen           // when each of Cloudflare's, Patroni's and the mail round trip's readings was taken
	natsSlow  map[string]int64       // last slow-consumer count per host/probe (under patroni.mu)
	etcdFsync map[string]*fsyncState // WAL fsync histogram and p99 per host (under patroni.mu)
	drift     driftWatch

	providers map[string]Provider // by id
	pendingMu sync.Mutex
	pending   map[string]pending // sign-ins out at a provider, by OAuth state

	// Sharing a PostgreSQL database with other copies (shared.go).
	copyID      string // this copy's name on the notice bus
	sectionsMu  sync.Mutex
	sectionSeen map[string]time.Time // when each UI section this copy has was saved
	caughtUp    chan struct{}        // closed once follow has caught up
	draining    atomic.Bool          // told to stop: /readyz says so
	lead        leadership           // the evaluator lease (lead.go)

	maps mapState // the maps' graph and learned edges (maps.go)

	ui    fs.FS
	index []byte
	csp   string
}

// New sets up a server from its config.
func New(cfg *Config) (*Server, error) {
	exp, err := LoadExpected(cfg.Expected)
	if err != nil {
		return nil, err
	}
	ui, err := fs.Sub(webui.Files, "dist")
	if err != nil {
		return nil, err
	}
	srv := &Server{cfg: cfg, store: NewStore(hostAgents(cfg.Agents)), expected: exp, ui: ui, started: time.Now(),
		providers: map[string]Provider{}, pending: map[string]pending{},
		copyID: newToken()[:12], sectionSeen: map[string]time.Time{}, caughtUp: make(chan struct{}),
		lead: leadership{poke: make(chan struct{}, 1)}}
	if cfg.Expected != "" {
		srv.expStat, _ = os.Stat(cfg.Expected)
	}
	if a := cfg.TLS.ACME; a != nil {
		if err := a.check(cfg); err != nil { // LoadConfig checked it; a Config built in code may not have been
			return nil, err
		}
		if cfg.Alerts.PublicURL == "" {
			cfg.Alerts.PublicURL = publicFromACME(cfg)
		}
		if srv.acme, err = newACME(a); err != nil {
			return nil, err
		}
	}
	if cfg.Database != "" {
		if srv.db, err = state.OpenWith(cfg.Database, cfg.StateKeyFile, state.Options{
			PasswordFile: cfg.DatabasePasswordFile,
			// In a cluster the database may start alongside: wait a while
			// rather than restart, which backs off longer each time.
			Wait: 2 * time.Minute,
			Log:  log.Printf,
		}); err != nil {
			return nil, err
		}
		srv.sessions.db = srv.db
		srv.sessions.ended = func(id string) { srv.publish(noticeSession, id) }
	}
	if srv.atls, err = setupAgentTLS(cfg, srv.db, time.Now()); err != nil {
		return nil, err
	}
	for i := range cfg.Maintenance { // LoadConfig checked them; a Config built in code may not have been
		if err := cfg.Maintenance[i].Check(); err != nil {
			return nil, err
		}
	}
	if srv.alerts, err = newAlerter(cfg.Alerts); err != nil {
		return nil, err
	}
	if cfg.Cloudflare != nil {
		if srv.cf, err = cloudflare.New(*cfg.Cloudflare); err != nil {
			return nil, err
		}
	}
	if cfg.Akamai != nil {
		if srv.ak, err = akamai.New(*cfg.Akamai); err != nil {
			return nil, err
		}
	}
	if cfg.Mail != nil {
		if srv.mail, err = mailmon.New(*cfg.Mail); err != nil {
			return nil, err
		}
	}
	srv.dns.db = srv.db
	srv.akdns.db, srv.akdns.prefix = srv.db, "ak_dns:"
	srv.patroni.reads = map[string]patroniRead{}
	srv.natsSlow = map[string]int64{}
	srv.etcdFsync = map[string]*fsyncState{}
	srv.wl.src = map[string]*wsource{}
	srv.syncWorkloads(true) // as stored: every copy's, or this one's before a restart
	if err := srv.loadJoined(); err != nil {
		return nil, err
	}
	if srv.shared() {
		srv.resyncHosts() // every host as the other copies last heard it
	}
	if err := srv.checkAgentNames(); err != nil {
		return nil, err
	}
	if cfg.Topology != "" {
		srv.topology.path = cfg.Topology
		if srv.topology.cur, err = LoadTopology(cfg.Topology, srv.agentList()); err != nil {
			return nil, err
		}
		srv.topology.stat, _ = os.Stat(cfg.Topology)
	}
	if cfg.Backups != "" {
		srv.backups.path = cfg.Backups
		if srv.backups.cur, err = loadBackups(cfg.Backups, srv.agentList()); err != nil {
			return nil, err
		}
		srv.backups.stat, _ = os.Stat(cfg.Backups)
	}
	if cfg.Checks != "" {
		// Refuse to start on a broken file; later edits that don't parse are only logged.
		srv.checks.path = cfg.Checks
		if srv.checks.cur, err = LoadChecks(cfg.Checks, srv.agentList()); err != nil {
			return nil, err
		}
		srv.checks.stat, _ = os.Stat(cfg.Checks)
	}
	if err := srv.loadSettings(); err != nil {
		return nil, err
	}
	srv.alerts.db = srv.db
	srv.alerts.load()
	srv.sessions.load(time.Now())
	for _, pc := range cfg.Signin.Providers {
		if pc.Type == "local" {
			if srv.local != nil {
				return nil, fmt.Errorf("sign-in provider %s: only one local directory", pc.ID)
			}
			if srv.local, err = newLocalDir(pc, srv.db, cfg.EmergencyListen); err != nil {
				return nil, err
			}
			continue
		}
		p, err := newProvider(pc)
		if err != nil {
			return nil, err
		}
		srv.providers[pc.ID] = p
	}
	if len(cfg.Signin.Providers) == 0 && srv.db != nil {
		// Nothing configured: the local directory, with passwords, so a new
		// install can make its first admin before it has HTTPS.
		pc := ProviderConfig{ID: "local", Type: "local", Label: "Sign in with a passkey", Origin: cfg.Alerts.PublicURL, Passwords: true}
		if srv.local, err = newLocalDir(pc, srv.db, cfg.EmergencyListen); err != nil {
			return nil, err
		}
		srv.local.builtIn = true
	}
	if cfg.EmergencyListen != "" && srv.local == nil {
		return nil, errors.New("emergency_listen needs the local directory (a `type: local` sign-in provider)")
	}
	if err := srv.firstRun(time.Now()); err != nil {
		return nil, err
	}
	srv.index, _ = fs.ReadFile(ui, "index.html")
	srv.csp = contentSecurityPolicy(srv.index)
	return srv, nil
}

// currentExpected re-reads expected.yaml when the file on disk changes, so
// editing the list never needs a restart. A file that doesn't parse is
// logged and the last good list stays in use.
func (srv *Server) currentExpected() *Expected {
	srv.expMu.Lock()
	defer srv.expMu.Unlock()
	if srv.cfg.Expected == "" {
		return srv.expected
	}
	st, err := os.Stat(srv.cfg.Expected)
	if err != nil || (srv.expStat != nil && st.ModTime().Equal(srv.expStat.ModTime()) && st.Size() == srv.expStat.Size()) {
		return srv.expected
	}
	srv.expStat = st
	exp, err := LoadExpected(srv.cfg.Expected)
	if err != nil {
		log.Printf("expected containers: %v; keeping the last good list", err)
		return srv.expected
	}
	srv.expected = exp
	srv.audit("obs-beacond", "reloaded the expected containers", srv.cfg.Expected)
	return exp
}

// audit records who did what, in the database's audit log and in the log.
func (srv *Server) audit(who, action, detail string) {
	log.Printf("audit: %s %s %s", who, action, detail)
	if srv.db != nil {
		if err := srv.db.Audit(state.Entry{At: time.Now(), Who: who, Action: action, Detail: detail}); err != nil {
			log.Printf("audit: can't store: %v", err)
		}
	}
}

func (srv *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries := []state.Entry{}
	if srv.db != nil {
		var err error
		if entries, err = srv.db.AuditLog(200); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "stored": srv.db != nil})
}

// snapshots writes a consistent copy of the database for the backup mesh,
// at start and then every snapshot_every.
func (srv *Server) snapshots(ctx context.Context) {
	write := func() {
		tmp := srv.cfg.Snapshot + ".new"
		// Its folder is made if need be, as data_dir is: a fresh volume
		// has none.
		if err := os.MkdirAll(filepath.Dir(srv.cfg.Snapshot), 0o700); err != nil {
			log.Printf("snapshot: %v", err)
			return
		}
		if err := srv.db.Backup(tmp); err != nil {
			log.Printf("snapshot: %v", err)
			return
		}
		if err := os.Rename(tmp, srv.cfg.Snapshot); err != nil {
			log.Printf("snapshot: %v", err)
		}
	}
	write()
	t := time.NewTicker(srv.cfg.SnapshotEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}

var inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// contentSecurityPolicy allows only our own files, plus the one inline
// bootstrap script SvelteKit puts in index.html, by its hash.
func contentSecurityPolicy(index []byte) string {
	scripts := []string{"'self'"}
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		sum := sha256.Sum256(m[1])
		scripts = append(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"manifest-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// Handler is every route, wrapped in the security headers.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok " + version.Version + "\n"))
	})
	mux.HandleFunc("GET /readyz", srv.handleReady)
	if srv.cfg.AgentListen == "" {
		mux.HandleFunc("POST /api/v1/agent/report", srv.handleReport)
		mux.HandleFunc("GET /api/v1/agent/checks", srv.handleAgentChecks)
		mux.HandleFunc("GET /api/v1/agent/actions", srv.handleAgentActions)
		mux.HandleFunc("POST /api/v1/agent/workloads", srv.handleWorkloads)
	}

	mux.HandleFunc("GET /api/v1/session", srv.handleSession)
	mux.HandleFunc("POST /auth/dev", srv.handleDevSignin)
	mux.HandleFunc("POST /auth/passkey/begin", srv.handlePasskeyBegin)
	mux.HandleFunc("POST /auth/passkey/finish", srv.handlePasskeyFinish)
	mux.HandleFunc("POST /auth/setup/info", srv.handleSetupInfo)
	mux.HandleFunc("POST /auth/setup/begin", srv.handleSetupBegin)
	mux.HandleFunc("POST /auth/setup/finish", srv.handleSetupFinish)
	mux.HandleFunc("POST /auth/setup/totp", srv.handleSetupTOTP)
	mux.HandleFunc("POST /auth/setup/password", srv.handleSetupPassword)
	mux.HandleFunc("POST /auth/first", srv.handleFirstAdmin)
	mux.HandleFunc("POST /auth/password", srv.handlePasswordSignin)
	mux.HandleFunc("GET /auth/{provider}/login", srv.handleLogin)
	mux.HandleFunc("GET /auth/{provider}/callback", srv.handleCallback)
	mux.HandleFunc("POST /auth/signout", srv.needSession(func(w http.ResponseWriter, r *http.Request) {
		if _, s, ok := srv.sessions.get(r); ok {
			srv.audit(s.User, "signed out", "")
		}
		srv.sessions.end(w, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/overview", srv.needSession(srv.handleOverview))
	mux.HandleFunc("GET /api/v1/containers", srv.needSession(srv.handleContainers))
	mux.HandleFunc("GET /api/v1/hosts", srv.needSession(srv.handleHosts))
	mux.HandleFunc("GET /api/v1/hosts/{name}", srv.needSession(srv.handleHost))
	mux.HandleFunc("GET /api/v1/incidents", srv.needSession(srv.handleIncidents))
	mux.HandleFunc("POST /api/v1/incidents/{id}/ack", srv.needOperator(srv.handleAck))
	mux.HandleFunc("POST /api/v1/alerts/test", srv.needAdmin(srv.handleAlertTest))
	mux.HandleFunc("GET /api/v1/users", srv.needOperator(srv.handleUsers))
	mux.HandleFunc("POST /api/v1/users", srv.needAdmin(srv.handleUserCreate))
	mux.HandleFunc("POST /api/v1/users/{id}", srv.needAdmin(srv.handleUserUpdate))
	mux.HandleFunc("DELETE /api/v1/users/{id}", srv.needAdmin(srv.handleUserDelete))
	mux.HandleFunc("POST /api/v1/users/{id}/setup-link", srv.needAdmin(srv.handleUserSetupLink))
	mux.HandleFunc("DELETE /api/v1/users/{id}/passkeys/{pid}", srv.needAdmin(srv.handlePasskeyDelete))
	mux.HandleFunc("DELETE /api/v1/users/{id}/password", srv.needAdmin(srv.handlePasswordDelete))
	mux.HandleFunc("GET /api/v1/audit", srv.needOperator(srv.handleAudit))
	mux.HandleFunc("GET /api/v1/actions", srv.needOperator(srv.handleActions))
	mux.HandleFunc("GET /api/v1/cloudflare", srv.needSession(srv.handleCloudflare))
	mux.HandleFunc("GET /api/v1/akamai", srv.needSession(srv.handleAkamai))
	mux.HandleFunc("GET /api/v1/mail", srv.needSession(srv.handleMail))
	mux.HandleFunc("GET /api/v1/backups", srv.needSession(srv.handleBackups))
	mux.HandleFunc("GET /api/v1/reports", srv.needSession(srv.handleReports))
	mux.HandleFunc("GET /api/v1/reports/csv/{section}", srv.needSession(srv.handleReportCSV))
	mux.HandleFunc("GET /api/v1/reports/digest", srv.needOperator(srv.handleDigest))
	mux.HandleFunc("POST /api/v1/reports/digest", srv.needAdmin(srv.handleDigest))
	mux.HandleFunc("PUT /api/v1/incidents/{id}/note", srv.needOperator(srv.handleNote))
	mux.HandleFunc("GET /api/v1/checks", srv.needSession(srv.handleChecks))
	mux.HandleFunc("GET /api/v1/certificates", srv.needSession(srv.handleCertificates))
	mux.HandleFunc("POST /api/v1/certificates/reload", srv.needAdmin(srv.handleCertReload))
	mux.HandleFunc("POST /api/v1/certificates/renew", srv.needAdmin(srv.handleCertRenew))
	mux.HandleFunc("GET /api/v1/settings/checks", srv.needOperator(srv.handleSettingsChecks))
	mux.HandleFunc("PUT /api/v1/settings/checks/{id}", srv.needAdmin(srv.handleCheckPut))
	mux.HandleFunc("DELETE /api/v1/settings/checks/{id}", srv.needAdmin(srv.handleCheckDelete))
	mux.HandleFunc("GET /api/v1/settings/maintenance", srv.needOperator(srv.handleSettingsWindows))
	mux.HandleFunc("PUT /api/v1/settings/maintenance/{name}", srv.needAdmin(srv.handleWindowPut))
	mux.HandleFunc("DELETE /api/v1/settings/maintenance/{name}", srv.needAdmin(srv.handleWindowDelete))
	mux.HandleFunc("GET /api/v1/settings", srv.needOperator(srv.handleSettingsSummary))
	mux.HandleFunc("GET /api/v1/settings/channels", srv.needOperator(srv.handleSettingsChannels))
	mux.HandleFunc("PUT /api/v1/settings/channels/{id}", srv.needAdmin(srv.handleChannelPut))
	mux.HandleFunc("DELETE /api/v1/settings/channels/{id}", srv.needAdmin(srv.handleChannelDelete))
	mux.HandleFunc("GET /api/v1/settings/expected/hosts/{host}", srv.needOperator(srv.handleSettingsExpected))
	mux.HandleFunc("PUT /api/v1/settings/expected/hosts/{host}", srv.needAdmin(srv.handleExpectedPut))
	mux.HandleFunc("GET /api/v1/settings/clusters/{cluster}/adopt", srv.needOperator(srv.handleClusterAdopt))
	mux.HandleFunc("PUT /api/v1/settings/clusters/{cluster}/adopt", srv.needAdmin(srv.handleClusterAdoptPut))
	mux.HandleFunc("GET /api/v1/settings/{section}/export", srv.needOperator(srv.handleExport))
	mux.HandleFunc("POST /api/v1/settings/{section}/import", srv.needAdmin(srv.handleImport))
	mux.HandleFunc("GET /api/v1/agents", srv.needOperator(srv.handleAgents))
	mux.HandleFunc("POST /api/v1/agents/join", srv.needAdmin(srv.handleJoinCreate))
	mux.HandleFunc("DELETE /api/v1/agents/join/{id}", srv.needAdmin(srv.handleJoinCancel))
	mux.HandleFunc("DELETE /api/v1/agents/{name}", srv.needAdmin(srv.handleAgentRemove))
	mux.HandleFunc("GET /install.sh", srv.handleInstallScript)
	mux.HandleFunc("GET /cluster.yaml", srv.handleClusterManifest)
	mux.HandleFunc("GET /agent-packages/{file}", srv.handleAgentPackage)
	mux.HandleFunc("GET /api/v1/map/network", srv.needSession(srv.handleMapNetwork))
	mux.HandleFunc("GET /api/v1/map/services", srv.needSession(srv.handleMapServices))
	mux.HandleFunc("GET /api/v1/map/find", srv.needSession(srv.handleMapFind))
	mux.HandleFunc("GET /api/v1/map/found", srv.needSession(srv.handleMapFound))
	mux.HandleFunc("GET /api/v1/map/corrections", srv.needSession(srv.handleMapCorrections))
	mux.HandleFunc("POST /api/v1/map/corrections", srv.needAdmin(srv.handleMapCorrectionPost))
	mux.HandleFunc("GET /api/v1/datalayer", srv.needSession(srv.handleDataLayer))
	mux.HandleFunc("GET /api/v1/clusters", srv.needSession(srv.handleClusters))
	mux.HandleFunc("GET /api/v1/workloads", srv.needSession(srv.handleWorkloadList))
	mux.HandleFunc("GET /api/v1/workloads/hours", srv.needSession(srv.handleWorkloadHours))
	mux.HandleFunc("POST /api/v1/cloudflare/dns/{zone}/accept", srv.needAdmin(srv.handleDNSAccept))
	mux.HandleFunc("POST /api/v1/akamai/dns/{zone}/accept", srv.needAdmin(srv.handleAkamaiDNSAccept))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such API"})
	})
	mux.HandleFunc("/auth/", http.NotFound)
	mux.HandleFunc("/", srv.handleUI)
	return securityHeaders(srv.csp, mux)
}

// AgentHandler is the agent listener: reports and a health check, nothing else.
// With agent_listen set, the browser listener doesn't take reports at all.
func (srv *Server) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agent/report", srv.handleReport)
	mux.HandleFunc("GET /api/v1/agent/checks", srv.handleAgentChecks)
	mux.HandleFunc("GET /api/v1/agent/actions", srv.handleAgentActions)
	mux.HandleFunc("POST /api/v1/agent/workloads", srv.handleWorkloads)
	if srv.atls != nil {
		mux.HandleFunc("POST /api/v1/agent/enroll", srv.handleEnroll)
		mux.HandleFunc("POST /api/v1/agent/renew", srv.handleRenew)
		mux.HandleFunc("POST /api/v1/agent/join", srv.handleJoin)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok " + version.Version + "\n"))
	})
	mux.HandleFunc("GET /readyz", srv.handleReady)
	return mux
}

// handleReady is the readiness probe: /healthz says the process is alive,
// /readyz that this copy should get requests. It isn't ready until it has
// caught up with the other copies (at once when it's the only one), and
// stops being ready as soon as it's told to stop, so a rolling upgrade
// moves traffic off it before it closes anything.
func (srv *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case srv.draining.Load():
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("stopping\n"))
	case !isClosed(srv.caughtUp):
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("starting: catching up with the other copies\n"))
	default:
		_, _ = w.Write([]byte("ready " + version.Version + "\n"))
	}
}

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func securityHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// needSession guards the browser API. State-changing requests must also
// carry the X-Beacon header, which a cross-site form can't add.
func (srv *Server) needSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, s, ok := srv.sessions.get(r)
		if ok && !srv.recheck(r.Context(), tok, s) {
			srv.sessions.drop(tok)
			ok = false
		}
		if !ok {
			writeJSON(w, http.StatusUnauthorized, srv.signinOptions())
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Beacon") != "1" {
			http.Error(w, "missing X-Beacon header", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (srv *Server) signinOptions() map[string]any {
	return map[string]any{"signin": map[string]any{"dev": srv.cfg.DevSignin, "providers": srv.providerList()}}
}

func (srv *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	tok, s, ok := srv.sessions.get(r)
	if ok && !srv.recheck(r.Context(), tok, s) {
		srv.sessions.drop(tok)
		ok = false
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, srv.signinOptions())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": s.User, "email": s.Email, "role": s.Role, "provider": s.Provider, "dev": s.Dev, "version": version.Version, "door": atDoor(r)})
}

func (srv *Server) handleDevSignin(w http.ResponseWriter, r *http.Request) {
	if !srv.cfg.DevSignin {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("X-Beacon") != "1" {
		http.Error(w, "missing X-Beacon header", http.StatusForbidden)
		return
	}
	srv.sessions.create(w, r, "Developer (local)", true)
	srv.audit("Developer (local)", "signed in", "development sign-in")
	w.WriteHeader(http.StatusNoContent)
}

const notBuilt = `<!doctype html><meta charset="utf-8"><title>Cairn OBS</title>
<p style="font:16px system-ui;margin:3em">obs-beacond is running, but its web interface wasn't built into it. Run <code>make web</code>, then rebuild obs-beacond.</p>`

func (srv *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if srv.index == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(notBuilt))
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p != "" && p != "index.html" {
		if st, err := fs.Stat(srv.ui, p); err == nil && !st.IsDir() {
			if strings.HasPrefix(p, immutableDir+"/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, srv.ui, p)
			return
		}
		if srv.otherVersionsFile(w, r, p) {
			return
		}
		if strings.HasPrefix(p, immutableDir+"/") {
			http.NotFound(w, r) // a script that doesn't exist mustn't come back as the page
			return
		}
	}
	// Every other path is a page of the single-page app.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(srv.index)
}

// Close closes the database, if there is one.
func (srv *Server) Close() error {
	if srv.db == nil {
		return nil
	}
	return srv.db.Close()
}

// ListenAndServe runs until ctx is done, then shuts down gracefully.
func (srv *Server) ListenAndServe(ctx context.Context) error {
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr: addr, Handler: h,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
	}
	servers := []*http.Server{newServer(srv.cfg.Listen, srv.Handler())}
	if srv.acme != nil {
		servers[0].TLSConfig = srv.webTLSConfig()
		if a := srv.cfg.TLS.ACME; a.HTTPListen != "" {
			servers = append(servers, newServer(a.HTTPListen, srv.acmeHTTPHandler()))
		}
		go srv.warmACME(ctx)
	}
	if srv.cfg.AgentListen != "" {
		as := newServer(srv.cfg.AgentListen, srv.AgentHandler())
		as.TLSConfig = srv.AgentTLSConfig()
		servers = append(servers, as)
	}
	if srv.cfg.EmergencyListen != "" {
		servers = append(servers, newServer(srv.cfg.EmergencyListen, srv.EmergencyHandler()))
	}
	errc := make(chan error, len(servers))
	for i, hs := range servers {
		go func() {
			var err error
			switch {
			case i == 0 && srv.cfg.TLS.Cert != "":
				err = hs.ListenAndServeTLS(srv.cfg.TLS.Cert, srv.cfg.TLS.Key)
			case hs.TLSConfig != nil: // the agent listener: certificates from the agent CA
				err = hs.ListenAndServeTLS("", "")
			default:
				err = hs.ListenAndServe()
			}
			errc <- err
		}()
	}
	agents := srv.agentListenerDescription()
	log.Printf("obs-beacond %s listening on %s, agents %s (%d agents, %d alert channels)", version.Version, srv.cfg.Listen, agents, len(srv.agentList()), len(srv.alerts.chans()))
	go srv.alerts.run(ctx)
	if srv.db != nil && srv.cfg.Snapshot != "" {
		go srv.snapshots(ctx)
	}
	srv.shareAssets()
	go srv.follow(ctx)
	// The evaluator's jobs: history, the digest, sending the outbox and
	// polling Cloudflare, Patroni and the mail round trip write to the
	// database, send or ask something outside, so one copy does them.
	go srv.runLead(ctx, func(ctx context.Context) {
		if srv.db != nil {
			go srv.alerts.sendOutbox(ctx)
		}
		go srv.recordHistory(ctx)
		go srv.runDigest(ctx)
		go srv.recordEdges(ctx)
		if srv.db != nil {
			go srv.purgeWorkloads(ctx.Done())
		}
		if srv.cf != nil {
			go srv.poll(ctx, readingCloudflare, srv.cf.Config().Every, func(ctx context.Context) { srv.cf.Poll(ctx, time.Now()) })
		}
		if srv.ak != nil {
			go srv.poll(ctx, readingAkamai, srv.ak.Config().Every, func(ctx context.Context) { srv.ak.Poll(ctx, time.Now()) })
		}
		if srv.readingOn(readingPatroni) {
			go srv.pollPatroni(ctx)
		}
		if srv.mail != nil {
			go srv.poll(ctx, readingMail, srv.mail.Config().Every, srv.mail.Round)
		}
		go srv.pollVaults(ctx)
	})
	go func() { // evaluate alerts every 15 seconds, on the evaluator
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if srv.leading() && srv.evaluate(now) {
					srv.publish(noticeIncidents, "")
				}
			}
		}
	}()
	select {
	case err := <-errc:
		srv.stepDown("")
		return err
	case <-ctx.Done():
	}
	// Stop being ready, and give the evaluator lease up before draining,
	// so another copy takes over while this one finishes its requests.
	// runLead does this too, but the process may be gone before it gets
	// the chance.
	srv.draining.Store(true)
	srv.stepDown("")
	if d := srv.cfg.ShutdownDelay; d > 0 {
		log.Printf("obs-beacond: stopping; still serving for %s while traffic moves off", d)
		time.Sleep(d)
	}
	grace := srv.cfg.ShutdownGrace
	if grace <= 0 {
		grace = 30 * time.Second
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for _, hs := range servers {
		if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}
