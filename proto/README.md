# proto

Shared `.proto` contracts. Source of truth for the agent↔ingest gRPC
service; each language generates its own bindings from these files rather
than sharing generated code across languages.

- `sentry/logs/v1/logs.proto` — `LogIngest.PushBatch`, the data-plane RPC
  an agent calls to ship log records.
- `sentry/agent/v1/agent_control.proto` — `AgentControl.CheckIn`, the
  control-plane RPC an agent calls (on its own heartbeat ticker, same
  push-not-pull posture) to report its config and fetch any remote
  override -- see /docs/agent-management-design.md. Same mTLS
  connection/listener as `LogIngest`, a second service on it rather than
  a second protocol.

## Go bindings

Go is the one language here with pre-generated, checked-in bindings
(`sentry/logs/v1/logs.pb.go`, `logs_grpc.pb.go`), living in this directory
as its own module (`github.com/cairnobs/cairnobs/proto`) that `/ingest` and
`/api` depend on via a local `replace` directive in their `go.mod`. Rust
(`/agent`) instead generates its bindings at build time via `tonic-build`
(see `agent/cairnobs-agent/build.rs`) — no checked-in Rust output.

To regenerate the Go bindings after changing either `.proto` file:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

cd proto
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       sentry/logs/v1/logs.proto sentry/agent/v1/agent_control.proto
go build ./...
```
