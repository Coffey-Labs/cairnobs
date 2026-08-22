# Phase 0 runbook

Walks one log line from a Linux host, through the Rust agent, Redpanda,
ingest, and ClickHouse, to a browser table. This is the actual
"done" criterion for Phase 0 — if this doesn't work, Phase 0 isn't done,
regardless of what any individual component's tests say.

**Update:** this sequence has since been run for real, more than once,
against a live Docker install — not just written and trusted. Two real
bugs turned up doing that (ClickHouse's official image silently disabling
network access without a password set; `rpk`'s exact flag syntax) and got
fixed; see the git history around the "Fix two bugs found by actually
running the Phase 0 pipeline end-to-end" commit if you want the details.
The steps below reflect what was actually run, not just planned. The
"Troubleshooting" section below is still worth reading first if something
doesn't work — it's not an exhaustive list, but it does reflect real
failures encountered, not hypothetical ones.

## Prerequisites

- Docker with **Compose v2** (`docker compose`, not the legacy
  `docker-compose` v1 binary) — the compose file uses
  `service_completed_successfully` conditions that v1 doesn't support.
- Rust toolchain (`cargo`) and `protoc` — to build the agent.
- `openssl` — to generate dev mTLS certs.
- A systemd-based Linux host to run the agent on (journald is the default
  source). If you're not on such a host, see `/agent/README.md`'s
  `file-tail` feature as an alternative source.

You do **not** need the musl cross-compilation target for this runbook —
that's for producing the distro-agnostic release binary. A native
`cargo build --release` is enough to run the agent on the same machine
you're testing on.

## 1. Generate dev mTLS certs

```sh
./hack/dev-certs/generate.sh
```

Writes a throwaway CA plus a server cert (for `ingest`) and a client cert
(for the agent) to `hack/dev-certs/out/`. Dev-only — see the script's
header comment for why.

## 2. Bring up the backend stack

```sh
docker compose up -d --build
```

This builds and starts, in dependency order: `redpanda` → `redpanda-provision`
(creates the `cairnobs.logs.raw` topic, then exits) → `clickhouse` →
`clickhouse-migrate` (applies `/storage/migrations`, then exits) →
`ingest` and `api` → `web`.

Check everything came up:

```sh
docker compose ps
```

`redpanda-provision` and `clickhouse-migrate` should show `Exited (0)`
(one-shot jobs, not long-running). Everything else should show `Up` /
`healthy`.

If `ingest` or `api` crash-looped, they likely started before their
`depends_on` conditions were actually satisfied, or the dev certs from
step 1 don't exist yet — check `docker compose logs ingest`.

## 3. Sanity-check the backend before involving the agent

```sh
curl http://localhost:8080/healthz
# -> 200, empty body

curl -X POST http://localhost:8080/query \
  -H 'Content-Type: application/json' \
  -d '{"sql": "SELECT 1"}'
# -> {"columns":["1"],"rows":[[1]]} (exact column name may vary by ClickHouse version)
```

This confirms `api` can reach `clickhouse` before you go looking for bugs
anywhere else. It doesn't touch the `logs` table, so it works even before
any agent has sent data.

## 4. Install the agent's mTLS material

The agent's default config expects certs at `/etc/cairnobs-agent/` (see
`/agent/config/agent.example.toml`), which requires root:

```sh
sudo mkdir -p /etc/cairnobs-agent
sudo cp hack/dev-certs/out/ca.pem \
        hack/dev-certs/out/client.pem \
        hack/dev-certs/out/client-key.pem \
        /etc/cairnobs-agent/
```

## 5. Build and run the agent

```sh
cd agent
cargo build --release
```

The agent's built-in defaults already match this setup with **zero
config file**: journald source (whole journal), service name `default`,
ingest endpoint `https://127.0.0.1:4317` (matches the port `ingest`
publishes in `docker-compose.yml`), and the cert paths from step 4. This
is the "no required flags for the common case" design goal from
`/agent/README.md` — if it doesn't just run, that design assumption is
wrong somewhere and worth reporting as a bug, not working around.

Reading the system journal generally needs root (or membership in the
`systemd-journal` group with a distro that grants it read access — varies
by distro, root is the reliable path for this runbook):

```sh
sudo RUST_LOG=info ./target/release/cairnobs-agent
```

`RUST_LOG=info` matters: `tracing_subscriber`'s default filter is
otherwise strict enough to suppress even the startup log line, and the
agent will look like it's silently doing nothing. Leave it running in
this terminal — you should see a `connected to ingest service` log line.
If you see a TLS or connection error instead, stop here and check the
Troubleshooting section before continuing.

## 6. Generate a test log line

In another terminal, **after** the agent is running and connected
(journald tailing starts from "now" — anything logged before the agent
started won't be picked up):

```sh
logger "hello from cairnobs phase 0"
```

`logger` (part of util-linux, present on virtually every Linux distro)
writes this to the system log, which journald captures immediately.

Give it a couple of seconds — the agent batches with a 2-second flush
interval by default, so the line won't hit ingest instantly.

## 7. Confirm it's queryable

**Via the web UI:**

`web` is already running from step 2 (`docker compose up -d --build`
starts every service in the file). Open `http://localhost:3000`, run the
default query (`SELECT * FROM logs
ORDER BY timestamp DESC LIMIT 100`), and look for a row with
`message = "hello from cairnobs phase 0"`.

**Or via curl, if you want to skip the browser:**

```sh
curl -X POST http://localhost:8080/query \
  -H 'Content-Type: application/json' \
  -d '{"sql": "SELECT * FROM logs ORDER BY timestamp DESC LIMIT 10"}'
```

**Or via cairnobsctl, just to confirm api is up (doesn't check the data
itself):**

```sh
cd cli && go run ./cmd/cairnobsctl ping
```

If you see the row: that's Phase 0 done, end to end. If you don't, see
Troubleshooting below.

## Tearing down

```sh
docker compose down        # stops and removes containers, keeps volumes
docker compose down -v     # also wipes Redpanda/ClickHouse data — start clean next time
```

## Troubleshooting

**Agent logs a TLS/certificate error on startup.**
Check the server cert's SAN actually covers how the agent is connecting
(`openssl x509 -in hack/dev-certs/out/server.pem -noout -ext
subjectAltName` — should list `DNS:ingest, DNS:localhost,
IP:127.0.0.1`). If you changed the agent's `ingest.endpoint` to something
not in that list, regenerate certs with an updated SAN in
`hack/dev-certs/generate.sh`, don't disable TLS verification.

**Agent connects but no data ever shows up in ClickHouse.**
Check each hop in order rather than guessing:
1. `docker compose logs ingest` — look for "batch produced to redpanda"
   (gRPC front end got the batch) vs. errors.
2. `docker compose logs ingest` again — look for "batch flushed to
   clickhouse" from the consumer half. If you see repeated "clickhouse
   batch write failed... will redeliver" messages, `clickhouse-migrate`
   likely hasn't finished (check `docker compose ps`) — the consumer will
   keep retrying and self-heal once the table exists, per its
   at-least-once design (see `/ingest/README.md`), so this may just need
   more time rather than intervention.
3. `docker compose exec redpanda rpk topic list` — confirm
   `cairnobs.logs.raw` exists (if `redpanda-provision` failed, it won't).

**`docker compose up` fails on `service_completed_successfully`.**
You're likely on Compose v1 (`docker-compose`, hyphenated) rather than v2
(`docker compose`, space) — see Prerequisites.

**Web UI query returns an error instead of rows.**
Open the browser's network tab — if the request never leaves the page
(CORS error in the console), confirm `api`'s `CORS_ALLOWED_ORIGIN`
(defaults to `*`, should not be the issue) and that `VITE_API_BASE_URL`
was set correctly at `web`'s build time (it's baked in, not read at
container start — see `/web/README.md`).
