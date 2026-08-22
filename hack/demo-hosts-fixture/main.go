// Command demo-hosts-fixture pushes synthetic cairnobs.metrics/
// cairnobs.heartbeat-tagged LogRecords directly to ingest's gRPC
// endpoint, one snapshot every -interval across -time-spread per
// synthetic host, so a demo deployment's Hosts page and per-host detail
// page (CPU/mem/disk charts) have something to show. Neither
// /hack/benchmark-fixture nor /hack/windows-fixture emit these two
// attributes -- only the real agent does
// (agent/cairnobs-agent/src/main.rs's send_metrics/send_heartbeat) --
// so this fills that gap for demo/exploration data specifically, the
// same use case benchmark-fixture's -time-spread flag was already added
// for.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

// Mirrors hack/benchmark-fixture's host/service pools so the same
// hosts show up consistently across both the raw log fixture and this
// one -- a viewer clicking from a host's log lines to its metrics page
// (or vice versa) sees the same host, not a disjoint set.
var (
	hosts        = []string{"host-01", "host-02", "host-03", "host-04", "host-05", "host-06", "host-07", "host-08"}
	hostServices = map[string]string{
		"host-01": "api", "host-02": "api", "host-03": "web", "host-04": "web",
		"host-05": "worker", "host-06": "worker", "host-07": "db", "host-08": "auth",
	}
	osNames        = []string{"Ubuntu 24.04.1 LTS", "Debian GNU/Linux 13 (trixie)", "Ubuntu 22.04.5 LTS"}
	kernelVersions = []string{"6.8.0-45-generic", "6.1.0-25-amd64", "5.15.0-118-generic"}
)

func main() {
	addr := flag.String("addr", "localhost:4317", "ingest gRPC address")
	caFile := flag.String("ca", "../dev-certs/out/ca.pem", "CA cert path")
	certFile := flag.String("cert", "../dev-certs/out/client.pem", "client cert path")
	keyFile := flag.String("key", "../dev-certs/out/client-key.pem", "client key path")
	timeSpread := flag.Duration("time-spread", 24*time.Hour, "spread metrics snapshots uniformly across [now-spread, now], one per interval per host")
	interval := flag.Duration("interval", 5*time.Minute, "spacing between synthetic metrics snapshots per host")
	flag.Parse()

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

	client := logsv1.NewLogIngestClient(conn)

	now := time.Now()
	var records []*logsv1.LogRecord
	for _, host := range hosts {
		service := hostServices[host]
		osName := osNames[rand.Intn(len(osNames))]
		kernel := kernelVersions[rand.Intn(len(kernelVersions))]
		cores := []int{2, 4, 8, 16}[rand.Intn(4)]
		memTotal := []int64{4, 8, 16, 32}[rand.Intn(4)] * 1024 * 1024 * 1024
		diskTotal := []int64{50, 100, 250, 500}[rand.Intn(4)] * 1024 * 1024 * 1024
		// A gently drifting baseline per host so the charts show real
		// variation instead of flat noise -- CPU/mem wander within a
		// plausible band across the snapshot window.
		cpuBase := 5 + rand.Float64()*40
		memBase := 0.3 + rand.Float64()*0.4
		diskBase := 0.2 + rand.Float64()*0.5
		uptime := int64(*timeSpread/time.Second) + rand.Int63n(30*86400)

		for t := now.Add(-*timeSpread); t.Before(now); t = t.Add(*interval) {
			cpuPercent := clamp(cpuBase+rand.NormFloat64()*8, 0.5, 98)
			memUsed := int64(float64(memTotal) * clamp(memBase+rand.NormFloat64()*0.05, 0.05, 0.95))
			diskUsed := int64(float64(diskTotal) * clamp(diskBase+rand.NormFloat64()*0.02, 0.05, 0.9))

			records = append(records, &logsv1.LogRecord{
				TimestampUnixNano: t.UnixNano(),
				Host:              host,
				Service:           service,
				Severity:          logsv1.Severity_SEVERITY_INFO,
				Message:           "host metrics",
				Attributes: map[string]string{
					"cairnobs.metrics": "true",
					"cpu_percent":      fmt.Sprintf("%.2f", cpuPercent),
					"mem_used_bytes":   strconv.FormatInt(memUsed, 10),
					"mem_total_bytes":  strconv.FormatInt(memTotal, 10),
					"disk_used_bytes":  strconv.FormatInt(diskUsed, 10),
					"disk_total_bytes": strconv.FormatInt(diskTotal, 10),
					"cpu_cores":        strconv.Itoa(cores),
					"os_name":          osName,
					"kernel_version":   kernel,
					"arch":             "x86_64",
					"uptime_seconds":   strconv.FormatInt(uptime-int64(now.Sub(t).Seconds()), 10),
					"ipv4_addresses":   fmt.Sprintf("10.0.0.%d", 10+rand.Intn(240)),
					"ipv6_addresses":   "",
				},
			})
			records = append(records, &logsv1.LogRecord{
				TimestampUnixNano: t.UnixNano(),
				Host:              host,
				Service:           service,
				Severity:          logsv1.Severity_SEVERITY_INFO,
				Message:           "agent heartbeat",
				Attributes:        map[string]string{"cairnobs.heartbeat": "true"},
			})
		}
	}

	fmt.Printf("pushing %d synthetic host-metrics/heartbeat records for %d hosts...\n", len(records), len(hosts))

	const batchSize = 500
	for offset := 0; offset < len(records); offset += batchSize {
		end := offset + batchSize
		if end > len(records) {
			end = len(records)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.PushBatch(ctx, &logsv1.PushBatchRequest{
			BatchId: fmt.Sprintf("demo-hosts-%d", offset),
			Records: records[offset:end],
		})
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "PushBatch at offset %d failed: %v\n", offset, err)
			os.Exit(1)
		}
		fmt.Printf("sent %d/%d (accepted %d)\n", end, len(records), resp.GetAccepted())
	}

	fmt.Println("done")
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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
