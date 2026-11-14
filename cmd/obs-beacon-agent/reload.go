package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/certreload"
	"github.com/cairnobs/cairnobs/internal/report"
)

// The "reload" action (docs/cert-rotation.md, step 2): reload a unit
// whose port serves an older certificate than its renewed file. The agent
// checks its own cert_reload and what the port serves now; the root helper
// checks cert_reload again, reading agent.yaml itself, and the files.

func init() { actionDoers["reload"] = (*agent).reload }

// reloadArgs are what obs-beacond sends: the unit, and the port it saw
// serving the old certificate.
type reloadArgs struct {
	Unit string `json:"unit"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

// confirmFor is how long the agent waits for the port to serve the new
// certificate after a reload.
var confirmFor = 15 * time.Second

func (a *agent) reload(ctx context.Context, raw json.RawMessage) (string, error) {
	var args reloadArgs
	if err := json.Unmarshal(raw, &args); err != nil || args.Unit == "" || args.Port <= 0 {
		return "", errors.New("bad reload request")
	}
	if _, ok := certreload.Rule(a.cfg.CertReload, args.Unit); !ok {
		return "", fmt.Errorf("%s isn't in this host's cert_reload", args.Unit)
	}
	if a.docker == nil {
		return "", errors.New("no root helper on this host")
	}
	if a.certs == nil {
		return "", errors.New("cert_scan is off on this host, so the agent can't see what the port serves")
	}
	where := fmt.Sprintf("%s:%d", args.Addr, args.Port)
	seen, known := a.certs.TryNow(ctx, args.Addr, args.Port, time.Now())
	switch {
	case !known:
		return "", fmt.Errorf("this host doesn't listen on %s", where)
	case seen == nil:
		return "", fmt.Errorf("%s doesn't answer TLS now", where)
	}
	res, err := a.docker.Reload(ctx, certreload.Request{Unit: args.Unit, Names: seen.Cert.Names, Served: seen.Cert.Fingerprint, Ends: seen.Cert.NotAfter})
	if err != nil {
		return "", err
	}
	// The reload went through; wait for the port to serve the new one.
	deadline := time.Now().Add(confirmFor)
	for {
		now := time.Now()
		if seen, _ = a.certs.TryNow(ctx, args.Addr, args.Port, now); seen != nil && seen.Cert.Fingerprint == res.Fingerprint {
			return fmt.Sprintf("reloaded %s; %s now serves %s (ends %s)", args.Unit, where, res.File, res.Ends.UTC().Format("2006-01-02")), nil
		}
		if now.After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	got := "nothing"
	if seen != nil {
		got = short(seen.Cert.Fingerprint)
	}
	return "", fmt.Errorf("reloaded %s, but after %s %s still serves %s, not %s from %s", args.Unit, confirmFor, where, got, short(res.Fingerprint), res.File)
}

func short(fp string) string {
	if len(fp) > 16 {
		return strings.ToUpper(fp[:16])
	}
	return strings.ToUpper(fp)
}

// reloadRules are the units sent to obs-beacond with each report, so it
// offers a reload only where one can happen.
func (a *agent) reloadRules() []report.ReloadRule {
	if a.docker == nil || a.certs == nil {
		return nil
	}
	return a.cfg.CertReload
}
