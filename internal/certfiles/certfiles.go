// Package certfiles finds the certificates in a host's files
// (docs/cert-inventory.md, step 3). The root helper reads the files the
// agent's globs match and gives back only the certificates parsed out of
// them: a file holding a key and a certificate (haproxy's) gives back the
// certificate alone, and the key's bytes never leave the helper. A file
// whose certificates are all CAs' (a bundle) gives back nothing.
package certfiles

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
)

// Defaults are the globs read when agent.yaml names none.
var Defaults = []string{
	"/etc/letsencrypt/live/*/cert.pem",
	"/etc/pki/tls/certs/*.crt",
	"/etc/nginx/**/*.{pem,crt}",
	"/etc/haproxy/**/*.pem",
	"/etc/postfix/**/*.pem",
	"/etc/dovecot/**/*.pem",
}

const (
	maxFiles = 5000    // files looked at for one read, all globs together
	maxSize  = 1 << 20 // a bigger file isn't a certificate
	maxDepth = 8       // directories below a ** pattern's root
	// AskEvery is how often the agent asks the helper; a file is parsed
	// again only when its time or size changed.
	AskEvery = 10 * time.Minute
	// resend is how often the list goes to obs-beacond when nothing
	// changed.
	resend = 30 * time.Minute
)

// skipped are directories never read: the system's CA bundles.
var skipped = []string{"/etc/ssl/certs/", "/etc/pki/ca-trust/", "/usr/share/ca-certificates/"}

// Expand turns a glob's braces into the globs they stand for:
// "/a/*.{pem,crt}" is "/a/*.pem" and "/a/*.crt".
func Expand(glob string) []string {
	i := strings.IndexByte(glob, '{')
	j := strings.IndexByte(glob[max(i, 0):], '}') + max(i, 0)
	if i < 0 || j <= i {
		return []string{glob}
	}
	var out []string
	for _, alt := range strings.Split(glob[i+1:j], ",") {
		out = append(out, Expand(glob[:i]+alt+glob[j+1:])...)
	}
	return out
}

// Match says whether a path is one Find would list for the globs (without
// looking at the disk): braces, and "**" for up to maxDepth directories.
func Match(globs []string, path string) bool {
	path = filepath.Clean(path)
	for _, g := range globs {
		for _, p := range Expand(g) {
			if !filepath.IsAbs(p) {
				continue
			}
			i := strings.Index(p, "**")
			if i < 0 {
				if ok, _ := filepath.Match(p, path); ok {
					return true
				}
				continue
			}
			root, name := filepath.Clean(p[:i]), strings.TrimPrefix(p[i+2:], "/")
			if strings.ContainsRune(name, '/') || name == "" || !strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
				continue
			}
			dirs := strings.Count(path[len(strings.TrimSuffix(root, "/")):], "/") - 1
			if ok, _ := filepath.Match(name, filepath.Base(path)); ok && dirs <= maxDepth {
				return true
			}
		}
	}
	return false
}

// Find lists the regular files the globs match, sorted, at most maxFiles.
// "**" stands for any number of directories (a pattern after it names
// files only: "/etc/nginx/**/*.pem").
func Find(globs []string) []string {
	seen := map[string]bool{}
	var out []string
	keep := func(p string) bool {
		p = filepath.Clean(p)
		if len(out) >= maxFiles {
			return false
		}
		if seen[p] || isSkipped(p) {
			return true
		}
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() || st.Size() > maxSize {
			return true
		}
		seen[p] = true
		out = append(out, p)
		return true
	}
	for _, g := range globs {
		for _, p := range Expand(g) {
			if !filepath.IsAbs(p) {
				continue
			}
			i := strings.Index(p, "**")
			if i < 0 {
				m, _ := filepath.Glob(p)
				for _, f := range m {
					keep(f)
				}
				continue
			}
			root, name := filepath.Clean(p[:i]), strings.TrimPrefix(p[i+2:], "/")
			if strings.ContainsRune(name, '/') || name == "" {
				continue // only a file name after **
			}
			depth := strings.Count(root, string(filepath.Separator))
			_ = filepath.WalkDir(root, func(f string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if strings.Count(f, string(filepath.Separator))-depth >= maxDepth || isSkipped(f+"/") {
						return filepath.SkipDir
					}
					return nil
				}
				if ok, _ := filepath.Match(name, d.Name()); ok && !keep(f) {
					return filepath.SkipAll
				}
				return nil
			})
		}
	}
	slices.Sort(out)
	return out
}

func isSkipped(p string) bool {
	for _, s := range skipped {
		if strings.HasPrefix(p, s) {
			return true
		}
	}
	return false
}

// Parse is the certificate a file holds: the first one that isn't a CA's,
// and how many more certificates the file has. A self-signed one is often
// marked a CA (openssl's, the snakeoil): with no other, a file's only
// certificate counts when it has names or a key sits next to it. False
// when it holds none (keys, bundles and everything else).
func Parse(data []byte) (*report.Cert, int, bool) {
	var certs []*x509.Certificate
	key := false
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" && b.Type != "TRUSTED CERTIFICATE" {
			key = key || strings.HasSuffix(b.Type, "PRIVATE KEY")
			continue // a key's bytes go no further than this
		}
		if x, err := x509.ParseCertificate(b.Bytes); err == nil {
			certs = append(certs, x)
		}
	}
	if len(certs) == 0 && !bytes.Contains(data, []byte("-----BEGIN")) {
		if x, err := x509.ParseCertificate(data); err == nil { // DER
			certs = append(certs, x)
		}
	}
	for _, x := range certs {
		if !x.IsCA {
			return certinfo.Of(x), len(certs) - 1, true
		}
	}
	if len(certs) == 1 && (key || len(certinfo.Names(certs[0])) > 0) {
		return certinfo.Of(certs[0]), 0, true
	}
	return nil, 0, false
}

// Reader reads files for the helper, parsing each again only when its
// time or size changed.
type Reader struct {
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	mod  time.Time
	size int64
	file *report.CertFile // nil: no certificate in it
}

// Read is the certificates the globs' files hold.
func (r *Reader) Read(globs []string) []report.CertFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]cached{}
	}
	now := map[string]cached{}
	out := []report.CertFile{}
	for _, p := range Find(globs) {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		c, ok := r.cache[p]
		if !ok || !c.mod.Equal(st.ModTime()) || c.size != st.Size() {
			c = cached{mod: st.ModTime(), size: st.Size()}
			if data, err := os.ReadFile(p); err == nil {
				if cert, chain, ok := Parse(data); ok {
					c.file = &report.CertFile{Path: p, Mod: st.ModTime().UTC().Truncate(time.Second), Cert: *cert, Chain: chain}
				}
				clear(data)
			}
		}
		now[p] = c
		if c.file != nil {
			out = append(out, *c.file)
		}
	}
	r.cache = now
	return out
}

// Watch is the agent's side: the last list the helper gave, and when it
// went to obs-beacond.
type Watch struct {
	mu   sync.Mutex
	list []report.CertFile
	have bool
	sent time.Time
	last string
}

// Set keeps the helper's latest list.
func (w *Watch) Set(list []report.CertFile) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list, w.have = list, true
}

// Report is the list to send: when it changed, or every resend; nil
// otherwise (obs-beacond keeps the last), and nil before the helper first
// answered.
func (w *Watch) Report(now time.Time) *report.CertFiles {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.have {
		return nil
	}
	var key strings.Builder
	for _, f := range w.list {
		fmt.Fprintf(&key, "%s %s|", f.Path, f.Cert.Fingerprint)
	}
	if key.String() == w.last && now.Sub(w.sent) < resend {
		return nil
	}
	w.last, w.sent = key.String(), now
	return &report.CertFiles{At: now.UTC(), List: append([]report.CertFile{}, w.list...)}
}
