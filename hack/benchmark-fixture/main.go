// Command benchmark-fixture pushes a large, realistically varied
// synthetic dataset directly to ingest's gRPC endpoint, batched, so
// Phase 2's "modest dataset" query-latency benchmark
// (/docs/phase-2-runbook.md) has real data to measure against instead of
// an asserted number. Distinct from /hack/windows-fixture: that one
// sends a handful of realistic Windows events to test pipeline
// *correctness*; this one sends a lot of Linux-shaped events to test
// query *performance* at volume.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

var (
	services  = []string{"api", "web", "worker", "db", "auth"}
	hosts     = []string{"host-01", "host-02", "host-03", "host-04", "host-05", "host-06", "host-07", "host-08"}
	severites = []logsv1.Severity{
		logsv1.Severity_SEVERITY_DEBUG,
		logsv1.Severity_SEVERITY_INFO,
		logsv1.Severity_SEVERITY_INFO,
		logsv1.Severity_SEVERITY_INFO,
		logsv1.Severity_SEVERITY_WARN,
		logsv1.Severity_SEVERITY_ERROR,
	}
	// A mix of messages, some containing terms worth full-text
	// searching for (connection refused, timeout) so the benchmark's
	// text-search-plus-aggregation case has real matches to find, not
	// just structured rows.
	messages = []string{
		"request completed successfully",
		"connection refused by upstream",
		"request timeout after 30s",
		"cache miss, falling back to database",
		"connection refused: too many open connections",
		"user authentication succeeded",
		"slow query detected: timeout approaching",
		"health check passed",
		"retrying after connection refused error",
		"scheduled job completed",
	}
)

func main() {
	addr := flag.String("addr", "localhost:4317", "ingest gRPC address")
	caFile := flag.String("ca", "../dev-certs/out/ca.pem", "CA cert path")
	certFile := flag.String("cert", "../dev-certs/out/client.pem", "client cert path")
	keyFile := flag.String("key", "../dev-certs/out/client-key.pem", "client key path")
	count := flag.Int("count", 1_000_000, "total number of records to generate")
	batchSize := flag.Int("batch-size", 1000, "records per PushBatch call")
	concurrency := flag.Int("concurrency", 16, "concurrent PushBatch calls in flight")
	timeSpread := flag.Duration("time-spread", 0, "spread record timestamps uniformly at random across [now-spread, now] instead of all landing at ~now -- 0 (default) preserves the original all-at-now behavior the volume benchmark wants; a real duration (e.g. 6h) is for building a demo/exploration dataset with a real time axis")
	includeFatal := flag.Bool("include-fatal", false, "include a low-frequency FATAL severity in the mix (off by default -- the volume benchmark's severity mix is deliberately unchanged unless asked for)")
	flag.Parse()

	recordSeverities := severites
	if *includeFatal {
		// Triple the existing 6-entry pool and append FATAL once, so it
		// lands at roughly 1-in-19 -- rare relative to ERROR, matching
		// how a real incident's FATAL/critical rate compares to its
		// error rate, not a coin-flip mix.
		recordSeverities = append(append(append([]logsv1.Severity{}, severites...), severites...), severites...)
		recordSeverities = append(recordSeverities, logsv1.Severity_SEVERITY_FATAL)
	}

	tlsConf, err := loadTLSConfig(*caFile, *certFile, *keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading TLS config:", err)
		os.Exit(1)
	}

	// One shared connection: gRPC multiplexes concurrent RPCs over HTTP/2
	// streams on a single connection, so concurrency here comes from
	// concurrent PushBatch calls, not from opening more connections.
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsConf)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dialing ingest:", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := logsv1.NewLogIngestClient(conn)

	numBatches := (*count + *batchSize - 1) / *batchSize
	batchIndexes := make(chan int, numBatches)
	for i := 0; i < numBatches; i++ {
		batchIndexes <- i
	}
	close(batchIndexes)

	var sent atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batchIdx := range batchIndexes {
				offset := batchIdx * *batchSize
				n := *batchSize
				if remaining := *count - offset; remaining < n {
					n = remaining
				}
				records := make([]*logsv1.LogRecord, n)
				for i := range records {
					records[i] = randomRecord(recordSeverities, *timeSpread)
				}

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				resp, err := client.PushBatch(ctx, &logsv1.PushBatchRequest{
					BatchId: fmt.Sprintf("benchmark-%d", batchIdx),
					Records: records,
				})
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "PushBatch %d failed: %v\n", batchIdx, err)
					os.Exit(1)
				}

				total := sent.Add(int64(resp.GetAccepted()))
				if total%int64(*batchSize*50) < int64(*batchSize) {
					elapsed := time.Since(start)
					rate := float64(total) / elapsed.Seconds()
					fmt.Printf("sent %d/%d (%.0f records/sec)\n", total, *count, rate)
				}
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(start)
	total := sent.Load()
	fmt.Printf("done: %d records in %s (%.0f records/sec)\n", total, elapsed, float64(total)/elapsed.Seconds())
}

func randomRecord(recordSeverities []logsv1.Severity, timeSpread time.Duration) *logsv1.LogRecord {
	ts := time.Now()
	if timeSpread > 0 {
		ts = ts.Add(-time.Duration(rand.Int63n(int64(timeSpread))))
	}
	return &logsv1.LogRecord{
		TimestampUnixNano: ts.UnixNano(),
		Host:              hosts[rand.Intn(len(hosts))],
		Service:           services[rand.Intn(len(services))],
		Severity:          recordSeverities[rand.Intn(len(recordSeverities))],
		Message:           messages[rand.Intn(len(messages))],
		Attributes: map[string]string{
			"status":     fmt.Sprintf("%d", []int{200, 200, 200, 301, 404, 500, 503}[rand.Intn(7)]),
			"latency_ms": fmt.Sprintf("%d", rand.Intn(2000)),
		},
	}
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
