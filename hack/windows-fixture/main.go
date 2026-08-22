// Command windows-fixture sends synthetic Windows Event Log-shaped
// PushBatchRequests directly to ingest's gRPC endpoint, bypassing the
// actual Windows agent entirely.
//
// This tests one specific thing: can the pipeline (ingest -> ClickHouse
// -> search -> api -> web) correctly handle Windows-*shaped* data (the
// winevt.* attributes, the record_id join, severity mapping)? It does
// NOT test whether the real Windows agent's EvtSubscribe/ETW integration
// actually works -- that's a fundamentally different question that can
// only be answered on a real or virtualized Windows host. See
// /docs/phase-1-runbook.md for exactly which is which.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

func main() {
	addr := flag.String("addr", "localhost:4317", "ingest gRPC address")
	caFile := flag.String("ca", "../dev-certs/out/ca.pem", "CA cert path")
	certFile := flag.String("cert", "../dev-certs/out/client.pem", "client cert path")
	keyFile := flag.String("key", "../dev-certs/out/client-key.pem", "client key path")
	count := flag.Int("count", 5, "number of synthetic events to send")
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
	records := syntheticWindowsRecords(*count)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.PushBatch(ctx, &logsv1.PushBatchRequest{
		BatchId: "windows-fixture",
		Records: records,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "PushBatch failed:", err)
		os.Exit(1)
	}

	fmt.Printf("sent %d synthetic Windows-shaped records, ingest accepted %d\n", len(records), resp.GetAccepted())
	for _, rec := range records {
		fmt.Printf("  [%s] %s (event_id=%s provider=%s)\n",
			rec.GetSeverity(), rec.GetMessage(), rec.GetAttributes()["winevt.event_id"], rec.GetAttributes()["winevt.provider"])
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

// A handful of realistic, well-known Windows Event Log entries (real
// EventIDs/providers/channels), cycled through if --count exceeds the
// list length.
func syntheticWindowsRecords(count int) []*logsv1.LogRecord {
	events := []struct {
		eventID  string
		provider string
		channel  string
		level    logsv1.Severity
		message  string
	}{
		{"4625", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_WARN, "An account failed to log on."},
		{"7036", "Service Control Manager", "System", logsv1.Severity_SEVERITY_INFO, "The Windows Update service entered the running state."},
		{"1000", "Application Error", "Application", logsv1.Severity_SEVERITY_ERROR, "Faulting application name: notepad.exe"},
		{"4624", "Microsoft-Windows-Security-Auditing", "Security", logsv1.Severity_SEVERITY_INFO, "An account was successfully logged on."},
		{"41", "Microsoft-Windows-Kernel-Power", "System", logsv1.Severity_SEVERITY_FATAL, "The system has rebooted without cleanly shutting down first."},
	}

	records := make([]*logsv1.LogRecord, 0, count)
	for i := 0; i < count; i++ {
		e := events[i%len(events)]
		records = append(records, &logsv1.LogRecord{
			TimestampUnixNano: time.Now().UnixNano(),
			Host:              "WIN-FIXTURE-01",
			Service:           "default",
			Severity:          e.level,
			Message:           e.message,
			Attributes: map[string]string{
				"winevt.event_id":      e.eventID,
				"winevt.provider":      e.provider,
				"winevt.channel":       e.channel,
				"winevt.computer":      "WIN-FIXTURE-01",
				"winevt.record_number": fmt.Sprintf("%d", 100000+i),
			},
		})
	}
	return records
}
