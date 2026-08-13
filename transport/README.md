# transport

Redpanda for local development, plus the script that provisions the topic
`/ingest` depends on.

## Topic naming contract

`ingest` defaults to `REDPANDA_TOPIC=sentry.logs.raw` (see
`/ingest/internal/config`). `provision-topics.sh` defaults to the same
name. These aren't wired together automatically — if you change one,
change the other, or override `REDPANDA_TOPIC` consistently wherever
both are invoked.

## Running standalone

```sh
docker compose up -d
./provision-topics.sh   # defaults (localhost:9092 / localhost:9644) match this compose file
```

Two separate addresses matter here, confirmed by actually running this
against a live Redpanda container: `rpk cluster health` talks to the
**Admin API** (`REDPANDA_ADMIN_HOSTS`, port 9644), while `rpk topic ...`
talks to the **Kafka API** (`REDPANDA_BROKERS`, port 9092) — and neither
accepts a `--brokers` flag directly, both need `-X admin.hosts=...` /
`-X brokers=...`. Get this wrong and it doesn't error loudly: it just
retries the health check forever without ever reporting why.

## In the full stack

The root-level `docker-compose.yml` builds this directory's `Dockerfile`
(FROM the Redpanda image itself, so `rpk` is already present) as a
one-shot init service that runs after Redpanda reports healthy. See
`/docs/phase-0-runbook.md`.
