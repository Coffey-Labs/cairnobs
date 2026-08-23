# Runbook: rotating the ClickHouse `default` password

Applies to the docker-compose deployments (`proto.cairnobs.org`,
`demo.cairnobs.org`). For Helm deployments the value is a K8s `Secret`
(`/deploy/helm/cairnobs/templates/secrets.yaml`) and the procedure is a
secret update plus a rollout restart — only steps 1 and 5 below apply.

## What you are rotating

Not a login gate. `CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1` (set in
`docker-compose.yml` for Phase 4's per-tenant provisioning) makes the
`default` user a ClickHouse superuser:

```
$ docker exec cairnobs-clickhouse clickhouse-client --query "SHOW GRANTS FOR default"
GRANT SHOW, SELECT, INSERT, ALTER, CREATE, DROP, ... ACCESS MANAGEMENT,
CLUSTER ON *.* TO default WITH GRANT OPTION
```

Read/write over every tenant's logs, plus the ability to create further
users. See `/docs/security/threat-model.md` for how this sits in the
wider model.

## Before you start

- **Confirm the blast radius is what you think it is.** ClickHouse
  publishes only to loopback on these boxes (`127.0.0.1:8123`,
  `127.0.0.1:9000`); the only `0.0.0.0` listener is `cairnobs-ingest:4317`,
  the mTLS agent endpoint. Verify with `docker ps --format "{{.Names}}\t{{.Ports}}"`
  before assuming an exposure is or isn't reachable.
- **The boxes do not share a password.** Rotate them independently, and
  do not assume a value recovered from one applies to the other.
- **Rotate demo before proto**, matching the deploy risk order.

## The trap

`CLICKHOUSE_PASSWORD` is read by the official image's entrypoint **only
when it initialises an empty data volume**. On a box that has already
been running, editing the env var and restarting does *not* change the
stored password — ClickHouse keeps the old one, the clients present the
new one, and every query starts returning 403. The password must be
changed *inside* ClickHouse first.

## Procedure

Four compose services declare the credential: `clickhouse`,
`clickhouse-migrate`, `api`, `ingest`. Only three of them are
long-running — `clickhouse-migrate` is a one-shot that has already
exited on a running box, which is why `docker ps` shows three. **All
four must be updated in the override file**, or the next `docker compose
up` will run the migrate step with a stale password and fail.

`web`, `alerting`, `search`, `enterprise-auth`, `metadata-postgres`, and
`redpanda` do not carry it and do not need restarting.

**1. Generate a new value** and keep it somewhere you can paste from.

```sh
openssl rand -base64 24
```

**2. Change it inside ClickHouse first.**

```sh
docker exec cairnobs-clickhouse clickhouse-client \
  --query "ALTER USER default IDENTIFIED BY '<new-password>'"
```

Existing connections keep working; this affects authentication of new
ones. Expect the next `api`/`ingest` query to fail until step 4.

**3. Update the override file** — `docker-compose.override.yml` in the
deployment directory (`/opt/sentry` on proto, `/home/john/cairnobs-demo`
on demo). This file is gitignored precisely because it carries real
credentials; it is not in the repo and rsync will not overwrite it
unless you tell it to.

Update every occurrence, not just the `clickhouse` service's — `api` and
`ingest` each set their own copy.

**4. Restart the three services that carry it.**

```sh
docker compose up -d clickhouse api ingest
```

**5. Verify.** All three must pass:

```sh
# a) the credential works
docker exec cairnobs-clickhouse clickhouse-client \
  --user default --password '<new-password>' --query "SELECT 1"

# b) the app can still read logs (non-zero, and climbing on a live box)
docker exec cairnobs-clickhouse clickhouse-client \
  --query "SELECT count() FROM cairnobs.logs"

# c) no auth failures since the restart
docker compose logs --since 5m api ingest | grep -iE 'auth|403|denied'
```

If (a) passes but (b) or (c) fail, the override file still holds the old
value somewhere — recheck every service block in step 3.

## Rollback

Re-run step 2 with the previous password and restart the three services.
There is no schema or data change here, so rollback is symmetric and
carries no data-loss risk.

## Known gap

The compose path keeps the password in the container environment, where
`docker inspect` exposes it to anyone with Docker access — which on
these boxes is root-equivalent anyway. The Helm path already avoids this
via `secretKeyRef`. Closing it for compose would need a
`CLICKHOUSE_PASSWORD_FILE` variant in the config loaders
(`api/internal/config`, `ingest/internal/config`); only `TLS_*_FILE`
paths support file-sourced values today. Worth doing only if these hosts
ever gain more than single-operator access.
