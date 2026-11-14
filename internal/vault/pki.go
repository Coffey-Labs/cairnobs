package vault

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/certinfo"
)

// PKI: every PKI mount's issuers (its CAs) and every leaf certificate it
// issued (docs/vault.md, decision 3). A certificate's end never changes,
// so each is fetched once, the first time its serial is listed, at most
// fetchBudget a read; the cache lives as long as the client does.

// fetchBudget is how many certificates one read fetches, across mounts:
// 200,000 are read in under two hours. A variable for the tests.
var fetchBudget = 2000

const (
	// fetchers is how many are fetched at once.
	fetchers = 8
	// fetchFor is the longest a read spends fetching, so it ends well
	// inside its own time.
	fetchFor = 30 * time.Second
)

// Cert is what's kept of one certificate.
type Cert struct {
	Serial    string
	Subject   string // its common name, or first alternative name
	Names     string // what it's for: common name and alternative names, sorted, comma-separated
	Issuer    string // the issuer's common name
	NotBefore time.Time
	NotAfter  time.Time
	CA        bool
	// For the inventory (docs/cert-inventory.md): the SHA-256 fingerprint,
	// the key ("ECDSA P-256") and signature algorithm.
	Fingerprint, Key, Sig string
	Revoked               bool // said by the certificate's own read (Vault before 1.12 doesn't list revoked serials)
}

// Lifetime is how long it was made to last.
func (c Cert) Lifetime() time.Duration { return c.NotAfter.Sub(c.NotBefore) }

// Issuer is one of a mount's CAs.
type Issuer struct {
	ID   string
	Name string // issuer_name, or its id
	Cert Cert
}

// Mount is one PKI mount's read.
type Mount struct {
	Path     string // "pki_int" (no slash)
	Issuers  []Issuer
	Certs    []Cert // leaf certificates read so far, not revoked
	Listed   int    // serials listed, issuers' included
	Revoked  int
	Pending  int    // listed, not fetched yet
	Err      string // what couldn't be read
	CertsErr string // the certificates couldn't be listed
}

// certCache is what's been fetched, by mount and serial.
type certCache map[string]map[string]Cert

func parsePEM(s string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

func certOf(serial string, x *x509.Certificate) Cert {
	names := append([]string{}, x.DNSNames...)
	for _, ip := range x.IPAddresses {
		names = append(names, ip.String())
	}
	for _, u := range x.URIs {
		names = append(names, u.String())
	}
	names = append(names, x.EmailAddresses...)
	subject := x.Subject.CommonName
	if subject == "" && len(names) > 0 {
		subject = names[0]
	}
	if subject == "" {
		subject = serial
	}
	all := names
	if x.Subject.CommonName != "" {
		all = append(all, x.Subject.CommonName)
	}
	sort.Strings(all)
	all = compact(all)
	d := certinfo.Of(x)
	return Cert{Serial: serial, Subject: subject, Names: strings.Join(all, ","), Issuer: x.Issuer.CommonName,
		NotBefore: x.NotBefore.UTC(), NotAfter: x.NotAfter.UTC(), CA: x.IsCA, Fingerprint: d.Fingerprint, Key: d.Key, Sig: d.Sig}
}

func compact(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// serialOf is a serial as Vault lists it: hex pairs and colons.
func serialOf(x *x509.Certificate) string {
	b := x.SerialNumber.Bytes()
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02x", v)
	}
	return strings.Join(parts, ":")
}

// pkiMounts are the mounts of type pki, by path without the slash.
func (c *Client) pkiMounts(ctx context.Context) ([]string, error) {
	var raw json.RawMessage
	var se *StatusError
	if err := c.get(ctx, "sys/mounts", &raw); errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, nil // not Vault's answer: an older OpenBao, or a fake
	} else if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := unwrap(raw, &m); err != nil {
		return nil, err
	}
	var out []string
	for path, v := range m {
		var mt struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(v, &mt) == nil && mt.Type == "pki" {
			out = append(out, strings.TrimSuffix(path, "/"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// readPKI reads every PKI mount, fetching what the cache lacks within
// fetchBudget and fetchFor.
func (c *Client) readPKI(ctx context.Context, s *Snapshot) {
	mounts, err := c.pkiMounts(ctx)
	if err != nil {
		s.Problems["its PKI mounts"] = err.Error()
		return
	}
	if c.certs == nil {
		c.certs = certCache{}
	}
	for k := range c.certs {
		if !contains(mounts, k) {
			delete(c.certs, k) // the mount is gone
		}
	}
	f := &fetch{left: fetchBudget, until: time.Now().Add(fetchFor)}
	if d, ok := ctx.Deadline(); ok && d.Add(-5*time.Second).Before(f.until) {
		f.until = d.Add(-5 * time.Second)
	}
	for _, path := range mounts {
		m := Mount{Path: path}
		c.readIssuers(ctx, &m)
		c.readCerts(ctx, &m, f)
		s.PKI = append(s.PKI, m)
	}
}

// fetch is one read's allowance for fetching certificates.
type fetch struct {
	left  int
	until time.Time
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// readIssuers reads a mount's CAs: its issuers (Vault 1.11 and later,
// OpenBao), or its one CA before that.
func (c *Client) readIssuers(ctx context.Context, m *Mount) {
	ids, err := c.list(ctx, m.Path+"/issuers")
	if err != nil {
		m.Err = "its issuers: " + err.Error()
		return
	}
	if len(ids) == 0 {
		if p, err := c.getText(ctx, m.Path+"/ca/pem"); err == nil && p != "" {
			if x, err := parsePEM(p); err == nil {
				m.Issuers = append(m.Issuers, Issuer{ID: "default", Name: or(x.Subject.CommonName, "default"), Cert: certOf(serialOf(x), x)})
			}
		}
		return
	}
	for _, id := range ids {
		var raw json.RawMessage
		if err := c.get(ctx, m.Path+"/issuer/"+id, &raw); err != nil {
			m.Err = "issuer " + id + ": " + err.Error()
			continue
		}
		var is struct {
			Certificate string `json:"certificate"`
			IssuerName  string `json:"issuer_name"`
		}
		if err := unwrap(raw, &is); err != nil {
			m.Err = "issuer " + id + ": " + err.Error()
			continue
		}
		x, err := parsePEM(is.Certificate)
		if err != nil {
			m.Err = "issuer " + id + ": " + err.Error()
			continue
		}
		m.Issuers = append(m.Issuers, Issuer{ID: id, Name: or(is.IssuerName, id), Cert: certOf(serialOf(x), x)})
	}
	sort.Slice(m.Issuers, func(i, j int) bool { return m.Issuers[i].Name < m.Issuers[j].Name })
}

// readCerts lists a mount's serials and revoked serials, and fetches the
// ones not yet cached, within f.
func (c *Client) readCerts(ctx context.Context, m *Mount, f *fetch) {
	serials, err := c.list(ctx, m.Path+"/certs")
	if err != nil {
		m.CertsErr = err.Error()
		return
	}
	m.Listed = len(serials)
	revoked := map[string]bool{}
	if rs, err := c.list(ctx, m.Path+"/certs/revoked"); err == nil {
		for _, r := range rs {
			revoked[r] = true
		}
	}
	ca := map[string]bool{}
	for _, is := range m.Issuers {
		ca[is.Cert.Serial] = true
	}
	cache := c.certs[m.Path]
	if cache == nil {
		cache = map[string]Cert{}
		c.certs[m.Path] = cache
	}
	listed := make(map[string]bool, len(serials))
	var need []string
	for _, sn := range serials {
		listed[sn] = true
		if _, ok := cache[sn]; !ok && !ca[sn] && !revoked[sn] {
			need = append(need, sn)
		}
	}
	for sn := range cache {
		if !listed[sn] {
			delete(cache, sn) // tidied away
		}
	}
	c.fetchCerts(ctx, m, need, f, cache)
	for _, sn := range serials {
		ct, ok := cache[sn]
		switch {
		case ca[sn]:
		case revoked[sn] || ok && ct.Revoked:
			m.Revoked++
		case !ok:
			m.Pending++ // not fetched yet, or its fetch failed: next time
		case ct.CA:
			// An intermediate this mount signed: its own mount shows it.
		default:
			m.Certs = append(m.Certs, ct)
		}
	}
}

// fetchCerts fetches serials into cache, several at once, while f allows.
// A failure is said once on the mount and tried again next read.
func (c *Client) fetchCerts(ctx context.Context, m *Mount, serials []string, f *fetch, cache map[string]Cert) {
	if len(serials) > f.left {
		serials = serials[:f.left]
	}
	if len(serials) == 0 || !time.Now().Before(f.until) {
		return
	}
	fctx, cancel := context.WithDeadline(ctx, f.until)
	defer cancel()
	work := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(fetchers, len(serials)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sn := range work {
				ct, err := c.fetchCert(fctx, m.Path, sn)
				mu.Lock()
				switch {
				case err == nil:
					cache[sn] = ct
				case fctx.Err() == nil && m.Err == "":
					m.Err = "certificate " + sn + ": " + err.Error()
				}
				mu.Unlock()
			}
		}()
	}
	for _, sn := range serials {
		if fctx.Err() != nil {
			break
		}
		work <- sn
	}
	close(work)
	wg.Wait()
	f.left -= len(serials)
}

// fetchCert reads one certificate.
func (c *Client) fetchCert(ctx context.Context, mount, serial string) (Cert, error) {
	var raw json.RawMessage
	if err := c.get(ctx, mount+"/cert/"+serial, &raw); err != nil {
		return Cert{}, err
	}
	var r struct {
		Certificate    string `json:"certificate"`
		RevocationTime int64  `json:"revocation_time"`
	}
	if err := unwrap(raw, &r); err != nil {
		return Cert{}, err
	}
	x, err := parsePEM(r.Certificate)
	if err != nil {
		return Cert{}, err
	}
	ct := certOf(serial, x)
	ct.Revoked = r.RevocationTime > 0
	return ct, nil
}

// getText reads a path that answers with text, not JSON (ca/pem).
func (c *Client) getText(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/"+path, nil)
	if err != nil {
		return "", err
	}
	t, err := c.readToken()
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Vault-Token", t)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{Code: resp.StatusCode, Msg: apiError(resp)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return string(b), err
}

// Subject is the certificates for one set of names: one service's,
// renewed. Newest is the one that counts.
type Subject struct {
	Names  string
	Newest Cert
	Count  int
}

// Subjects groups a mount's certificates by what they're for. Short-lived
// ones (meant to last under minLife) aren't returned: they're counted,
// not judged; short is how many there were.
func (m Mount) Subjects(minLife time.Duration) (subs []Subject, short int) {
	by := map[string]*Subject{}
	for _, ct := range m.Certs {
		if ct.Lifetime() < minLife {
			short++
			continue
		}
		s := by[ct.Names]
		if s == nil {
			s = &Subject{Names: ct.Names, Newest: ct}
			by[ct.Names] = s
		}
		s.Count++
		if ct.NotAfter.After(s.Newest.NotAfter) {
			s.Newest = ct
		}
	}
	for _, s := range by {
		subs = append(subs, *s)
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].Names < subs[j].Names })
	return subs, short
}

// subjectID is a subject's record id: short, stable, from its names.
func subjectID(mount, names string) string {
	h := sha256.Sum256([]byte(mount + "\x00" + names))
	return "cert/" + hex.EncodeToString(h[:10])
}
