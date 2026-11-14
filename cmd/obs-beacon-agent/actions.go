package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/agentlink"
	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/version"
)

// Actions (docs/cert-rotation.md): after a report's answer says some are
// waiting, the agent fetches them, runs each its config allows, and puts
// what came of every one, done or refused, in its next report. The agent
// has the last word: obs-beacond only asks.

// actionDoers are the kinds of action this agent knows, each deciding for
// itself whether the config allows it.
var actionDoers = map[string]func(a *agent, ctx context.Context, args json.RawMessage) (string, error){}

func (a *agent) takeActions(ctx context.Context) {
	list, err := fetchActions(ctx, a.link, a.cfg.Server)
	if err != nil {
		a.once("actions:"+err.Error(), "%v", err)
		return
	}
	for _, act := range list {
		a.results = append(a.results, a.doAction(ctx, act))
	}
}

// fetchActions takes the actions waiting for this agent; a host agent and
// a cluster agent alike.
func fetchActions(ctx context.Context, link *agentlink.Link, server string) ([]report.Action, error) {
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, strings.TrimRight(server, "/")+"/api/v1/agent/actions", nil)
	if err != nil {
		return nil, err
	}
	link.Authorize(req)
	req.Header.Set("User-Agent", "obs-beacon-agent/"+version.Version)
	resp, err := link.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't fetch actions: %w", err)
	}
	defer resp.Body.Close()
	var list struct {
		Actions []report.Action `json:"actions"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list) != nil {
		return nil, fmt.Errorf("can't fetch actions: HTTP %d", resp.StatusCode)
	}
	return list.Actions, nil
}

// doAction runs one action, or says why not.
func (a *agent) doAction(ctx context.Context, act report.Action) report.ActionResult {
	res := report.ActionResult{ID: act.ID}
	do := actionDoers[act.Kind]
	if do == nil {
		res.Detail = "this agent doesn't do " + act.Kind + " actions (it may be older than obs-beacond)"
		log.Printf("action %s: %s", act.ID, res.Detail)
		return res
	}
	actx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	detail, err := do(a, actx, act.Args)
	if err != nil {
		res.Detail = err.Error()
		log.Printf("action %s (%s) not done: %v", act.ID, act.Kind, err)
		return res
	}
	res.OK, res.Detail = true, detail
	log.Printf("action %s (%s): %s", act.ID, act.Kind, detail)
	return res
}
