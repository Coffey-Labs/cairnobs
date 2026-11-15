package server

import (
	"strings"
	"testing"

	clusteragent "github.com/cairnobs/cairnobs/deploy/kubernetes/cluster-agent"
	"github.com/cairnobs/cairnobs/internal/version"
)

// Without join.image clusters pull the public image at this version, or
// :latest from a development build; a set join.image gets this version
// unless it names a tag or digest itself.
func TestClusterImage(t *testing.T) {
	was := version.Version
	t.Cleanup(func() { version.Version = was })
	for _, c := range []struct{ image, version, want string }{
		{"", "0.1.0", "ghcr.io/coffey-labs/cairnobs:0.1.0"},
		{"", "v0.2.0-rc.3", "ghcr.io/coffey-labs/cairnobs:0.2.0-rc.3"},
		{"", "dev", "ghcr.io/coffey-labs/cairnobs:latest"},
		{"", "v0.1.0-3-gabc1234-dirty", "ghcr.io/coffey-labs/cairnobs:latest"},
		{"registry.example.org:5000/ops/obs-beacond", "0.1.0", "registry.example.org:5000/ops/obs-beacond:0.1.0"},
		{"registry.example.org/ops/obs-beacond", "dev", "registry.example.org/ops/obs-beacond:dev"},
		{"registry.example.org/ops/obs-beacond:pinned", "0.1.0", "registry.example.org/ops/obs-beacond:pinned"},
		{"registry.example.org/ops/obs-beacond@sha256:abc", "0.1.0", "registry.example.org/ops/obs-beacond@sha256:abc"},
	} {
		version.Version = c.version
		srv := &Server{cfg: &Config{Join: JoinConfig{Image: c.image}}}
		if got := srv.clusterImage(); got != c.want {
			t.Errorf("join.image %q at %s: %s, want %s", c.image, c.version, got, c.want)
		}
	}
}

// /cluster.yaml?cert_renew=1 is the same manifest with -cert-renew and
// cert-renew.yaml's role; without it, neither.
func TestClusterManifestCertRenew(t *testing.T) {
	if n := strings.Count(clusteragent.Manifest, clusteragent.Command); n != 1 {
		t.Fatalf("the agent's command line is in cluster-agent.yaml %d times", n)
	}
	plain := clusterManifest("img:1", "", false)
	if strings.Contains(plain, "-cert-renew") || strings.Contains(plain, "certificates/status") || strings.Contains(plain, "@IMAGE@") {
		t.Errorf("plain manifest:\n%s", plain)
	}
	m := clusterManifest("img:1", "", true)
	if !strings.Contains(m, clusteragent.CertRenewCommand) || strings.Contains(m, clusteragent.Command+"\n") ||
		!strings.Contains(m, "resources: [certificates/status]\n    verbs: [patch]") || !strings.HasPrefix(m, plain[:200]) {
		t.Errorf("cert_renew manifest:\n%s", m)
	}
	// Only patch on the status: nothing else is added.
	for _, verb := range []string{"create", "update", "delete", "secrets"} {
		if strings.Contains(clusteragent.CertRenew, verb) {
			t.Errorf("cert-renew.yaml mentions %s", verb)
		}
	}
}
