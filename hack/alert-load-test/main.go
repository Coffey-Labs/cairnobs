// Command alert-load-test seeds a realistic number of concurrent alert
// rules via alerting's real create API (not a direct DB insert -- the
// same discipline hack/benchmark-fixture uses, exercising the actual
// code path under test) and measures whether the evaluator's claim
// scheduling keeps up under load. See /docs/phase-3-alerting-design.md's
// "Load-testing plan" and /docs/phase-3-runbook.md for the methodology
// and real measured results from running this.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type target struct {
	ID string `json:"id"`
}

type ruleState struct {
	State             string  `json:"state"`
	LastEvaluatedAt   *string `json:"last_evaluated_at,omitempty"`
	LastEvalStatus    string  `json:"last_eval_status"`
	ConsecutiveErrors int     `json:"consecutive_errors"`
}

type rule struct {
	ID    string    `json:"id"`
	State ruleState `json:"state"`
}

var (
	alertingURL  = flag.String("alerting-url", "http://localhost:8081", "alerting service base URL")
	ruleCount    = flag.Int("rule-count", 500, "number of rules to seed")
	evalInterval = flag.Int("eval-interval-seconds", 60, "each rule's eval_interval_seconds")
	duration     = flag.Duration("duration", 5*time.Minute, "how long to observe after seeding")
	pollInterval = flag.Duration("poll-interval", 5*time.Second, "how often to poll GET /rules while observing")
	concurrency  = flag.Int("concurrency", 20, "concurrent rule-creation requests")
	skipCleanup  = flag.Bool("no-cleanup", false, "leave the seeded rules/target in place after the run")
	webhookURL   = flag.String("webhook-url", "http://sentry-webhook-sink:9099/", "notification target URL -- default assumes a webhook-sink container reachable on the compose network")
)

func main() {
	flag.Parse()
	client := &http.Client{Timeout: 30 * time.Second}

	fmt.Printf("creating notification target...\n")
	targetID, err := createTarget(client)
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating notification target:", err)
		os.Exit(1)
	}
	fmt.Printf("target_id=%s\n", targetID)

	fmt.Printf("seeding %d rules at %d concurrent requests...\n", *ruleCount, *concurrency)
	start := time.Now()
	ruleIDs, err := seedRules(client, targetID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seeding rules:", err)
		os.Exit(1)
	}
	fmt.Printf("seeded %d rules in %s\n", len(ruleIDs), time.Since(start))

	fmt.Printf("observing for %s (polling every %s)...\n", *duration, *pollInterval)
	observations := observe(client)

	report(observations, ruleIDs)

	if !*skipCleanup {
		fmt.Println("cleaning up...")
		cleanup(client, ruleIDs, targetID)
	}
}

func createTarget(client *http.Client) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"name": "alert-load-test", "kind": "webhook", "webhook_url": *webhookURL,
	})
	resp, err := client.Post(*alertingURL+"/targets", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t target
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	return t.ID, nil
}

// seedRules creates *ruleCount rules, each querying a different host's
// count over the last minute against real ClickHouse data (reuse
// hack/benchmark-fixture to populate that data first). threshold_value
// is deliberately unreachably high (real fixture volumes stay well
// under it) so rules stay in "ok" and the measurement here isolates
// evaluator/ClickHouse scheduling throughput, not delivery-worker load
// -- a query that never fires still exercises the exact same claim ->
// /query -> evaluateCondition -> ApplyTransition path every tick.
func seedRules(client *http.Client, targetID string) ([]string, error) {
	hosts := []string{"host-01", "host-02", "host-03", "host-04", "host-05", "host-06", "host-07", "host-08"}

	type result struct {
		id  string
		err error
	}
	work := make(chan int, *ruleCount)
	for i := 0; i < *ruleCount; i++ {
		work <- i
	}
	close(work)

	results := make(chan result, *ruleCount)
	var wg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				host := hosts[i%len(hosts)]
				body, _ := json.Marshal(map[string]any{
					"name": fmt.Sprintf("load-test-rule-%d", i),
					// host=host-01 (unquoted) fails to parse -- Phase 2's query
					// language lexer treats the "-" in an unquoted comparison
					// value as a token boundary (confirmed by actually running
					// this query: "unexpected MINUS after query"). Quoting the
					// value is the fix, not a language bug worth chasing here.
					"query":                  fmt.Sprintf(`earliest=-1m host="%s" | stats count`, host),
					"condition_type":         "threshold",
					"comparator":             "gt",
					"threshold_value":        1_000_000_000,
					"eval_interval_seconds":  *evalInterval,
					"notification_target_id": targetID,
				})
				resp, err := client.Post(*alertingURL+"/rules", "application/json", bytes.NewReader(body))
				if err != nil {
					results <- result{err: err}
					continue
				}
				var r rule
				decodeErr := json.NewDecoder(resp.Body).Decode(&r)
				resp.Body.Close()
				if decodeErr != nil || r.ID == "" {
					results <- result{err: fmt.Errorf("rule %d: unexpected response (status %d)", i, resp.StatusCode)}
					continue
				}
				results <- result{id: r.ID}
			}
		}()
	}
	wg.Wait()
	close(results)

	var ids []string
	var errs int
	for r := range results {
		if r.err != nil {
			errs++
			if errs <= 5 {
				fmt.Fprintln(os.Stderr, "seed error:", r.err)
			}
			continue
		}
		ids = append(ids, r.id)
	}
	if errs > 0 {
		fmt.Fprintf(os.Stderr, "%d rule-creation errors (showing up to 5 above)\n", errs)
	}
	return ids, nil
}

func observe(client *http.Client) [][]rule {
	deadline := time.Now().Add(*duration)
	var polls [][]rule
	for time.Now().Before(deadline) {
		resp, err := client.Get(*alertingURL + "/rules")
		if err == nil {
			var rules []rule
			if json.NewDecoder(resp.Body).Decode(&rules) == nil {
				polls = append(polls, rules)
			}
			resp.Body.Close()
		}
		time.Sleep(*pollInterval)
	}
	return polls
}

func report(observations [][]rule, seededIDs []string) {
	if len(observations) < 2 {
		fmt.Println("not enough observations to compute drift (need at least 2 polls)")
		return
	}

	// Per rule, collect the distinct last_evaluated_at timestamps seen
	// across polls, in order -- consecutive differences are the observed
	// inter-evaluation intervals.
	seen := map[string][]string{}
	latestErrors := map[string]int{}
	for _, poll := range observations {
		for _, r := range poll {
			if r.State.LastEvaluatedAt != nil {
				ts := *r.State.LastEvaluatedAt
				vals := seen[r.ID]
				if len(vals) == 0 || vals[len(vals)-1] != ts {
					seen[r.ID] = append(vals, ts)
				}
			}
			latestErrors[r.ID] = r.State.ConsecutiveErrors
		}
	}

	var intervals []float64
	for _, timestamps := range seen {
		var parsed []time.Time
		for _, ts := range timestamps {
			t, err := time.Parse(time.RFC3339Nano, ts)
			if err == nil {
				parsed = append(parsed, t)
			}
		}
		sort.Slice(parsed, func(i, j int) bool { return parsed[i].Before(parsed[j]) })
		for i := 1; i < len(parsed); i++ {
			intervals = append(intervals, parsed[i].Sub(parsed[i-1]).Seconds())
		}
	}

	erroring := 0
	for _, n := range latestErrors {
		if n > 0 {
			erroring++
		}
	}

	fmt.Println()
	fmt.Println("=== alert-load-test results ===")
	fmt.Printf("rules seeded: %d\n", len(seededIDs))
	fmt.Printf("rules observed with at least one evaluation: %d\n", len(seen))
	fmt.Printf("rules with consecutive_errors > 0 at last poll: %d\n", erroring)
	fmt.Printf("configured eval_interval_seconds: %d\n", *evalInterval)
	if len(intervals) == 0 {
		fmt.Println("no inter-evaluation intervals observed (duration may be too short relative to eval_interval_seconds)")
		return
	}
	sort.Float64s(intervals)
	fmt.Printf("observed evaluation intervals (n=%d): min=%.1fs mean=%.1fs p50=%.1fs p95=%.1fs max=%.1fs\n",
		len(intervals), intervals[0], mean(intervals), percentile(intervals, 0.5), percentile(intervals, 0.95), intervals[len(intervals)-1])
	fmt.Printf("drift vs configured interval (p95 - configured): %.1fs\n", percentile(intervals, 0.95)-float64(*evalInterval))
}

func mean(xs []float64) float64 {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

func cleanup(client *http.Client, ruleIDs []string, targetID string) {
	for _, id := range ruleIDs {
		req, _ := http.NewRequest(http.MethodDelete, *alertingURL+"/rules/"+id, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
	req, _ := http.NewRequest(http.MethodDelete, *alertingURL+"/targets/"+targetID, nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	fmt.Printf("deleted %d rules and 1 target\n", len(ruleIDs))
}
