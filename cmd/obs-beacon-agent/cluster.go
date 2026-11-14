package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	clusteragent "github.com/cairnobs/cairnobs/deploy/kubernetes/cluster-agent"
	"github.com/cairnobs/cairnobs/internal/agentlink"
	"github.com/cairnobs/cairnobs/internal/clusterwatch"
	"github.com/cairnobs/cairnobs/internal/jointoken"
	"github.com/cairnobs/cairnobs/internal/kube"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/version"
)

// runCluster is `obs-beacon-agent cluster`: the cluster agent, run by
// cluster-agent.yaml's Deployment in the cluster it watches. Its Secret
// holds the join token (which names obs-beacond and pins its CA), and once
// it has joined, its key and certificate.
func runCluster(args []string) error {
	fs := flag.NewFlagSet("cluster", flag.ExitOnError)
	secret := fs.String("secret", clusteragent.Secret, "the Secret in the agent's namespace holding its join token, key and certificate")
	certScan := fs.Bool("cert-scan", true, "try a TLS handshake on each TLS Ingress and Route host once a day, for the certificate inventory")
	certRenew := fs.Bool("cert-renew", false, "let an admin ask cert-manager to renew a Certificate now (needs cert-renew.yaml's role; docs/cert-rotation.md)")
	_ = fs.Parse(args)
	kc, err := kube.InCluster()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st := &secretStore{kube: kc, path: fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", kc.Namespace(), *secret)}
	link, err := clusterLink(ctx, st)
	if err != nil || ctx.Err() != nil {
		return err
	}
	name := link.Name()
	log.Printf("obs-beacon-agent %s: cluster agent %s, sending to %s", version.Version, name, st.server)
	if *certRenew {
		log.Printf("cert-renew is on: an admin may ask cert-manager to renew a Certificate")
	}
	// Results of actions go with the next batch: a heartbeat at most 30
	// seconds away.
	var results []report.ActionResult
	send := func(ctx context.Context, b report.Workloads) error {
		if err := link.Maintain(ctx, time.Now()); err != nil {
			return err
		}
		b.Actions = results
		more, err := postWorkloads(ctx, link, st.server, b)
		if err != nil {
			return err
		}
		results = nil
		if more {
			acts, err := fetchActions(ctx, link, st.server)
			if err != nil {
				log.Printf("cluster agent: %v", err)
			}
			for _, act := range acts {
				results = append(results, clusterAction(ctx, kc, *certRenew, act))
			}
		}
		return nil
	}
	clusterwatch.Run(ctx, kc, clusterwatch.Config{Name: name, Agent: version.Version, NoCertScan: !*certScan, CertRenew: *certRenew}, send)
	return nil
}

// clusterAction does one action, or says why not. The one kind a cluster
// agent knows is renew, and only with -cert-renew.
func clusterAction(ctx context.Context, kc *kube.Client, certRenew bool, act report.Action) report.ActionResult {
	res := report.ActionResult{ID: act.ID}
	err := func() error {
		if act.Kind != "renew" {
			return fmt.Errorf("this agent doesn't do %s actions (it may be older than obs-beacond)", act.Kind)
		}
		if !certRenew {
			return errors.New("this cluster agent runs without -cert-renew")
		}
		a, err := clusterwatch.ParseRenew(act.Args)
		if err != nil {
			return err
		}
		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := clusterwatch.Renew(actx, kc, a.Namespace, a.Name, time.Now()); err != nil {
			return err
		}
		res.Detail = fmt.Sprintf("asked cert-manager to renew Certificate %s/%s", a.Namespace, a.Name)
		return nil
	}()
	if err != nil {
		res.Detail = err.Error()
		log.Printf("action %s (%s) not done: %v", act.ID, act.Kind, err)
	} else {
		res.OK = true
		log.Printf("action %s: %s", act.ID, res.Detail)
	}
	return res
}

// clusterLink joins with the Secret's token the first time, and from then
// on uses the certificate kept there. A token that can't be used is
// logged, and tried again every few minutes, so the agent waits for a new
// one rather than restarting over and over.
func clusterLink(ctx context.Context, st *secretStore) (*agentlink.Link, error) {
	for wait := 10 * time.Second; ; wait = min(wait*2, 5*time.Minute) {
		tok, err := st.token(ctx)
		if err == nil {
			cfg := agentlink.Config{Server: tok.Server, CAFingerprint: tok.CAFingerprint, Store: st}
			st.server = tok.Server
			var link *agentlink.Link
			if link, err = agentlink.New(cfg); err == nil && link.Ready() {
				return link, nil
			}
			if err == nil {
				var name string
				jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				name, err = agentlink.JoinStore(jctx, cfg, tok.Secret)
				cancel()
				if err == nil {
					log.Printf("joined as %s", name)
					continue // load what was kept
				}
			}
		}
		log.Printf("cluster agent: %v; trying again in %s", err, wait)
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(wait):
		}
	}
}

// postWorkloads sends a batch; true when obs-beacond says actions wait.
func postWorkloads(ctx context.Context, link *agentlink.Link, server string, b report.Workloads) (bool, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/api/v1/agent/workloads", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	link.Authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "obs-beacon-agent/"+version.Version)
	resp, err := link.Client().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Header.Get("X-Beacon-Actions") != "", nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		link.Refused()
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return false, rejected{code: resp.StatusCode, msg: strings.TrimSpace(string(msg))}
}

// secretStore keeps the agent's key and certificate in its Secret, which
// cluster-agent.yaml's Role lets it read and patch, and no other.
type secretStore struct {
	kube   *kube.Client
	path   string
	server string // from the token
}

type secretData struct {
	Data map[string]string `json:"data"` // base64, as the API keeps it
}

// The Secret's keys.
const (
	secretJoin = "join"
	secretKey  = "agent.key"
	secretCert = "agent.crt"
	secretCA   = "ca.pem"
)

func (s *secretStore) read(ctx context.Context) (map[string][]byte, error) {
	var sd secretData
	if err := s.kube.Get(ctx, s.path, &sd); err != nil {
		if kube.Code(err) == http.StatusNotFound {
			return nil, fmt.Errorf("the Secret %s isn't there: apply the manifest from Add a cluster", s.path)
		}
		return nil, err
	}
	out := map[string][]byte{}
	for k, v := range sd.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("the Secret's %s: %w", k, err)
		}
		out[k] = b
	}
	return out, nil
}

func (s *secretStore) token(ctx context.Context) (jointoken.Token, error) {
	d, err := s.read(ctx)
	if err != nil {
		return jointoken.Token{}, err
	}
	if len(d[secretJoin]) == 0 {
		return jointoken.Token{}, errors.New("the Secret holds no join token: apply the manifest from Add a cluster")
	}
	return jointoken.Parse(string(d[secretJoin]))
}

func (s *secretStore) Load() (key, cert, ca []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := s.read(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(d[secretKey]) == 0 || len(d[secretCert]) == 0 || len(d[secretCA]) == 0 {
		return nil, nil, nil, os.ErrNotExist
	}
	return d[secretKey], d[secretCert], d[secretCA], nil
}

// Save patches the three in one request, so they change together.
func (s *secretStore) Save(key, cert, ca []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	enc := base64.StdEncoding.EncodeToString
	return s.kube.MergePatch(ctx, s.path, map[string]any{"data": map[string]string{
		secretKey: enc(key), secretCert: enc(cert), secretCA: enc(ca)}})
}
