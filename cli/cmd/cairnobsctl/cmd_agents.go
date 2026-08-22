// Command surface for api/agents -- agent inventory, remote config, and
// lifecycle commands (see /docs/agent-management-design.md). Same
// list/get shape as dashboards/alerts, plus a "config" sub-subcommand
// (mirroring dashboards' "permissions") since an agent's remote
// override has its own get/set/clear lifecycle distinct from the
// resource itself.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

func cmdAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "cairnobsctl agents: expected a subcommand (list, get, config, restart)")
		return 1
	}
	apiURL, rest := extractAPIFlag(args[1:], os.Getenv)
	token := resolveToken(os.Getenv)

	switch args[0] {
	case "list":
		return httpGetJSON(apiURL, "/agents", token, stdout, stderr)
	case "get":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl agents get: missing host")
			return 1
		}
		return httpGetJSON(apiURL, "/agents/"+rest[0], token, stdout, stderr)
	case "config":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl agents config: expected a subcommand (get, set, clear)")
			return 1
		}
		return cmdAgentsConfig(rest, apiURL, token, stdout, stderr)
	case "restart":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl agents restart: missing host")
			return 1
		}
		// os.Stdin passed explicitly at this inner layer (not threaded
		// through cmdAgents' own signature) so the confirmation prompt
		// is testable the same way cmd_query.go's cmdQueryNL is -- tests
		// call cmdAgentsRestart directly with a fake reader.
		return cmdAgentsRestart(rest, apiURL, token, os.Stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "cairnobsctl agents: unknown subcommand %q (want list, get, config, restart)\n", args[0])
		return 1
	}
}

func cmdAgentsConfig(args []string, apiURL, token string, stdout, stderr io.Writer) int {
	sub, rest := args[0], args[1:]
	switch sub {
	case "get":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl agents config get: missing host")
			return 1
		}
		// Same GET /agents/{host} as plain "get" -- an agent's reported
		// config, desired override, and pending/applied status are all
		// one resource server-side; a narrower "config-only" response
		// shape isn't worth a second endpoint just for this command.
		return httpGetJSON(apiURL, "/agents/"+rest[0], token, stdout, stderr)
	case "set":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl agents config set: missing host")
			return 1
		}
		return cmdAgentsConfigSet(rest[0], rest[1:], apiURL, token, stdout, stderr)
	case "clear":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "cairnobsctl agents config clear: missing host")
			return 1
		}
		return httpMutateNoBody(http.MethodDelete, apiURL, "/agents/"+rest[0]+"/config", token, "", "config override cleared -- agent will run its local agent.toml again", stdout, stderr)
	default:
		fmt.Fprintf(stderr, "cairnobsctl agents config: unknown subcommand %q (want get, set, clear)\n", sub)
		return 1
	}
}

// agentInfo is a CLI-local mirror of api/agents.Agent's JSON shape --
// only the fields config-merging actually needs. Deliberately
// duplicated rather than imported (cli is a separate Go module from
// api), same convention as every other cross-module shared shape in
// this codebase (see ingest/internal/agentregistry.overrideFields).
type agentInfo struct {
	SourceKind           string               `json:"source_kind"`
	BatchMaxSize         int64                `json:"batch_max_size"`
	BatchFlushIntervalMS int64                `json:"batch_flush_interval_ms"`
	HeartbeatEnabled     bool                 `json:"heartbeat_enabled"`
	HeartbeatIntervalMS  int64                `json:"heartbeat_interval_ms"`
	DesiredOverride      *agentConfigOverride `json:"desired_override,omitempty"`
}

type agentConfigOverride struct {
	BatchMaxSize         *int64  `json:"batch_max_size,omitempty"`
	BatchFlushIntervalMS *int64  `json:"batch_flush_interval_ms,omitempty"`
	HeartbeatEnabled     *bool   `json:"heartbeat_enabled,omitempty"`
	HeartbeatIntervalMS  *int64  `json:"heartbeat_interval_ms,omitempty"`
	JournaldUnit         *string `json:"journald_unit,omitempty"`
}

// cmdAgentsConfigSet parses --batch-max-size/--batch-flush-interval-ms/
// --heartbeat-enabled/--heartbeat-interval-ms/--journald-unit, fetches
// the agent's current effective config, and PUTs the complete merged
// override -- api/agents.Store.SetOverride replaces the whole stored
// override, it doesn't patch individual fields (same as the web UI's
// edit form, see /docs/agent-management-design.md), so every field this
// command doesn't touch has to be carried forward from whatever's
// currently in effect (the existing override if one's set, otherwise
// the agent's reported value) rather than silently reset to zero.
func cmdAgentsConfigSet(host string, flagArgs []string, apiURL, token string, stdout, stderr io.Writer) int {
	var (
		batchMaxSize, batchFlushMS, heartbeatMS *int64
		heartbeatEnabled                        *bool
		journaldUnit                            *string
	)
	for i := 0; i < len(flagArgs); i++ {
		flag := flagArgs[i]
		next := func() (string, bool) {
			if i+1 >= len(flagArgs) {
				return "", false
			}
			i++
			return flagArgs[i], true
		}
		switch flag {
		case "--batch-max-size":
			v, ok := next()
			if !ok {
				fmt.Fprintln(stderr, "cairnobsctl agents config set: --batch-max-size requires a value")
				return 1
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				fmt.Fprintf(stderr, "cairnobsctl agents config set: invalid --batch-max-size %q: %v\n", v, err)
				return 1
			}
			batchMaxSize = &n
		case "--batch-flush-interval-ms":
			v, ok := next()
			if !ok {
				fmt.Fprintln(stderr, "cairnobsctl agents config set: --batch-flush-interval-ms requires a value")
				return 1
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				fmt.Fprintf(stderr, "cairnobsctl agents config set: invalid --batch-flush-interval-ms %q: %v\n", v, err)
				return 1
			}
			batchFlushMS = &n
		case "--heartbeat-interval-ms":
			v, ok := next()
			if !ok {
				fmt.Fprintln(stderr, "cairnobsctl agents config set: --heartbeat-interval-ms requires a value")
				return 1
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				fmt.Fprintf(stderr, "cairnobsctl agents config set: invalid --heartbeat-interval-ms %q: %v\n", v, err)
				return 1
			}
			heartbeatMS = &n
		case "--heartbeat-enabled":
			v, ok := next()
			if !ok {
				fmt.Fprintln(stderr, "cairnobsctl agents config set: --heartbeat-enabled requires true or false")
				return 1
			}
			b, err := strconv.ParseBool(v)
			if err != nil {
				fmt.Fprintf(stderr, "cairnobsctl agents config set: invalid --heartbeat-enabled %q: %v\n", v, err)
				return 1
			}
			heartbeatEnabled = &b
		case "--journald-unit":
			v, ok := next()
			if !ok {
				fmt.Fprintln(stderr, "cairnobsctl agents config set: --journald-unit requires a value (empty string clears the filter)")
				return 1
			}
			journaldUnit = &v
		default:
			fmt.Fprintf(stderr, "cairnobsctl agents config set: unknown flag %q\n", flag)
			return 1
		}
	}
	if batchMaxSize == nil && batchFlushMS == nil && heartbeatMS == nil && heartbeatEnabled == nil && journaldUnit == nil {
		fmt.Fprintln(stderr, "cairnobsctl agents config set: at least one of --batch-max-size, --batch-flush-interval-ms, --heartbeat-enabled, --heartbeat-interval-ms, --journald-unit is required")
		return 1
	}

	current, err := fetchAgent(apiURL, host, token)
	if err != nil {
		fmt.Fprintf(stderr, "cairnobsctl agents config set: fetching current state: %v\n", err)
		return 1
	}

	mergedBatchMax := mergeInt64(batchMaxSize, overrideBatchMaxSize(current), current.BatchMaxSize)
	mergedBatchFlush := mergeInt64(batchFlushMS, overrideBatchFlushMS(current), current.BatchFlushIntervalMS)
	mergedHeartbeatMS := mergeInt64(heartbeatMS, overrideHeartbeatMS(current), current.HeartbeatIntervalMS)
	mergedHeartbeatEnabled := mergeBool(heartbeatEnabled, overrideHeartbeatEnabled(current), current.HeartbeatEnabled)

	merged := agentConfigOverride{
		BatchMaxSize:         &mergedBatchMax,
		BatchFlushIntervalMS: &mergedBatchFlush,
		HeartbeatEnabled:     &mergedHeartbeatEnabled,
		HeartbeatIntervalMS:  &mergedHeartbeatMS,
	}
	// journald_unit only applies (and is only ever sent) when the
	// agent's actual source is journald -- ignored server-side
	// otherwise anyway, but sending it for a non-journald agent would
	// be misleading in the stored override. Matches
	// web/src/routes/agents/[host]/+page.svelte's save() exactly.
	if current.SourceKind == "journald" {
		unit := ""
		if u := overrideJournaldUnit(current); u != nil {
			unit = *u
		}
		if journaldUnit != nil {
			unit = *journaldUnit
		}
		merged.JournaldUnit = &unit
	}

	body, err := json.Marshal(merged)
	if err != nil {
		fmt.Fprintf(stderr, "cairnobsctl agents config set: encoding request: %v\n", err)
		return 1
	}
	return httpPutJSON(apiURL, "/agents/"+host+"/config", token, string(body), stdout, stderr)
}

func mergeInt64(flag, override *int64, reported int64) int64 {
	if flag != nil {
		return *flag
	}
	if override != nil {
		return *override
	}
	return reported
}

func mergeBool(flag, override *bool, reported bool) bool {
	if flag != nil {
		return *flag
	}
	if override != nil {
		return *override
	}
	return reported
}

func overrideBatchMaxSize(a *agentInfo) *int64 {
	if a.DesiredOverride == nil {
		return nil
	}
	return a.DesiredOverride.BatchMaxSize
}

func overrideBatchFlushMS(a *agentInfo) *int64 {
	if a.DesiredOverride == nil {
		return nil
	}
	return a.DesiredOverride.BatchFlushIntervalMS
}

func overrideHeartbeatMS(a *agentInfo) *int64 {
	if a.DesiredOverride == nil {
		return nil
	}
	return a.DesiredOverride.HeartbeatIntervalMS
}

func overrideHeartbeatEnabled(a *agentInfo) *bool {
	if a.DesiredOverride == nil {
		return nil
	}
	return a.DesiredOverride.HeartbeatEnabled
}

func overrideJournaldUnit(a *agentInfo) *string {
	if a.DesiredOverride == nil {
		return nil
	}
	return a.DesiredOverride.JournaldUnit
}

func fetchAgent(apiURL, host, token string) (*agentInfo, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL+"/agents/"+host, nil)
	if err != nil {
		return nil, err
	}
	setAuth(req, token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errResp errorResponseBody
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("%s", errResp.Error)
		}
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var info agentInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &info, nil
}

// cmdAgentsRestart requires explicit confirmation -- interactive y/N,
// or --yes for scripted use -- same "never run something disruptive
// without an explicit signal" posture as cmd_query.go's --execute for
// running an AI-translated query, matching restart's own real (if
// brief) blast radius: it interrupts log collection on that host until
// the service manager brings the agent back up.
func cmdAgentsRestart(args []string, apiURL, token string, stdin io.Reader, stdout, stderr io.Writer) int {
	host := args[0]
	yes := false
	for _, a := range args[1:] {
		if a == "--yes" || a == "-y" {
			yes = true
		}
	}
	if !yes {
		if !isInteractive(stdin) {
			fmt.Fprintln(stdout, "Not restarting: pass --yes to confirm non-interactively.")
			return 1
		}
		prompt := fmt.Sprintf("Restart agent %q? This briefly interrupts log collection on that host.", host)
		if !confirmRun(stdin, stdout, prompt) {
			fmt.Fprintln(stdout, "Not restarting.")
			return 0
		}
	}
	return httpPutJSON(apiURL, "/agents/"+host+"/command", token, `{"command":"restart"}`, stdout, stderr)
}
