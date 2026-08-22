// Command demo-simulator is the demo deployment's whole synthetic world
// in one process: a fictional fleet (see fleet.go) whose agents check in
// over AgentControl, report CPU/memory/disk, and ship realistically
// shaped logs for eight services -- backfilled across a window of
// history first, then continuously in real time for as long as it runs.
//
// Why one long-running process rather than another one-shot fixture:
// three of the demo's features are only convincing if data keeps
// arriving. The Agents page marks a host stale once it stops checking in
// (a one-shot fixture's fleet would go stale minutes after the nightly
// reset); alert rules evaluate over trailing windows like -5m and would
// settle into a permanent OK state against a frozen dataset; and a live
// tail or a "last 15 minutes" dashboard over a dataset that stopped
// growing at 03:00 shows an empty screen. Backfill alone can't fix any
// of those.
//
// It does not replace /hack/benchmark-fixture (volume benchmarking) or
// /hack/windows-fixture (Windows pipeline correctness) -- those stay the
// focused tools they were built as. This one is for the demo.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/cairnobs/cairnobs/proto/sentry/agent/v1"
	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

// metricsInterval is how often each host samples CPU/memory/disk and
// emits a heartbeat, in both backfill and live mode -- matching the real
// agent's default 60s heartbeat would multiply backfill volume for no
// visible gain on a chart, so history is sampled more coarsely than the
// present.
const (
	backfillMetricsInterval = 5 * time.Minute
	liveMetricsInterval     = time.Minute
	liveTick                = 5 * time.Second
)

func main() {
	addr := flag.String("addr", "localhost:4317", "ingest gRPC address")
	caFile := flag.String("ca", "../dev-certs/out/ca.pem", "CA cert path")
	certFile := flag.String("cert", "../dev-certs/out/client.pem", "client cert path")
	keyFile := flag.String("key", "../dev-certs/out/client-key.pem", "client key path")
	backfill := flag.Duration("backfill", 168*time.Hour, "how much history to generate before going live; 0 skips backfill")
	live := flag.Bool("live", true, "after backfill, keep generating events in real time until terminated")
	rateScale := flag.Float64("rate-scale", 0.5, "multiplier on every host's per-minute event rate -- the knob for how much total data a backfill produces")
	batchSize := flag.Int("batch-size", 1000, "records per PushBatch call")
	concurrency := flag.Int("concurrency", 4, "concurrent PushBatch calls in flight during backfill")
	seed := flag.Int64("seed", 0, "random seed; 0 uses the current time")
	dryRun := flag.Bool("dry-run", false, "generate the backfill without connecting to ingest and print what it would have sent, then exit")
	flag.Parse()

	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	if *dryRun {
		runDryRun(context.Background(), time.Now(), *backfill, *rateScale, *seed)
		return
	}

	tlsConf, err := loadTLSConfig(*caFile, *certFile, *keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading TLS config:", err)
		os.Exit(1)
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConf)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dialing ingest:", err)
		os.Exit(1)
	}
	defer conn.Close()

	logs := logsv1.NewLogIngestClient(conn)
	control := agentv1.NewAgentControlClient(conn)

	// origin is both the end of the backfill window and the reference
	// point every incident window is measured back from, so history and
	// live traffic tell one continuous story.
	origin := time.Now()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *backfill > 0 {
		runBackfill(ctx, logs, origin, *backfill, *rateScale, *batchSize, *concurrency, *seed)
	}
	if ctx.Err() != nil {
		return
	}
	if !*live {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runCheckIns(ctx, control) }()
	// metricsAnchor is the oldest moment this run's metrics series
	// covers. It has to be the same value in both modes: disk growth and
	// uptime are both measured from it, and anchoring live samples at
	// `origin` instead would make worker-02's disk usage jump backwards
	// (and every uptime reset to near zero) the instant backfill ended.
	metricsAnchor := origin.Add(-*backfill)
	go func() { defer wg.Done(); runLive(ctx, logs, origin, metricsAnchor, *rateScale, *seed) }()
	wg.Wait()
	log.Println("demo-simulator stopped")
}

// runBackfill walks the history window a minute at a time, streaming
// batches to a small pool of pushers as it goes rather than building the
// whole dataset in memory first -- the demo box this runs on has 3GB of
// RAM and a week of history is hundreds of thousands of records.
func runBackfill(ctx context.Context, client logsv1.LogIngestClient, origin time.Time, window time.Duration, rateScale float64, batchSize, concurrency int, seed int64) {
	start := origin.Add(-window)
	log.Printf("backfilling %s of history (%s .. %s) at rate-scale %.2f",
		window, start.UTC().Format(time.RFC3339), origin.UTC().Format(time.RFC3339), rateScale)

	batches := make(chan []*logsv1.LogRecord, concurrency*2)
	var sent atomic.Int64
	var failed atomic.Int64

	var pushers sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		pushers.Add(1)
		go func(worker int) {
			defer pushers.Done()
			for batch := range batches {
				n, err := push(ctx, client, fmt.Sprintf("demo-backfill-%d-%d", worker, sent.Load()), batch)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("backfill PushBatch failed: %v", err)
					}
					failed.Add(int64(len(batch)))
					continue
				}
				total := sent.Add(int64(n))
				if total%50000 < int64(batchSize) {
					log.Printf("backfill: %d records sent", total)
				}
			}
		}(i)
	}

	batch := make([]*logsv1.LogRecord, 0, batchSize)
	walkHistory(ctx, origin, window, rateScale, seed, func(rec *logsv1.LogRecord) {
		batch = append(batch, rec)
		if len(batch) >= batchSize {
			batches <- batch
			batch = make([]*logsv1.LogRecord, 0, batchSize)
		}
	})
	if len(batch) > 0 {
		batches <- batch
	}
	close(batches)
	pushers.Wait()

	if f := failed.Load(); f > 0 {
		log.Printf("backfill complete: %d records sent, %d dropped by failed pushes", sent.Load(), f)
		return
	}
	log.Printf("backfill complete: %d records sent", sent.Load())
}

// walkHistory replays the backfill window a minute at a time, handing
// every generated record to emit. Shared by the real backfill and
// -dry-run so the two can never disagree about what a run would produce.
func walkHistory(ctx context.Context, origin time.Time, window time.Duration, rateScale float64, seed int64, emit func(*logsv1.LogRecord)) {
	start := origin.Add(-window)
	rng := rand.New(rand.NewSource(seed))

	nextMetrics := start
	for minute := start; minute.Before(origin) && ctx.Err() == nil; minute = minute.Add(time.Minute) {
		metricsDue := !minute.Before(nextMetrics)
		if metricsDue {
			nextMetrics = minute.Add(backfillMetricsInterval)
		}
		for i := range fleet {
			h := &fleet[i]
			if h.stale {
				continue
			}
			c := conditionsAt(minute, origin, h)
			for _, rec := range minuteRecords(h, minute, rng, c, rateScale) {
				emit(rec)
			}
			if metricsDue {
				emit(metricsRecord(h, minute, start, rng))
				emit(heartbeatRecord(h, minute))
			}
		}
	}
}

// runDryRun generates a backfill without sending it anywhere and reports
// what it would have produced -- the volume/mix tuning knob, so
// -rate-scale and the per-host rates in fleet.go can be adjusted without
// pushing a few hundred thousand records into ClickHouse to find out.
func runDryRun(ctx context.Context, origin time.Time, window time.Duration, rateScale float64, seed int64) {
	byService := map[string]int{}
	bySeverity := map[string]int{}
	total := 0
	walkHistory(ctx, origin, window, rateScale, seed, func(rec *logsv1.LogRecord) {
		total++
		byService[rec.GetService()]++
		bySeverity[rec.GetSeverity().String()]++
	})

	fmt.Printf("dry run: %d records over %s at rate-scale %.2f (%.0f/min average)\n",
		total, window, rateScale, float64(total)/window.Minutes())
	fmt.Println("by service:")
	for _, k := range sortedKeys(byService) {
		fmt.Printf("  %-10s %8d\n", k, byService[k])
	}
	fmt.Println("by severity:")
	for _, k := range sortedKeys(bySeverity) {
		fmt.Printf("  %-22s %8d\n", k, bySeverity[k])
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// minuteRecords generates one host's events for one minute of wall
// clock: its primary service's traffic, plus the journald `system`
// stream every Linux host also ships.
func minuteRecords(h *host, minute time.Time, rng *rand.Rand, c conditions, rateScale float64) []*logsv1.LogRecord {
	shape := diurnal(minute) * rateScale
	var out []*logsv1.LogRecord
	for i := 0; i < countFor(h.eventsPerMin*shape, rng); i++ {
		out = append(out, primaryRecord(h, jitter(minute, rng), rng, c))
	}
	if h.systemPerMin > 0 {
		// System/journald volume rises during a probe window -- that
		// burst is the whole point of the Security dashboard's panels.
		sysRate := h.systemPerMin
		if c.bruteForce && internetFacing(h) {
			sysRate *= 12
		}
		for i := 0; i < countFor(sysRate*shape, rng); i++ {
			out = append(out, systemRecord(h, jitter(minute, rng), rng, c))
		}
	}
	return out
}

// countFor turns a fractional per-minute rate into a whole number of
// events, carrying the fraction as a probability so a host rated at 0.4
// events/min really does produce roughly two events every five minutes
// instead of none at all.
func countFor(rate float64, rng *rand.Rand) int {
	n := int(rate)
	if rng.Float64() < rate-float64(n) {
		n++
	}
	return n
}

func jitter(minute time.Time, rng *rand.Rand) time.Time {
	return minute.Add(time.Duration(rng.Int63n(int64(time.Minute))))
}

// runLive keeps the present moving: the same generators, driven by a
// ticker instead of a cursor, so trailing-window alert rules, the Agents
// page's staleness heuristic, and any "last 15 minutes" view all have
// something real to read.
func runLive(ctx context.Context, client logsv1.LogIngestClient, origin, metricsAnchor time.Time, rateScale float64, seed int64) {
	log.Printf("live mode: generating events every %s", liveTick)
	rng := rand.New(rand.NewSource(seed + 1))

	// Fractional carry per host: at a 5-second tick most hosts are owed
	// less than one event per tick, and dropping that remainder every
	// time would silently zero out every low-rate stream.
	carry := make(map[string]float64, len(fleet)*2)
	ticker := time.NewTicker(liveTick)
	defer ticker.Stop()
	metricsTicker := time.NewTicker(liveMetricsInterval)
	defer metricsTicker.Stop()

	tickFraction := liveTick.Minutes()
	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return

		case now := <-ticker.C:
			var batch []*logsv1.LogRecord
			for i := range fleet {
				h := &fleet[i]
				if h.stale {
					continue
				}
				c := conditionsAt(now, origin, h)
				shape := diurnal(now) * rateScale * tickFraction

				n := carried(carry, h.name+"/primary", h.eventsPerMin*shape)
				for j := 0; j < n; j++ {
					batch = append(batch, primaryRecord(h, between(last, now, rng), rng, c))
				}
				if h.systemPerMin > 0 {
					sysRate := h.systemPerMin
					if c.bruteForce && internetFacing(h) {
						sysRate *= 12
					}
					n := carried(carry, h.name+"/system", sysRate*shape)
					for j := 0; j < n; j++ {
						batch = append(batch, systemRecord(h, between(last, now, rng), rng, c))
					}
				}
			}
			last = now
			if len(batch) == 0 {
				continue
			}
			if _, err := push(ctx, client, fmt.Sprintf("demo-live-%d", now.Unix()), batch); err != nil && ctx.Err() == nil {
				log.Printf("live PushBatch failed: %v", err)
			}

		case now := <-metricsTicker.C:
			var batch []*logsv1.LogRecord
			for i := range fleet {
				h := &fleet[i]
				if h.stale {
					continue
				}
				batch = append(batch, metricsRecord(h, now, metricsAnchor, rng), heartbeatRecord(h, now))
			}
			if _, err := push(ctx, client, fmt.Sprintf("demo-metrics-%d", now.Unix()), batch); err != nil && ctx.Err() == nil {
				log.Printf("metrics PushBatch failed: %v", err)
			}
		}
	}
}

// carried accumulates a fractional event count for one stream until it
// crosses 1, then spends the whole part. The leftover is kept, not
// rounded away, so long-run volume matches the configured rate exactly
// rather than drifting low -- at a 5-second tick most streams are owed
// well under one event per tick, and rounding would zero them out.
func carried(carry map[string]float64, key string, rate float64) int {
	total := carry[key] + rate
	n := int(total)
	carry[key] = total - float64(n)
	return n
}

func between(from, to time.Time, rng *rand.Rand) time.Time {
	span := to.Sub(from)
	if span <= 0 {
		return to
	}
	return from.Add(time.Duration(rng.Int63n(int64(span))))
}

func push(ctx context.Context, client logsv1.LogIngestClient, batchID string, records []*logsv1.LogRecord) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.PushBatch(ctx, &logsv1.PushBatchRequest{BatchId: batchID, Records: records})
	if err != nil {
		return 0, err
	}
	return int(resp.GetAccepted()), nil
}

func loadTLSConfig(caFile, certFile, keyFile string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert %s: %w", caFile, err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in %s", caFile)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client cert/key: %w", err)
	}

	return &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{cert},
	}, nil
}
