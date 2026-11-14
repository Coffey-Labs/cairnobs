package server

// Adding a cluster (docs/cluster-watcher.md, Joining): an admin makes a
// join token for a cluster on Configure → Clusters and runs one command
// against the cluster. It fetches /cluster.yaml with the token in a header,
// and gets the cluster agent's manifest with the token in its Secret. The
// agent joins as a host's agent does, and keeps its certificate in that
// Secret.

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	clusteragent "github.com/cairnobs/cairnobs/deploy/kubernetes/cluster-agent"
	"github.com/cairnobs/cairnobs/internal/jointoken"
	"github.com/cairnobs/cairnobs/internal/version"
)

// clusterImage is join.image, with this obs-beacond's version as the tag
// when it has none.
func (srv *Server) clusterImage() string {
	img := srv.cfg.Join.Image
	last := img[strings.LastIndex(img, "/")+1:]
	if strings.Contains(last, ":") || strings.Contains(last, "@") {
		return img
	}
	return img + ":" + strings.TrimPrefix(version.Version, "v")
}

// handleClusterManifest serves the cluster agent's manifest. With a
// pending cluster join token (Authorization: Bearer), it adds the Secret
// holding it; without one, it's the manifest alone, for upgrading or
// deleting the agent. The token isn't spent here: the agent spends it.
// ?cert_renew=1 adds -cert-renew and its role (cert-renew.yaml).
func (srv *Server) handleClusterManifest(w http.ResponseWriter, r *http.Request) {
	if why := srv.clusterJoinUnavailable(r); why != "" {
		http.Error(w, why, http.StatusNotFound)
		return
	}
	secret := ""
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		t, err := jointoken.Parse(tok)
		if !ok || err != nil || !srv.pendingClusterToken(HashToken(t.Secret)) {
			http.Error(w, "unknown, used or expired cluster join token: make a new one on Configure, Clusters", http.StatusUnauthorized)
			return
		}
		secret = clusterSecret(strings.TrimSpace(tok))
	}
	m := clusterManifest(srv.clusterImage(), secret, r.URL.Query().Get("cert_renew") == "1")
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(m))
}

// clusterManifest is cluster-agent.yaml for image, with the join token's
// Secret when there is one, and with certRenew, the agent's -cert-renew
// and the role it needs to ask cert-manager to renew a Certificate.
func clusterManifest(image, secret string, certRenew bool) string {
	m := strings.ReplaceAll(clusteragent.Manifest, "@IMAGE@", image)
	m = strings.Replace(m, clusteragent.SecretMarker+"\n", secret, 1)
	if certRenew {
		m = strings.Replace(m, clusteragent.Command, clusteragent.CertRenewCommand, 1) + "---\n" + clusteragent.CertRenew
	}
	return m
}

// pendingClusterToken says whether a token is waiting to be used, and is
// for a cluster.
func (srv *Server) pendingClusterToken(sha string) bool {
	ts, err := srv.db.PendingJoinTokens(time.Now())
	if err != nil {
		return false
	}
	for _, t := range ts {
		if t.TokenSHA256 == sha {
			return t.Cluster
		}
	}
	return false
}

// clusterSecret is the Secret the agent starts from: the whole token
// (it names the server and pins its CA). The agent adds its key and
// certificate when it has joined. Applying the manifest again without
// the token leaves them be: the manifest alone has no Secret.
func clusterSecret(tok string) string {
	return fmt.Sprintf(`---
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  join: %s
`, clusteragent.Secret, clusteragent.Namespace, tok)
}

// clusterAgentView is a cluster agent as Configure → Clusters lists it.
type clusterAgentView struct {
	Name      string     `json:"name"`
	Source    string     `json:"source"` // config or joined
	Added     *time.Time `json:"added,omitempty"`
	AddedBy   string     `json:"added_by,omitempty"`
	CertUntil *time.Time `json:"cert_until,omitempty"`
	Status    string     `json:"status"`
	Word      string     `json:"word"`
}

func (srv *Server) clusterAgentViews(certs map[string]time.Time) []clusterAgentView {
	now := time.Now()
	status := map[string][2]string{}
	for _, s := range srv.workloadSnapshot() {
		st, word, _ := srv.sourceStatus(s, now)
		status[s.name] = [2]string{st, word}
	}
	out := []clusterAgentView{}
	for _, n := range srv.clusterAgents() {
		v := clusterAgentView{Name: n, Source: "config", Status: status[n][0], Word: status[n][1]}
		srv.joinedMu.RLock()
		if j, ok := srv.joined[n]; ok && !srv.inConfig(n) {
			v.Source, v.Added, v.AddedBy = "joined", &j.Added, j.AddedBy
		}
		srv.joinedMu.RUnlock()
		if t, ok := certs[n]; ok {
			v.CertUntil = &t
		}
		out = append(out, v)
	}
	return out
}
