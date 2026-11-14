package server

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/clusterwatch"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/vault"
	"github.com/cairnobs/cairnobs/internal/version"
)

// Vault and OpenBao clusters obs-beacond reads itself (docs/vault.md,
// decision 1). Only the evaluator reads them, as it polls Cloudflare and
// Patroni; what it reads goes into the workload tables under
// "obs-beacond:<name>", as a host agent's arrays do under its name, so
// every copy shows them and they're judged like any cluster.

// serverSource is the agent name obs-beacond keeps what it reads under.
// No agent may take it.
const serverSource = "obs-beacond"

// serverReads says whether key is something obs-beacond reads now.
func (srv *Server) serverReads(key string) bool {
	agent, name, ok := strings.Cut(key, ":")
	if !ok || agent != serverSource {
		return false
	}
	for _, v := range srv.cfg.Vault {
		if v.Name == name {
			return true
		}
	}
	return false
}

// serverSourced says whether a source is one obs-beacond reads itself.
func serverSourced(key string) bool { return sourceAgent(key) == serverSource }

// forgetServerSources drops what obs-beacond read before and no longer
// does: a Vault taken out of obs.yaml.
func (srv *Server) forgetServerSources() {
	var gone []string
	ws := &srv.wl
	ws.mu.RLock()
	for key := range ws.src {
		if sourceAgent(key) == serverSource && !srv.serverReads(key) {
			gone = append(gone, key)
		}
	}
	ws.mu.RUnlock()
	srv.dropSources(gone, "it's no longer in obs.yaml")
}

// pollVaults reads each Vault cluster, while this copy is the evaluator.
func (srv *Server) pollVaults(ctx context.Context) {
	srv.forgetServerSources()
	for _, cfg := range srv.cfg.Vault {
		c, err := vault.New(cfg)
		if err != nil {
			log.Printf("vault %s: %v", cfg.Name, err) // LoadConfig checked it: not expected
			continue
		}
		go srv.pollVault(ctx, c)
	}
}

// pollVault reads one cluster every so often, and stores what changed as
// a cluster agent sends it: batches, a full listing now and then.
func (srv *Server) pollVault(ctx context.Context, c *vault.Client) {
	key := report.SourceKey(serverSource, c.Name())
	send := func(ctx context.Context, b report.Workloads) error {
		return srv.storeWorkloads(key, b, time.Now())
	}
	bt := clusterwatch.NewBatcher(clusterwatch.Config{Name: serverSource, Agent: version.Version}, send)
	every := c.Config().Every
	var recs []report.Workload
	var use []report.Usage
	var readAt, usedAt time.Time
	failing := ""
	t := time.NewTicker(bt.Every())
	defer t.Stop()
	for {
		now := time.Now()
		if now.Sub(readAt) >= every {
			readAt = now
			rctx, cancel := context.WithTimeout(ctx, min(every, 50*time.Second))
			s, err := c.Read(rctx, now)
			cancel()
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				if err.Error() != failing {
					failing = err.Error()
					log.Printf("vault %s: %v", c.Name(), err)
				}
				recs = vault.Unreachable(c.Name(), recs, err)
			default:
				if failing != "" {
					log.Printf("vault %s: answers again", c.Name())
					failing = ""
				}
				if s.Renewed {
					log.Printf("vault %s: the monitoring token renewed itself", c.Name())
				}
				recs = vault.Records(s)
				if u := s.Usage(); u != nil {
					use, usedAt = u, now
				}
			}
		}
		bt.Tick(ctx, recs, use, usedAt, now)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
