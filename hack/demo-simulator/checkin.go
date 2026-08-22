package main

import (
	"context"
	"log"
	"sync"
	"time"

	agentv1 "github.com/cairnobs/cairnobs/proto/sentry/agent/v1"
)

// The Agents page is populated by the AgentControl.CheckIn RPC, not by
// log volume: ingest's internal/agentregistry upserts one row per
// (tenant, host) on every check-in, and nothing else ever writes that
// table. A demo with plenty of logs but no check-ins therefore shows an
// empty Agents page -- which is exactly the state this replaces.
//
// The simulated agents are faithful to the real protocol in the two
// places a demo viewer can actually observe it:
//
//   - Remote config edits round-trip. The response's DesiredOverride
//     version is echoed back as applied_override_version on the next
//     check-in, so editing an agent's batch size or log paths in the web
//     UI shows the real pending -> applied transition instead of a badge
//     stuck on "pending" forever.
//   - Restart commands are consumed. A queued RESTART is delivered
//     at-most-once and cleared by ingest the moment it hands it out; the
//     simulated agent acknowledges it by logging and resetting its
//     applied-version state, the same visible outcome a real restart has.
type simAgent struct {
	h *host
	// appliedVersion is what this agent reports having applied. Starts
	// empty (never applied an override) and follows whatever the server
	// hands back, one check-in behind -- the same one-tick lag a real
	// agent has.
	appliedVersion string
	// applied is the override itself, kept so the *reported* config on
	// subsequent check-ins reflects it. A real agent restarts its
	// batcher/heartbeat with the new settings and then reports the new
	// values back; without this the web UI would show a config marked
	// "applied" next to reported values that never changed, which reads
	// like the edit silently failed.
	applied *agentv1.DesiredOverride
}

// reportedConfig is what the agent says it is currently running: its
// local agent.toml settings, with any applied override layered on top.
func (a *simAgent) reportedConfig() *agentv1.ReportedConfig {
	cfg := &agentv1.ReportedConfig{
		AgentVersion:         a.h.agentVersion,
		SourceKind:           a.h.sourceKind,
		SourceDetail:         a.h.sourceDetail,
		BatchMaxSize:         uint64(a.h.batchMax),
		BatchFlushIntervalMs: uint64(a.h.batchFlushMS),
		HeartbeatEnabled:     true,
		HeartbeatIntervalMs:  uint64(a.h.heartbeatMS),
	}
	if o := a.applied; o != nil {
		if o.BatchMaxSize != nil {
			cfg.BatchMaxSize = o.GetBatchMaxSize()
		}
		if o.BatchFlushIntervalMs != nil {
			cfg.BatchFlushIntervalMs = o.GetBatchFlushIntervalMs()
		}
		if o.HeartbeatEnabled != nil {
			cfg.HeartbeatEnabled = o.GetHeartbeatEnabled()
		}
		if o.HeartbeatIntervalMs != nil {
			cfg.HeartbeatIntervalMs = o.GetHeartbeatIntervalMs()
		}
		// A journald-unit override changes what the agent is tailing,
		// which is exactly what source_detail describes.
		if o.JournaldUnit != nil && a.h.sourceKind == "journald" {
			if u := o.GetJournaldUnit(); u != "" {
				cfg.SourceDetail = "unit=" + u
			} else {
				cfg.SourceDetail = "whole journal"
			}
		}
	}
	return cfg
}

func (a *simAgent) checkIn(ctx context.Context, client agentv1.AgentControlClient) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := client.CheckIn(ctx, &agentv1.CheckInRequest{
		Host:                   a.h.name,
		Service:                a.h.service,
		CurrentConfig:          a.reportedConfig(),
		AppliedOverrideVersion: a.appliedVersion,
	})
	if err != nil {
		return err
	}

	if resp.GetHasOverride() && resp.GetOverride() != nil {
		if v := resp.GetOverride().GetVersion(); v != a.appliedVersion {
			log.Printf("agent %s: applying remote config override version %s", a.h.name, v)
			a.appliedVersion = v
			a.applied = resp.GetOverride()
		}
	} else if a.applied != nil {
		// The override was cleared (DELETE /agents/{host}/config). A real
		// agent falls back to its local agent.toml at that point, and so
		// must this one -- otherwise the web UI shows an agent with no
		// override still reporting the settings that override gave it,
		// and the Clear button looks broken.
		log.Printf("agent %s: remote config override cleared, reverting to local config", a.h.name)
		a.applied = nil
		a.appliedVersion = ""
	}
	if resp.GetPendingCommand() == agentv1.AgentCommand_AGENT_COMMAND_RESTART {
		log.Printf("agent %s: restart command received, simulating restart", a.h.name)
	}
	return nil
}

// runCheckIns keeps every non-stale agent checking in on its own
// heartbeat interval for as long as ctx lives. Stale hosts check in once
// at startup and never again, which is what puts a genuine "stale" row
// on the Agents page a few minutes into any demo session.
func runCheckIns(ctx context.Context, client agentv1.AgentControlClient) {
	var agents []*simAgent
	for i := range fleet {
		agents = append(agents, &simAgent{h: &fleet[i]})
	}

	var wg sync.WaitGroup
	for _, a := range agents {
		if err := a.checkIn(ctx, client); err != nil {
			log.Printf("agent %s: initial check-in failed: %v", a.h.name, err)
		}
		if a.h.stale {
			continue
		}
		wg.Add(1)
		go func(a *simAgent) {
			defer wg.Done()
			ticker := time.NewTicker(time.Duration(a.h.heartbeatMS) * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := a.checkIn(ctx, client); err != nil && ctx.Err() == nil {
						log.Printf("agent %s: check-in failed: %v", a.h.name, err)
					}
				}
			}
		}(a)
	}
	log.Printf("registered %d simulated agents (%d checking in every heartbeat interval)", len(agents), len(agents)-staleCount())
	wg.Wait()
}

func staleCount() int {
	n := 0
	for i := range fleet {
		if fleet[i].stale {
			n++
		}
	}
	return n
}
