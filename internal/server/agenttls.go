package server

// The agent listener's TLS: obs-beacond's own CA
// signs the listener's certificate and one client certificate per agent.
// An agent enrolls once with its token and from then on proves who it is
// with its certificate; the token is good for nothing else on this
// listener. See internal/pki for why agents pin the CA and skip the name
// check, and docs/protocol.md for the exchange.

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/pki"
	"github.com/cairnobs/cairnobs/internal/state"
)

// AgentTLSConfig is the agent listener's TLS. It is on whenever
// agent_listen is set, unless turned off.
type AgentTLSConfig struct {
	// Off serves the agent listener as plain HTTP with bearer tokens, as
	// before. Only for a listener inside a VPN, and only until its agents
	// have moved over.
	Off bool `yaml:"off"`
	// Names adds host names or addresses to the listener certificate. Agents
	// don't check them; they help curl and other tools. The host name and
	// the machine's addresses are always in it.
	Names []string `yaml:"names"`
}

// renewServerBefore is how long before its end the listener's certificate
// is replaced.
const renewServerBefore = 30 * 24 * time.Hour

type agentTLS struct {
	ca    *pki.CA
	names []string

	mu   sync.Mutex
	cert *tls.Certificate
}

// AgentCAFile is where obs-beacond writes the agent CA's certificate (public)
// for `obs-beacond ca` and for people: in data_dir.
func AgentCAFile(cfg *Config) string {
	if cfg.Dir() == "" {
		return ""
	}
	return filepath.Join(cfg.Dir(), "agent-ca.pem")
}

// agentTLSWanted says whether this config serves the agent listener over TLS.
func agentTLSWanted(cfg *Config) bool { return cfg.AgentListen != "" && !cfg.AgentTLS.Off }

// setupAgentTLS loads the agent CA from the database, making it on the
// first start, and writes its certificate to AgentCAFile.
func setupAgentTLS(cfg *Config, db *state.DB, now time.Time) (*agentTLS, error) {
	if !agentTLSWanted(cfg) {
		return nil, nil
	}
	if db == nil {
		return nil, errors.New("agent_listen serves TLS with obs-beacond's own CA, which is kept in the database: set database and state_key_file (or agent_tls.off: true for plain HTTP inside a VPN)")
	}
	raw, err := db.AgentCA()
	if err != nil {
		return nil, err
	}
	var ca *pki.CA
	if raw == nil {
		if ca, err = pki.NewCA(now); err != nil {
			return nil, err
		}
		b, err := ca.Marshal()
		if err != nil {
			return nil, err
		}
		if err := db.SetAgentCA(b); err != nil {
			return nil, err
		}
		log.Printf("made the agent CA, %s", ca.Fingerprint())
	} else if ca, err = pki.ParseCA(raw); err != nil {
		return nil, fmt.Errorf("agent CA: %w", err)
	}
	if p := AgentCAFile(cfg); p != "" {
		if err := writeFileAtomic(p, ca.CertPEM(), 0o644); err != nil {
			log.Printf("can't write %s (agents don't need it; `obs-beacond ca` does): %v", p, err)
		}
	}
	return &agentTLS{ca: ca, names: listenerNames(cfg.AgentTLS.Names)}, nil
}

// listenerNames is extra plus this machine's name and addresses.
func listenerNames(extra []string) []string {
	names := append([]string{"localhost"}, extra...)
	if h, err := os.Hostname(); err == nil {
		names = append(names, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				names = append(names, ipn.IP.String())
			}
		}
	}
	return names
}

func (t *agentTLS) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.cert == nil || now.After(t.cert.Leaf.NotAfter.Add(-renewServerBefore)) {
		c, err := t.ca.ServerCert(t.names, now)
		if err != nil {
			return nil, err
		}
		t.cert = &c
	}
	return t.cert, nil
}

// tlsConfig asks for a client certificate but doesn't require one: an agent
// enrolling has none yet. Routes other than enroll refuse a request without
// one (agentFor).
func (t *agentTLS) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: t.getCertificate,
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      t.ca.Pool(),
	}
}

// AgentTLSConfig is the agent listener's TLS config; nil when it's plain HTTP.
func (srv *Server) AgentTLSConfig() *tls.Config {
	if srv.atls == nil {
		return nil
	}
	return srv.atls.tlsConfig()
}

// agentFromCert is the agent whose verified client certificate the request
// carries. The certificate must be that agent's live one (enrolling again or
// renewing revokes the old), and the agent must still be in the config:
// removing it from `agents` cuts it off at once.
func (srv *Server) agentFromCert(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", false
	}
	leaf := r.TLS.VerifiedChains[0][0]
	name := leaf.Subject.CommonName
	if !srv.knownAgent(name) {
		return "", false
	}
	live, err := srv.db.AgentCertLive(pki.SerialHex(leaf.SerialNumber), name)
	if err != nil {
		log.Printf("agent certificates: %v", err)
		return "", false
	}
	return name, live
}

// agentRefusal is the 401's text for reports and checks.
func (srv *Server) agentRefusal() string {
	if srv.atls != nil {
		return "no live agent certificate (a token only enrolls on this listener)"
	}
	return "unknown agent token"
}

// maxCSR bounds a certificate request.
const maxCSR = 16 << 10

// Enrolled is the answer to enroll and renew.
type Enrolled struct {
	Cert     string    `json:"cert"` // PEM
	CA       string    `json:"ca"`   // PEM
	NotAfter time.Time `json:"not_after"`
	Serial   string    `json:"serial"`
}

// handleEnroll signs an agent's first certificate (or a new one, after it
// lost its key): the request proves the agent by its token.
func (srv *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	name, ok := srv.agentByToken(r)
	if !ok {
		http.Error(w, "unknown agent token", http.StatusUnauthorized)
		return
	}
	srv.issueAgentCert(w, r, name, "enrolled")
}

// handleRenew replaces an agent's certificate before it runs out: the
// request proves the agent by its current one.
func (srv *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	name, ok := srv.agentFromCert(r)
	if !ok {
		http.Error(w, "no live agent certificate", http.StatusUnauthorized)
		return
	}
	srv.issueAgentCert(w, r, name, "renewed its certificate")
}

func (srv *Server) issueAgentCert(w http.ResponseWriter, r *http.Request, name, action string) {
	csr, err := readCSR(w, r)
	if err != nil {
		return
	}
	en, err := srv.signAgent(csr, name, time.Now())
	if errors.Is(err, errBadCSR) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	srv.audit("agent "+name, action, fmt.Sprintf("certificate %s, valid until %s", en.Serial, en.NotAfter.UTC().Format("2006-01-02")))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(en)
}

// readCSR reads a certificate request body, answering the request itself
// when it can't.
func readCSR(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	csr, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCSR))
	if err != nil {
		http.Error(w, "certificate request too large", http.StatusRequestEntityTooLarge)
	}
	return csr, err
}

var errBadCSR = errors.New("bad certificate request")

// signAgent signs and records an agent's certificate; the new one revokes
// any it had.
func (srv *Server) signAgent(csr []byte, name string, now time.Time) (Enrolled, error) {
	iss, err := srv.atls.ca.SignAgent(csr, name, now)
	if err != nil {
		return Enrolled{}, fmt.Errorf("%w: %v", errBadCSR, err)
	}
	if err := srv.db.IssueAgentCert(state.AgentCert{Serial: iss.Serial, Agent: name, Issued: now, NotAfter: iss.NotAfter}); err != nil {
		log.Printf("agent certificates: %v", err)
		return Enrolled{}, errors.New("can't record the certificate")
	}
	return Enrolled{Cert: string(iss.PEM), CA: string(srv.atls.ca.CertPEM()), NotAfter: iss.NotAfter, Serial: iss.Serial}, nil
}

// writeFileAtomic writes through a temporary file and a rename, so a reader
// never sees half a file.
func writeFileAtomic(p string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// agentListenerDescription is the start-up log's word on the agent listener.
func (srv *Server) agentListenerDescription() string {
	if srv.cfg.AgentListen == "" {
		return "on the same address"
	}
	if srv.atls == nil {
		return "on " + srv.cfg.AgentListen + " (plain HTTP: agent_tls.off)"
	}
	return "on " + srv.cfg.AgentListen + " (TLS, agent CA " + srv.atls.ca.Fingerprint() + ")"
}
