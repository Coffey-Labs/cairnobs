# windows-fixture

Sends synthetic Windows Event Log-shaped `PushBatchRequest`s directly to
`ingest`'s gRPC endpoint, bypassing the actual Windows agent entirely.

## What this does and doesn't test

**Tests:** can the pipeline (ingest → ClickHouse → search → api → web)
correctly handle Windows-*shaped* data — the `winevt.*` attributes, the
`record_id` join between SQL and full-text search, Windows severity
levels mapping onto the right column values? This is exactly what's
automatable without a Windows host, and it's genuinely exercised: five
realistic, well-known Windows events (failed/successful logon, a service
state change, an application crash, an unexpected reboot) with real
EventIDs and providers.

**Does not test:** whether the real Windows agent's `EvtSubscribe`/ETW
integration actually works, whether Windows service registration
succeeds, whether ETW session creation/provider enabling works. Those are
fundamentally different questions — they need a real or virtualized
Windows host, and nothing here pretends otherwise. See
`/docs/phase-1-runbook.md` for exactly which is which.

## Running

Requires the docker-compose stack up (`ingest` reachable, dev certs
generated):

```sh
cd hack/windows-fixture
go run . --count 5
```

```
sent 5 synthetic Windows-shaped records, ingest accepted 5
  [SEVERITY_WARN] An account failed to log on. (event_id=4625 provider=Microsoft-Windows-Security-Auditing)
  ...
```

Then confirm both query paths see it:

```sh
curl -s -X POST http://localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query": "SELECT host, severity, message, attributes['"'"'winevt.event_id'"'"'] AS event_id FROM logs WHERE host = '"'"'WIN-FIXTURE-01'"'"' ORDER BY timestamp DESC"}'

curl -s -X POST http://localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query": "notepad"}'
```

## Multi-tenant deployments

Against an `ingest` with `ENTERPRISE_AUTH_URL` set, every agent must
present a tenant bearer token or be refused outright
(`Unauthenticated: missing bearer credential`). Mint one and pass it:

```sh
docker compose run --rm enterprise-auth -create-ingest-credential-tenant=acme
go run . --count 3 --token '<the token>'
```

The records are then tagged with that tenant's id, and
`enterprise-ingest` routes them into that tenant's own ClickHouse
database rather than the shared `logs` table.

Omit `--token` and nothing is sent, which is what a single-tenant
deployment expects -- the Phase 0-3 behaviour of this fixture is
unchanged.

**The tenant needs a schema first.** `enterprise-api -provision-tenant`
creates the database, user and grants but not the tables, so apply the
storage migrations to the tenant's own database before sending anything
to it -- see `docker-compose.tenant-ingest.yml`.

Flags: `--addr` (default `localhost:4317`), `--token`, `--ca`/`--cert`/`--key`
(default to `../dev-certs/out/{ca,client,client-key}.pem`), `--count`
(default 5, cycles through the fixed event list if higher).
