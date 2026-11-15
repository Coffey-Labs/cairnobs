package server

// Adding a host from the UI: an admin makes a
// one-time join token for a name, and runs the one line the UI shows on the
// new host. install.sh installs the agent package and runs
// `obs-beacon-agent join`, which sends a key's certificate request with the
// token's secret to POST /api/v1/agent/join; obs-beacond spends the token,
// adds the agent to its database and signs its certificate. From then on the
// agent is like any other on the TLS listener.
//
// Agents can still be listed in obs-beacond.yaml (`agents`, with tokens);
// both kinds count everywhere an agent name does. A name can't be in both.

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/jointoken"
	"github.com/cairnobs/cairnobs/internal/state"
	"github.com/cairnobs/cairnobs/internal/version"
)

// JoinConfig is how new hosts reach obs-beacond when they join.
type JoinConfig struct {
	// AgentURL is the agent listener as new hosts reach it, e.g.
	// https://monitor.example.org:8461. Default: alerts.public_url's host
	// with agent_listen's port.
	AgentURL string `yaml:"agent_url"`
	// PackagesDir is served at /agent-packages/ for install.sh: the agent's
	// .deb and .rpm files and SHA256SUMS from a release. For networks that
	// can't reach the public download, and before there is one.
	PackagesDir string `yaml:"packages_dir"`
	// PackagesURL is where install.sh fetches packages otherwise: a release
	// download URL holding the same files.
	PackagesURL string `yaml:"packages_url"`
	// Image is where clusters pull the obs-beacond image from, which
	// carries the cluster agent: e.g. registry.example.org/ops/obs-beacond,
	// for clusters that can't reach the public one. Without it,
	// DefaultClusterImage. Without a tag, this obs-beacond's version is used.
	Image string `yaml:"image"`
}

// joinTTL is how long a join token is good for.
const joinTTL = time.Hour

// agentName is what a joined agent may be called: a short host name.
var agentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// loadJoined reads the joined agents into memory and the store.
func (srv *Server) loadJoined() error {
	srv.joined = map[string]state.JoinedAgent{}
	if srv.db == nil {
		return nil
	}
	list, err := srv.db.JoinedAgents()
	if err != nil {
		return err
	}
	for _, a := range list {
		if srv.inConfig(a.Name) {
			log.Printf("agent %s is in obs-beacond.yaml and joined too; the config file's entry is used", a.Name)
			continue
		}
		srv.joined[a.Name] = a
		if !a.Cluster {
			srv.store.Add(a.Name)
		}
	}
	return nil
}

// checkAgentNames checks that everything in the config naming an agent
// names one, from the config file or joined.
func (srv *Server) checkAgentNames() error {
	known := map[string]bool{}
	for _, a := range srv.agentList() {
		known[a.Name] = true
	}
	hostsOnly := maps.Clone(known)
	for _, c := range srv.clusterAgents() {
		known[c] = true // a maintenance window can cover a cluster
	}
	for _, w := range srv.cfg.Maintenance {
		for _, h := range w.Hosts {
			if !known[h] {
				return fmt.Errorf("maintenance window %q: no agent called %q", w.Name, h)
			}
		}
	}
	if m := srv.cfg.Mail; m != nil && m.Host != "" && !hostsOnly[m.Host] {
		return fmt.Errorf("mail: no agent called %q", m.Host)
	}
	if srv.cfg.DataLayer != nil {
		return srv.cfg.DataLayer.check(hostsOnly)
	}
	return nil
}

func (srv *Server) inConfig(name string) bool {
	for _, a := range srv.cfg.Agents {
		if a.Name == name {
			return true
		}
	}
	return false
}

// agentList is every host's agent: the config file's, then the joined
// ones. Joined agents have no token. Cluster agents aren't hosts: they're
// in clusterAgents.
func (srv *Server) agentList() []AgentConfig {
	out := hostAgents(srv.cfg.Agents)
	srv.joinedMu.RLock()
	defer srv.joinedMu.RUnlock()
	names := make([]string, 0, len(srv.joined))
	for n := range srv.joined {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if j := srv.joined[n]; !j.Cluster {
			out = append(out, AgentConfig{Name: n, Workstation: j.Workstation})
		}
	}
	return out
}

// hostAgents leaves the cluster agents out.
func hostAgents(all []AgentConfig) []AgentConfig {
	out := make([]AgentConfig, 0, len(all))
	for _, a := range all {
		if !a.Cluster {
			out = append(out, a)
		}
	}
	return out
}

// clusterAgents is every cluster agent's name, from the config file and
// joined, sorted.
func (srv *Server) clusterAgents() []string {
	var out []string
	for _, a := range srv.cfg.Agents {
		if a.Cluster {
			out = append(out, a.Name)
		}
	}
	srv.joinedMu.RLock()
	for n, j := range srv.joined {
		if j.Cluster && !srv.inConfig(n) {
			out = append(out, n)
		}
	}
	srv.joinedMu.RUnlock()
	sort.Strings(out)
	return out
}

// clusterAgent says whether name is a cluster agent.
func (srv *Server) clusterAgent(name string) bool {
	for _, a := range srv.cfg.Agents {
		if a.Name == name {
			return a.Cluster
		}
	}
	srv.joinedMu.RLock()
	defer srv.joinedMu.RUnlock()
	return srv.joined[name].Cluster
}

func (srv *Server) knownAgent(name string) bool {
	if srv.inConfig(name) {
		return true
	}
	srv.joinedMu.RLock()
	defer srv.joinedMu.RUnlock()
	_, ok := srv.joined[name]
	return ok
}

// workstation reports whether the named agent is a workstation.
func (srv *Server) workstation(name string) bool {
	if srv.inConfig(name) {
		return srv.cfg.workstation(name)
	}
	srv.joinedMu.RLock()
	defer srv.joinedMu.RUnlock()
	return srv.joined[name].Workstation
}

// joinAgentURL is the agent listener's URL for join tokens; "" when it
// can't be worked out.
func (srv *Server) joinAgentURL(r *http.Request) string {
	if u := strings.TrimRight(srv.cfg.Join.AgentURL, "/"); u != "" {
		return u
	}
	_, port, err := net.SplitHostPort(srv.cfg.AgentListen)
	if err != nil {
		return ""
	}
	host := ""
	if pu, err := url.Parse(srv.cfg.Alerts.PublicURL); err == nil && pu.Hostname() != "" {
		host = pu.Hostname()
	} else if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	} else {
		host = r.Host
	}
	if host == "" || loopbackHost(host) {
		return ""
	}
	return "https://" + net.JoinHostPort(host, port)
}

// publicBase is the web address new hosts fetch install.sh from.
func (srv *Server) publicBase(r *http.Request) string {
	if srv.cfg.Alerts.PublicURL != "" {
		return srv.cfg.Alerts.PublicURL
	}
	scheme := "https"
	if r.TLS == nil && loopbackHost(hostOnly(r.Host)) {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func hostOnly(hp string) string {
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	return hp
}

type joinTokenView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Workstation bool      `json:"workstation"`
	Cluster     bool      `json:"cluster"`
	CreatedBy   string    `json:"created_by"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
}

type agentView struct {
	Name        string     `json:"name"`
	Source      string     `json:"source"` // config or joined
	Workstation bool       `json:"workstation"`
	Added       *time.Time `json:"added,omitempty"`
	AddedBy     string     `json:"added_by,omitempty"`
	CertUntil   *time.Time `json:"cert_until,omitempty"`
	// How it's doing, as on the hosts list.
	Status string `json:"status"`
	Word   string `json:"word"`
}

// joinUnavailable says why hosts can't join here; "" when they can.
func (srv *Server) joinUnavailable(r *http.Request) string {
	switch {
	case srv.db == nil:
		return "Joining needs the database (database and state_key_file in obs-beacond.yaml)."
	case srv.atls == nil:
		return "Joining needs the TLS agent listener (agent_listen, without agent_tls.off)."
	case srv.joinAgentURL(r) == "":
		return "obs-beacond can't tell how new hosts reach its agent listener: set join.agent_url (or alerts.public_url) in obs-beacond.yaml."
	}
	return ""
}

// clusterJoinUnavailable says why clusters can't join here; "" when they
// can. It's what stops hosts joining: the image has a default.
func (srv *Server) clusterJoinUnavailable(r *http.Request) string {
	return srv.joinUnavailable(r)
}

// handleAgents lists the agents and the pending join tokens.
func (srv *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	certs := map[string]time.Time{}
	if srv.db != nil {
		if live, err := srv.db.LiveAgentCerts(); err == nil {
			for _, c := range live {
				certs[c.Agent] = c.NotAfter
			}
		}
	}
	state := map[string]HostView{}
	hosts, _ := srv.view(time.Now())
	for _, h := range hosts {
		state[h.Name] = h
	}
	// Paged like the other lists (lists.go): ?q= (part of the name),
	// ?offset=, ?limit= (100, at most 500).
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	offset, limit := page(r, 100, 500)
	agents := []agentView{}
	for _, a := range srv.agentList() {
		if q != "" && !strings.Contains(strings.ToLower(a.Name), q) {
			continue
		}
		v := agentView{Name: a.Name, Source: "config", Workstation: a.Workstation, Status: state[a.Name].Status, Word: state[a.Name].Word}
		srv.joinedMu.RLock()
		if j, ok := srv.joined[a.Name]; ok && !srv.inConfig(a.Name) {
			v.Source, v.Added, v.AddedBy = "joined", &j.Added, j.AddedBy
		}
		srv.joinedMu.RUnlock()
		if t, ok := certs[a.Name]; ok {
			v.CertUntil = &t
		}
		agents = append(agents, v)
	}
	pending := []joinTokenView{}
	if srv.db != nil {
		ts, err := srv.db.PendingJoinTokens(time.Now())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		for _, t := range ts {
			pending = append(pending, joinTokenView{ID: t.TokenSHA256[:12], Name: t.Agent, Workstation: t.Workstation, Cluster: t.Cluster, CreatedBy: t.CreatedBy, Created: t.Created, Expires: t.Expires})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": window(agents, offset, limit), "total": len(agents), "offset": offset, "limit": limit,
		"pending": pending, "join_unavailable": srv.joinUnavailable(r), "clusters": srv.clusterAgentViews(certs),
		"cluster_join_unavailable": srv.clusterJoinUnavailable(r)})
}

// handleJoinCreate makes a join token and the line to run on the new host.
func (srv *Server) handleJoinCreate(w http.ResponseWriter, r *http.Request) {
	if why := srv.joinUnavailable(r); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": why})
		return
	}
	var req struct {
		Name        string `json:"name"`
		Workstation bool   `json:"workstation"`
		Cluster     bool   `json:"cluster"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	what := "host"
	if req.Cluster {
		what = "cluster"
		if why := srv.clusterJoinUnavailable(r); why != "" {
			writeJSON(w, http.StatusConflict, map[string]string{"error": why})
			return
		}
		if req.Workstation {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A cluster isn't a workstation."})
			return
		}
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	if !agentName.MatchString(req.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A " + what + " name is lower-case letters, digits and dashes, up to 63 characters."})
		return
	}
	if srv.knownAgent(req.Name) || req.Name == serverSource {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "There is already a host or cluster called " + req.Name + "."})
		return
	}
	now := time.Now()
	pending, err := srv.db.PendingJoinTokens(now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, p := range pending {
		if p.Agent == req.Name {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "A join command for " + req.Name + " is already waiting; cancel it first."})
			return
		}
	}
	secret := newToken()
	t := state.JoinToken{TokenSHA256: HashToken(secret), Agent: req.Name, Workstation: req.Workstation, Cluster: req.Cluster,
		Created: now, CreatedBy: srv.who(r), Expires: now.Add(joinTTL)}
	if err := srv.db.AddJoinToken(t); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tok := jointoken.Token{Server: srv.joinAgentURL(r), CAFingerprint: srv.atls.ca.Fingerprint(), Secret: secret}.Encode()
	srv.audit(srv.who(r), "made a join command", fmt.Sprintf("for %s %s, good until %s", what, req.Name, t.Expires.UTC().Format("15:04 UTC")))
	command := fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- --join %s", srv.publicBase(r), tok)
	// Windows: PowerShell as Administrator (docs/windows.md). The script
	// is fetched and run with the token as its argument, which keeps the
	// token out of the address, as on Linux.
	windows := fmt.Sprintf("& ([scriptblock]::Create((irm %s/install.ps1))) -Join %s", srv.publicBase(r), tok)
	if req.Cluster {
		// The token goes in a header, not the address: addresses end up
		// in proxies' and servers' logs.
		command = fmt.Sprintf("curl -fsSL -H 'Authorization: Bearer %s' %s/cluster.yaml | kubectl apply -f -", tok, srv.publicBase(r))
		windows = ""
	}
	out := map[string]any{
		"name":    req.Name,
		"token":   tok,
		"command": command,
		"expires": t.Expires,
	}
	if windows != "" {
		out["windows_command"] = windows
	}
	writeJSON(w, http.StatusOK, out)
}

func (srv *Server) handleJoinCancel(w http.ResponseWriter, r *http.Request) {
	if srv.db == nil {
		http.NotFound(w, r)
		return
	}
	ok, err := srv.db.CancelJoinToken(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No such join command (used, cancelled or run out)."})
		return
	}
	srv.audit(srv.who(r), "cancelled a join command", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentRemove removes a joined agent: its certificate stops working
// at once. Agents in obs-beacond.yaml are removed there.
func (srv *Server) handleAgentRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if srv.inConfig(name) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": name + " is listed in obs-beacond.yaml; remove it there."})
		return
	}
	if srv.db == nil {
		http.NotFound(w, r)
		return
	}
	srv.joinedMu.Lock()
	cluster := srv.joined[name].Cluster
	ok, err := srv.db.RemoveJoinedAgent(name, time.Now())
	if ok {
		delete(srv.joined, name)
	}
	srv.joinedMu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No joined host or cluster called " + name + "."})
		return
	}
	what := "a host"
	if cluster {
		what = "a cluster"
	} else {
		srv.store.Remove(name)
	}
	// Its cluster's records, or what it read beside its host.
	srv.forgetAgentSources(name)
	if srv.shared() {
		_ = srv.db.DeleteLive(name)
		srv.publish(noticeJoined, "")
	}
	srv.audit(srv.who(r), "removed "+what, name+" (its certificate is revoked)")
	w.WriteHeader(http.StatusNoContent)
}

// JoinAnswer is the answer to join: the certificate, and the name the
// agent was given.
type JoinAnswer struct {
	Enrolled
	Agent string `json:"agent"`
}

// handleJoin spends a join token and signs the new agent's first
// certificate.
func (srv *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || secret == "" {
		http.Error(w, "no join token", http.StatusUnauthorized)
		return
	}
	csr, err := readCSR(w, r)
	if err != nil {
		return
	}
	now := time.Now()
	// Check the request before spending the token on it.
	if _, err := srv.atls.ca.CheckAgentCSR(csr); err != nil {
		http.Error(w, "bad certificate request: "+err.Error(), http.StatusBadRequest)
		return
	}
	srv.joinedMu.Lock()
	a, err := srv.db.UseJoinToken(HashToken(secret), now, srv.inConfig)
	if err == nil {
		srv.joined[a.Name] = a
	}
	srv.joinedMu.Unlock()
	if err == nil {
		srv.publish(noticeJoined, "")
	}
	if errors.Is(err, state.ErrJoinToken) {
		http.Error(w, "unknown, used or expired join token: make a new one in the UI", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	joined := "joined"
	if a.Cluster {
		joined = "joined as a cluster agent"
	} else {
		srv.store.Add(a.Name)
	}
	srv.audit("agent "+a.Name, joined, "with a join command from "+a.AddedBy+", from "+hostOnly(r.RemoteAddr))
	en, err := srv.signAgent(csr, a.Name, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, JoinAnswer{Enrolled: en, Agent: a.Name})
}

// The install script, for `curl .../install.sh | sudo sh -s -- --join ...`.

func (srv *Server) packagesBase(r *http.Request) string {
	switch {
	case srv.cfg.Join.PackagesDir != "":
		return srv.publicBase(r) + "/agent-packages"
	case srv.cfg.Join.PackagesURL != "":
		return strings.TrimRight(srv.cfg.Join.PackagesURL, "/")
	}
	return ""
}

func (srv *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	s := strings.NewReplacer("@VERSION@", shellQuote(version.Version), "@PACKAGES@", shellQuote(srv.packagesBase(r))).Replace(installScript)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s))
}

// handleAgentPackage serves a file from join.packages_dir: nothing else,
// and no listing.
func (srv *Server) handleAgentPackage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if srv.cfg.Join.PackagesDir == "" || name != path.Base(name) || strings.HasPrefix(name, ".") ||
		!(name == "SHA256SUMS" || name == "SHA256SUMS.minisig" || strings.HasSuffix(name, ".deb") || strings.HasSuffix(name, ".rpm") ||
			strings.HasSuffix(name, "_windows_amd64.zip") || strings.HasSuffix(name, "_windows_arm64.zip")) {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(srv.cfg.Join.PackagesDir, name)
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, p)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// psQuote quotes for PowerShell: single quotes, a quote doubled.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// installPS1 is the Windows install script, for `& ([scriptblock]::Create((irm
// .../install.ps1))) -Join <token>` in PowerShell as Administrator.
//
//go:embed install.ps1
var installPS1 string

func (srv *Server) handleInstallPS1(w http.ResponseWriter, r *http.Request) {
	s := strings.NewReplacer("@VERSION@", psQuote(version.Version), "@PACKAGES@", psQuote(srv.packagesBase(r))).Replace(installPS1)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s))
}

const installScript = `#!/bin/sh
# Cairn OBS: install obs-beacon-agent on this host and join it to the
# obs-beacond that served this script.
#
#   curl -fsSL https://<server>/install.sh | sudo sh -s -- --join <token>
#
# Options: --join <token> (from Hosts, Add host), --packages <url> (where
# the agent's .deb/.rpm and SHA256SUMS are), --force (replace an existing
# /etc/obs-beacon/agent.yaml).
set -eu

VERSION=@VERSION@
PACKAGES=@PACKAGES@
TOKEN=""
FORCE=""

say() { printf 'obs-beacon install: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --join) [ $# -ge 2 ] || die "--join needs the token"; TOKEN=$2; shift 2 ;;
    --packages) [ $# -ge 2 ] || die "--packages needs a URL"; PACKAGES=${2%/}; shift 2 ;;
    --force) FORCE=-force; shift ;;
    *) die "unknown option $1" ;;
  esac
done
[ -n "$TOKEN" ] || die "run it with --join <token>: Hosts, Add host in the web interface makes one"
[ "$(id -u)" = 0 ] || die "run it as root (sudo sh)"
command -v systemctl >/dev/null 2>&1 || die "this host has no systemd; see docs/install.md"

if command -v obs-beacon-agent >/dev/null 2>&1; then
  say "obs-beacon-agent is installed already: $(obs-beacon-agent -version)"
else
  case "$(uname -m)" in
    x86_64|amd64) DEB_ARCH=amd64 RPM_ARCH=x86_64 ;;
    aarch64|arm64) DEB_ARCH=arm64 RPM_ARCH=aarch64 ;;
    *) die "no package for $(uname -m)" ;;
  esac
  [ -n "$PACKAGES" ] || die "this server doesn't say where agent packages are (join.packages_dir or join.packages_url in obs-beacond.yaml). Install obs-beacon-agent by hand (docs/install.md), then: sudo obs-beacon-agent join <token>"
  PKG_VERSION=$(printf '%s' "${VERSION#v}" | sed 's/-/~/')
  if command -v dpkg >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
    FILE="obs-beacon-agent_${PKG_VERSION}_${DEB_ARCH}.deb"
  elif command -v rpm >/dev/null 2>&1 && { command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; }; then
    FILE="obs-beacon-agent-${PKG_VERSION}-1.${RPM_ARCH}.rpm"
  else
    die "neither apt nor dnf/yum here; see docs/install.md"
  fi
  command -v curl >/dev/null 2>&1 || die "curl is needed"
  command -v sha256sum >/dev/null 2>&1 || die "sha256sum is needed"
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  say "fetching $FILE from $PACKAGES"
  curl -fsSL -o "$TMP/SHA256SUMS" "$PACKAGES/SHA256SUMS" || die "can't fetch $PACKAGES/SHA256SUMS"
  curl -fsSL -o "$TMP/$FILE" "$PACKAGES/$FILE" || die "can't fetch $PACKAGES/$FILE"
  (cd "$TMP" && awk -v f="$FILE" '$2 == f' SHA256SUMS | grep -q . && awk -v f="$FILE" '$2 == f' SHA256SUMS | sha256sum -c --quiet -) || die "$FILE doesn't match SHA256SUMS"
  case "$FILE" in
    *.deb) DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$TMP/$FILE" >/dev/null ;;
    *.rpm) if command -v dnf >/dev/null 2>&1; then dnf install -y -q "$TMP/$FILE" >/dev/null; else yum install -y -q "$TMP/$FILE" >/dev/null; fi ;;
  esac
  say "installed $(obs-beacon-agent -version)"
fi

obs-beacon-agent join $FORCE "$TOKEN"
systemctl daemon-reload
systemctl enable --now obs-beacon-agent-helper.service >/dev/null 2>&1 || true
systemctl enable obs-beacon-agent.service >/dev/null 2>&1
systemctl restart obs-beacon-agent.service
say "done: obs-beacon-agent is running and reporting"
`
