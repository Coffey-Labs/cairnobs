// Package certreload reloads a service that's serving an old certificate
// after its file was renewed (docs/cert-rotation.md, step 2). The root
// helper runs it with the rules it reads from agent.yaml itself, not the
// agent's word: a unit is reloaded only when it's listed, one of its
// files holds a certificate for the same names that ends later than the
// one served, hasn't ended, and matches its key, and systemd says the
// unit can reload. Keys are read only to compare; nothing leaves.
package certreload

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/certfiles"
	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
	"gopkg.in/yaml.v3"
)

// Request is what the agent asks for: the unit, and the certificate it
// found the unit's port serving.
type Request struct {
	Unit   string    `json:"unit"`
	Names  []string  `json:"names"`  // the served certificate's
	Served string    `json:"served"` // its fingerprint
	Ends   time.Time `json:"ends"`   // its end
}

// Result is the renewed certificate the unit was reloaded for.
type Result struct {
	File        string    `json:"file"`
	Fingerprint string    `json:"fingerprint"`
	Ends        time.Time `json:"ends"`
}

// System is systemd, or the tests' stand-in.
type System interface {
	CanReload(ctx context.Context, unit string) (bool, error)
	Reload(ctx context.Context, unit string) error
}

// ReadRules reads cert_reload from an agent.yaml; none when it isn't
// there.
func ReadRules(path string) ([]report.ReloadRule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c struct {
		CertReload []report.ReloadRule `yaml:"cert_reload"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := Validate(c.CertReload); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c.CertReload, nil
}

// Validate checks cert_reload: plain unit names, each once, each with
// absolute file globs.
func Validate(rules []report.ReloadRule) error {
	seen := map[string]bool{}
	for _, r := range rules {
		if err := validUnit(r.Unit); err != nil {
			return fmt.Errorf("cert_reload: %w", err)
		}
		if seen[r.Unit] {
			return fmt.Errorf("cert_reload: %s is listed twice", r.Unit)
		}
		seen[r.Unit] = true
		if len(r.Files) == 0 {
			return fmt.Errorf("cert_reload: %s needs files: the certificate files it serves", r.Unit)
		}
		for _, f := range r.Files {
			if !filepath.IsAbs(f) {
				return fmt.Errorf("cert_reload: %s: %q isn't an absolute path", r.Unit, f)
			}
		}
	}
	return nil
}

// Rule finds the unit's rule.
func Rule(rules []report.ReloadRule, unit string) (report.ReloadRule, bool) {
	i := slices.IndexFunc(rules, func(r report.ReloadRule) bool { return r.Unit == unit })
	if i < 0 {
		return report.ReloadRule{}, false
	}
	return rules[i], true
}

// Do checks the request against the rules and the files, and reloads.
func Do(ctx context.Context, rules []report.ReloadRule, req Request, sys System, now time.Time) (Result, error) {
	res, err := Check(rules, req, now)
	if err != nil {
		return res, err
	}
	can, err := sys.CanReload(ctx, req.Unit)
	switch {
	case err != nil:
		return res, fmt.Errorf("can't ask systemd about %s: %w", req.Unit, err)
	case !can:
		return res, fmt.Errorf("%s can't reload (it has no ExecReload); it isn't restarted", req.Unit)
	}
	if err := sys.Reload(ctx, req.Unit); err != nil {
		return res, fmt.Errorf("systemctl reload %s: %w", req.Unit, err)
	}
	return res, nil
}

// Check finds the renewed certificate the reload would be for, or says why
// there's none.
func Check(rules []report.ReloadRule, req Request, now time.Time) (Result, error) {
	rule, ok := Rule(rules, req.Unit)
	if !ok {
		return Result{}, fmt.Errorf("%s isn't in this host's cert_reload", req.Unit)
	}
	want := namesKey(req.Names)
	var best *candidate
	for _, f := range certfiles.Find(rule.Files) {
		c := read(f)
		if c == nil || namesKey(certinfo.Names(c.cert)) != want || c.fp == req.Served ||
			!c.cert.NotAfter.After(req.Ends) || !c.cert.NotAfter.After(now) || c.cert.NotBefore.After(now) {
			continue
		}
		if best == nil || c.cert.NotAfter.After(best.cert.NotAfter) {
			best = c
		}
	}
	if best == nil {
		return Result{}, fmt.Errorf("no certificate for %s ending after the served one in %s's files (%s)", strings.Join(req.Names, ", "), req.Unit, strings.Join(rule.Files, ", "))
	}
	res := Result{File: best.path, Fingerprint: best.fp, Ends: best.cert.NotAfter}
	key, from := findKey(best.path, best.data)
	switch {
	case key == nil:
		return res, fmt.Errorf("no private key found for %s (in it, privkey.pem beside it, or the same name with .key): not reloading", best.path)
	case !samePublic(key.Public(), best.cert.PublicKey):
		return res, fmt.Errorf("%s doesn't match its key %s: not reloading", best.path, from)
	}
	return res, nil
}

type candidate struct {
	path string
	data []byte
	cert *x509.Certificate
	fp   string
}

// read parses the first certificate in a file that isn't a CA's.
func read(path string) *candidate {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 1<<20 {
		return nil
	}
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return nil
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		x, err := x509.ParseCertificate(blk.Bytes)
		if err != nil || x.IsCA {
			continue
		}
		return &candidate{path: path, data: b, cert: x, fp: certinfo.Of(x).Fingerprint}
	}
}

// findKey looks for the certificate's key: in its own file (haproxy's),
// privkey.pem beside it (Let's Encrypt's), or the same name ending .key.
func findKey(path string, data []byte) (crypto.Signer, string) {
	if k := parseKey(data); k != nil {
		return k, path
	}
	for _, p := range []string{filepath.Join(filepath.Dir(path), "privkey.pem"), strings.TrimSuffix(path, filepath.Ext(path)) + ".key"} {
		if b, err := os.ReadFile(p); err == nil {
			if k := parseKey(b); k != nil {
				return k, p
			}
		}
	}
	return nil, ""
}

func parseKey(b []byte) crypto.Signer {
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return nil
		}
		if !strings.HasSuffix(blk.Type, "PRIVATE KEY") {
			continue
		}
		var k any
		var err error
		switch blk.Type {
		case "RSA PRIVATE KEY":
			k, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
		case "EC PRIVATE KEY":
			k, err = x509.ParseECPrivateKey(blk.Bytes)
		default:
			k, err = x509.ParsePKCS8PrivateKey(blk.Bytes)
		}
		if s, ok := k.(crypto.Signer); err == nil && ok {
			return s
		}
	}
}

func samePublic(a, b crypto.PublicKey) bool {
	e, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && e.Equal(b)
}

func namesKey(names []string) string {
	n := slices.Clone(names)
	for i := range n {
		n[i] = strings.ToLower(n[i])
	}
	slices.Sort(n)
	return strings.Join(slices.Compact(n), ",")
}

// Systemd reloads with systemctl.
type Systemd struct{}

func (Systemd) CanReload(ctx context.Context, unit string) (bool, error) {
	if err := validUnit(unit); err != nil {
		return false, err
	}
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=CanReload", "--value", "--", unit).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

func (Systemd) Reload(ctx context.Context, unit string) error {
	if err := validUnit(unit); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "systemctl", "reload", "--", unit).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
	}
	return err
}

// validUnit refuses anything but a plain unit name.
func validUnit(u string) error {
	if u == "" || strings.HasPrefix(u, "-") || strings.ContainsAny(u, " /\\\t\n") {
		return fmt.Errorf("%q isn't a unit name", u)
	}
	return nil
}
