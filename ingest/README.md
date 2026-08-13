# ingest

Go service sitting between the Rust agent and ClickHouse. Two halves in one
binary, selected with `--mode`:

- **server** — mTLS gRPC front end (`LogIngest.PushBatch`) that agents
  connect to. Forwards each record, proto-encoded and unchanged, onto
  Redpanda. Does no normalization — kept thin so agent-facing latency isn't
  coupled to ClickHouse write performance.
- **consumer** — reads back off Redpanda, normalizes into the ClickHouse row
  shape (`internal/normalize`), and batch-writes via the native protocol
  driver. Commits Redpanda offsets only after a successful ClickHouse
  write, so a ClickHouse outage causes redelivery on restart rather than
  data loss.
- **all** (default) — both, in one process. This is what docker-compose
  runs. Splitting into two deployments later (e.g. to scale them
  independently in k8s) is a manifest change, not a code change — see
  `--mode`.

## Why Redpanda stays in the path

Confirmed with the project owner during Phase 0 planning: the gRPC front
end produces to Redpanda rather than writing ClickHouse directly. This
exercises the pinned transport layer from day one and keeps agents from
ever needing Kafka credentials — mTLS to `ingest` is the only network
egress an agent has. See `/docs/architecture.md`.

## Dependencies worth knowing about

- **github.com/segmentio/kafka-go** — pure Go, no cgo, chosen over
  franz-go/confluent-kafka-go specifically to keep the distroless build
  simple (confirmed with the project owner; see git history / PR
  discussion for the tradeoffs considered).
- **github.com/ClickHouse/clickhouse-go/v2** — official client, native
  protocol, pure Go (no cgo).
- **golang.org/x/sync/errgroup** — used in `cmd/ingest/main.go` to run the
  server and consumer halves concurrently and propagate the first error.

## Configuration

All via environment variables (see `internal/config/config.go` for the
full list and defaults) — no config file format for Phase 0:

| Var | Default | Purpose |
|---|---|---|
| `GRPC_LISTEN_ADDR` | `:4317` | Agent-facing gRPC listen address |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | `/etc/sentry-ingest/server{,-key}.pem` | ingest's own mTLS identity |
| `TLS_CLIENT_CA_FILE` | `/etc/sentry-ingest/ca.pem` | CA used to verify agent client certs |
| `REDPANDA_BROKERS` | `localhost:9092` | Comma-separated broker list |
| `REDPANDA_TOPIC` | `sentry.logs.raw` | Must match the topic provisioned in `/transport` |
| `REDPANDA_CONSUMER_GROUP` | `sentry-ingest` | Consumer group id |
| `CLICKHOUSE_ADDR` | `localhost:9000` | Native protocol port, not HTTP |
| `CLICKHOUSE_DATABASE` / `_USERNAME` / `_PASSWORD` | `sentry` / `default` / `` | |
| `CONSUMER_BATCH_MAX_SIZE` | `500` | Records per ClickHouse batch insert |
| `CONSUMER_BATCH_FLUSH_INTERVAL_MS` | `2000` | Max time a partial batch waits before flushing |

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

Requires `google.golang.org/protobuf/cmd/protoc-gen-go` and
`google.golang.org/grpc/cmd/protoc-gen-go-grpc` only if you're
regenerating `/proto`'s Go bindings — ingest itself just imports the
already-generated `github.com/sentry/sentry/proto` module (see the
`replace` directive in `go.mod`, pointing at `../proto`).

```sh
# from the repo root, not ingest/
docker build -f ingest/Dockerfile -t sentry-ingest .
```

## Testing notes

`internal/consumer` and `internal/grpcserver` depend on Redpanda and
ClickHouse only through small interfaces (`reader`/`chWriter` in consumer,
`batchProducer` in grpcserver), so the flush/commit/error-handling logic is
unit-tested against fakes — no embedded broker or database needed. What's
*not* covered by these tests: the real `kafka.Reader`/`kafka.Writer`
wiring and the ClickHouse native-protocol driver itself. Those are only
exercised by the docker-compose end-to-end flow described in
`/docs/phase-0-runbook.md`.
