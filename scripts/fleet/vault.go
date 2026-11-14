package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Two Vault clusters obs-beacond reads itself (docs/vault.md: the fleet
// simulator), served over HTTPS on loopback:
//
//   - vault-prod: Vault Enterprise, three healthy Raft servers, DR
//     replication streaming to its secondary, a license 300 days out, an
//     audit device, and a PKI mount (pki_int) with 400 services'
//     certificates (some ending soon) and 2,000 short-lived ones.
//   - vault-edge: OpenBao, three Raft servers with one failed (no failure
//     tolerance left), and an intermediate CA 12 days from its end.
//
// They listen on vaultPort and the port after it; -vault's directory
// keeps their certificate and key, so vault.yaml (written there) stays
// the same across runs.

const vaultPort = 58310

type fakeVault struct {
	name, version string
	enterprise    bool
	servers       []fakeRaft
	ft            int
	mount         string
	issuer        string // PEM
	issuerID      string
	certs         map[string]string // serial → PEM
	serials       []string
	revoked       []string
	leases        int
}

type fakeRaft struct{ id, status, node string }

func serveVaults(ctx context.Context, dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	cert, fp, err := vaultTLS(dir)
	if err != nil {
		log.Fatalf("fleet: vault: %v", err)
	}
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("fleet-vault-token\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	now := time.Now()
	vaults := []*fakeVault{
		newFakeVault("vault-prod", "1.21.4+ent", true, "pki_int", now, 300*24*time.Hour, 400, 2000,
			[]fakeRaft{{"v1", "leader", "alive"}, {"v2", "voter", "alive"}, {"v3", "voter", "alive"}}, 1, 18_000),
		newFakeVault("vault-edge", "2.4.1", false, "pki_edge", now, 12*24*time.Hour, 40, 0,
			[]fakeRaft{{"e1", "leader", "alive"}, {"e2", "voter", "alive"}, {"e3", "voter", "failed"}}, 0, 900),
	}
	var yaml strings.Builder
	yaml.WriteString("vault:\n")
	for i, v := range vaults {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", vaultPort+i))
		if err != nil {
			log.Fatal(err)
		}
		srv := &http.Server{Handler: v, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.ServeTLS(ln, "", "") }()
		go func() { <-ctx.Done(); _ = srv.Close() }()
		fmt.Fprintf(&yaml, "  - {name: %s, url: 'https://%s', token_file: %s, fingerprint: %s}\n", v.name, ln.Addr(), tok, fp)
		log.Printf("fleet: Vault %s (%s) at https://%s: %d certificates on %s", v.name, v.version, ln.Addr(), len(v.serials), v.mount)
	}
	p := filepath.Join(dir, "vault.yaml")
	if err := os.WriteFile(p, []byte(yaml.String()), 0o600); err != nil {
		log.Fatal(err)
	}
	log.Printf("fleet: %s has the vault: list for obs-beacond.yaml", p)
}

// vaultTLS is the fake Vaults' certificate, made once and kept in dir.
func vaultTLS(dir string) (tls.Certificate, string, error) {
	cp, kp := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if _, err := os.Stat(cp); errors.Is(err, os.ErrNotExist) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleet vault"}, DNSNames: []string{"localhost"},
			IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		kb, _ := x509.MarshalECPrivateKey(key)
		_ = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
		_ = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	}
	c, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		return c, "", err
	}
	sum := sha256.Sum256(c.Certificate[0])
	return c, hex.EncodeToString(sum[:]), nil
}

// newFakeVault makes a cluster with an intermediate CA ending caLeft from
// now, a year's certificates for that many services (most renewed once)
// and short ones lasting an hour.
func newFakeVault(name, version string, ent bool, mount string, now time.Time, caLeft time.Duration, services, short int,
	servers []fakeRaft, ft, leases int) *fakeVault {
	v := &fakeVault{name: name, version: version, enterprise: ent, servers: servers, ft: ft, mount: mount, certs: map[string]string{},
		issuerID: "4f6c1e2a-0000-4000-8000-" + fmt.Sprintf("%012d", len(name)), leases: leases}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	n := int64(1000)
	sign := func(tpl *x509.Certificate, parent *x509.Certificate, pub any, by *ecdsa.PrivateKey) (*x509.Certificate, string) {
		n++
		tpl.SerialNumber = big.NewInt(n)
		if parent == nil {
			parent = tpl
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, parent, pub, by)
		if err != nil {
			log.Fatal(err)
		}
		x, _ := x509.ParseCertificate(der)
		return x, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	ca, caPEM := sign(&x509.Certificate{Subject: pkix.Name{CommonName: name + " Intermediate CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-3 * 365 * 24 * time.Hour), NotAfter: now.Add(caLeft)}, nil, &caKey.PublicKey, caKey)
	v.issuer = caPEM
	add := func(x *x509.Certificate, p string) {
		s := serialHex(x)
		v.certs[s], v.serials = p, append(v.serials, s)
	}
	add(ca, caPEM)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	day := 24 * time.Hour
	for i := range services {
		cn := fmt.Sprintf("svc-%03d.%s.internal", i, strings.TrimPrefix(name, "vault-"))
		// Most a long way from their end; a few within the warning's 21
		// days, fewer within the critical 7.
		left := time.Duration(25+(i*37)%340) * day
		switch {
		case i%97 == 5:
			left = 5 * day
		case i%41 == 7:
			left = 15 * day
		}
		for k, end := range []time.Duration{left - 30*day, left} { // the one it replaced, then the renewal
			x, p := sign(&x509.Certificate{Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn}, NotBefore: now.Add(end - 365*day), NotAfter: now.Add(end)},
				ca, &leafKey.PublicKey, caKey)
			if k == 0 && end < -8*day {
				continue // tidied away
			}
			add(x, p)
		}
	}
	for i := range short {
		cn := fmt.Sprintf("job-%04d.batch.internal", i)
		start := now.Add(-time.Duration(i%60) * time.Minute)
		x, p := sign(&x509.Certificate{Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn}, NotBefore: start, NotAfter: start.Add(time.Hour)}, ca, &leafKey.PublicKey, caKey)
		add(x, p)
	}
	if len(v.serials) > 5 {
		v.revoked = []string{v.serials[3]}
	}
	return v
}

func serialHex(x *x509.Certificate) string {
	b := x.SerialNumber.Bytes()
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, ":")
}

func (v *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	list := r.Method == "LIST" || r.URL.Query().Get("list") == "true"
	now := time.Now()
	var out any
	data := func(d any) any { return map[string]any{"data": d} }
	switch {
	case p == "sys/health":
		mode := "disabled"
		if v.enterprise {
			mode = "primary"
		}
		out = map[string]any{"initialized": true, "sealed": false, "standby": false, "version": v.version, "cluster_name": v.name,
			"enterprise": v.enterprise, "replication_dr_mode": mode, "replication_performance_mode": "disabled"}
	case p == "sys/leader":
		out = map[string]any{"ha_enabled": true, "is_self": true, "leader_address": "https://" + v.servers[0].id + "." + v.name + ":8200"}
	case p == "sys/storage/raft/autopilot/state":
		servers, voters, healthy := map[string]any{}, []string{}, true
		for _, s := range v.servers {
			ok := s.node == "alive"
			healthy = healthy && ok
			sv := map[string]any{"id": s.id, "name": v.name + "-" + s.id, "address": s.id + ":8201", "node_status": s.node, "healthy": ok,
				"status": s.status, "last_contact": "0s", "version": strings.TrimSuffix(v.version, "+ent"), "stable_since": now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)}
			if !ok {
				sv["last_contact"] = "41m12s"
			}
			servers[s.id] = sv
			voters = append(voters, s.id)
		}
		out = data(map[string]any{"healthy": healthy, "failure_tolerance": v.ft, "voters": voters, "servers": servers})
	case p == "auth/token/lookup-self":
		out = data(map[string]any{"display_name": "token-obs-beacond", "policies": []string{"default", "obs-monitor"}, "ttl": 20 * 3600,
			"creation_ttl": 24 * 3600, "renewable": true, "type": "service"})
	case p == "auth/token/renew-self":
		out = map[string]any{"auth": map[string]any{"lease_duration": 24 * 3600}}
	case p == "sys/metrics":
		l := v.leases + int(float64(v.leases)/10*math.Sin(float64(now.Unix())/3600))
		fmt.Fprintf(w, "# TYPE vault_expire_num_leases gauge\nvault_expire_num_leases %d\nvault_expire_num_irrevocable_leases 0\n", l)
		if v.enterprise {
			fmt.Fprint(w, "vault_audit_log_request_failure 0\nvault_audit_log_response_failure 0\n")
		}
		return
	case p == "sys/mounts":
		out = data(map[string]any{v.mount + "/": map[string]any{"type": "pki"}, "secret/": map[string]any{"type": "kv"}})
	case p == "sys/replication/status" && v.enterprise:
		out = data(map[string]any{"dr": map[string]any{"mode": "primary", "state": "running", "known_secondaries": []string{"dr-west"}, "last_wal": 912_334,
			"secondaries": []any{map[string]any{"node_id": "dr-west", "api_address": "https://vault-dr.west:8200", "connection_state": "ready",
				"last_heartbeat": now.Add(-4 * time.Second).Format(time.RFC3339)}}}, "performance": map[string]any{"mode": "disabled"}})
	case p == "sys/license/status" && v.enterprise:
		out = data(map[string]any{"autoloading_used": true, "autoloaded": map[string]any{"license_id": "fleet-0000",
			"expiration_time": now.Add(300 * 24 * time.Hour).Format(time.RFC3339), "termination_time": now.Add(310 * 24 * time.Hour).Format(time.RFC3339)}})
	case p == v.mount+"/issuers" && list:
		out = data(map[string]any{"keys": []string{v.issuerID}})
	case p == v.mount+"/issuer/"+v.issuerID:
		out = data(map[string]any{"certificate": v.issuer, "issuer_name": "intermediate-2023"})
	case p == v.mount+"/certs" && list:
		out = data(map[string]any{"keys": v.serials})
	case p == v.mount+"/certs/revoked" && list:
		out = data(map[string]any{"keys": v.revoked})
	case strings.HasPrefix(p, v.mount+"/cert/"):
		c, ok := v.certs[strings.TrimPrefix(p, v.mount+"/cert/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			out = map[string]any{"errors": []string{}}
			break
		}
		out = data(map[string]any{"certificate": c})
	default:
		w.WriteHeader(http.StatusNotFound)
		out = map[string]any{"errors": []string{}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
