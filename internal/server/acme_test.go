package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestACMEConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "obs-beacond.yaml")
	load := func(s string) (*Config, error) {
		_ = os.WriteFile(p, []byte(s), 0o600)
		return LoadConfig(p)
	}
	base := "listen: \":443\"\ndatabase: " + dir + "/beacon.db\nstate_key_file: " + dir + "/k\n"
	for body, want := range map[string]string{
		"tls:\n  acme:\n    accept_terms: true\n":                                                        "hostnames is required",
		"tls:\n  acme:\n    hostnames: [obs.example.org]\n":                                              "accept_terms",
		"tls:\n  acme:\n    hostnames: [192.0.2.1]\n    accept_terms: true\n":                            "not a host name",
		"tls:\n  acme:\n    hostnames: [localhost]\n    accept_terms: true\n":                            "not a host name",
		"tls:\n  cert: a\n  key: b\n  acme:\n    hostnames: [obs.example.org]\n    accept_terms: true\n": "either-or",
	} {
		if _, err := load(base + body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
	c, err := load(base + "tls:\n  acme:\n    hostnames: [OBS.Example.org.]\n    accept_terms: true\n")
	if err != nil {
		t.Fatal(err)
	}
	a := c.TLS.ACME
	if a.Hostnames[0] != "obs.example.org" || a.Directory != letsEncrypt || a.Cache != filepath.Join(dir, "acme") {
		t.Fatalf("defaults: %+v", a)
	}
	if c.Alerts.PublicURL != "https://obs.example.org" {
		t.Fatalf("public url %q", c.Alerts.PublicURL)
	}
	c, _ = load(strings.Replace(base, ":443", ":8443", 1) + "alerts:\n  public_url: \"\"\ntls:\n  acme:\n    hostnames: [obs.example.org]\n    accept_terms: true\n")
	if c.Alerts.PublicURL != "https://obs.example.org:8443" {
		t.Fatalf("public url with a port: %q", c.Alerts.PublicURL)
	}
	c, _ = load(base + "alerts:\n  public_url: https://monitor.example.org/\ntls:\n  acme:\n    hostnames: [obs.example.org]\n    accept_terms: true\n")
	if c.Alerts.PublicURL != "https://monitor.example.org" {
		t.Fatalf("an explicit public url was replaced: %q", c.Alerts.PublicURL)
	}
}
