package server

import (
	"strings"
	"testing"

	clusteragent "github.com/cairnobs/cairnobs/deploy/kubernetes/cluster-agent"
)

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
