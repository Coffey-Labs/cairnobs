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
REDPANDA_BROKERS=localhost:9092 ./provision-topics.sh
```

## In the full stack

The root-level `docker-compose.yml` builds this directory's `Dockerfile`
(FROM the Redpanda image itself, so `rpk` is already present) as a
one-shot init service that runs after Redpanda reports healthy. See
`/docs/phase-0-runbook.md`.
