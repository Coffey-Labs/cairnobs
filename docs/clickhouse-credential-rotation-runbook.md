# Runbook: rotating the ClickHouse `default` password

Applies to the docker-compose deployments (`proto.cairnobs.org`,
`demo.cairnobs.org`). For Helm deployments the value is a K8s `Secret`
(`/deploy/helm/cairnobs/templates/secrets.yaml`) and the procedure is a
secret update plus a rollout restart. The `users_xml` mechanics in "The
trap" below apply there too — the chart's ClickHouse pod builds the same
`default-user.xml` from the same env var — so the shape is identical:
change the value, recreate the pod, verify. Only the sed-the-override
steps differ.

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

## The trap: do NOT reach for `ALTER USER`

The obvious move — change the password inside ClickHouse, then update
the env to match — **does not work here, and fails loudly**:

```
$ docker exec cairnobs-clickhouse clickhouse-client \
    --query "SELECT name, storage FROM system.users"
default	users_xml
```

`storage = users_xml` means the `default` user is defined by a config
file, not by SQL. ClickHouse treats that storage as read-only and
rejects `ALTER USER` against it. There is no in-database password to
change.

The password lives in `/etc/clickhouse-server/users.d/default-user.xml`,
which the official image's entrypoint **regenerates from
`CLICKHOUSE_PASSWORD` on every container start** — not only on first
init. That directory is part of the container filesystem; only
`/var/lib/clickhouse` (the data) is a volume, so the file is rebuilt
each time and the data survives untouched.

So the env var *is* the source of truth, and recreating the container is
what applies it. Verify by comparing the file's mtime against
`docker inspect <container> --format '{{.State.StartedAt}}'` — they
match to the second, before and after a rotation.

## Procedure

Four compose services declare the credential: `clickhouse`,
`clickhouse-migrate`, `api`, `ingest`. Only three of them are
long-running — `clickhouse-migrate` is a one-shot that has already
exited on a running box, which is why `docker ps` shows three. **All
four must be updated in the override file**, or the next `docker compose
up` will run the migrate step with a stale password and fail.

`web`, `alerting`, `search`, `enterprise-auth`, `metadata-postgres`, and
`redpanda` do not carry it and do not need restarting.

**1. Back up the override file.** It is the only copy of these
credentials, and it is the rollback path.

```sh
cd /opt/sentry   # or /home/john/cairnobs-demo on demo
cp -p docker-compose.override.yml \
      docker-compose.override.yml.bak.$(date +%Y%m%d-%H%M%S)
```

**2. Generate the value on the box, and never let it leave.** Use hex,
not base64: this string is interpolated into the generated
`default-user.xml`, and base64's `/`, `+`, and `=` are needless risk in
XML. Generating it remotely also keeps it out of your terminal
scrollback and out of any AI-assistant transcript.

```sh
NEW="$(openssl rand -hex 24)"
```

**3. Update all four occurrences in the override file.** Match only
indented `KEY: value` lines, so the header comment (which mentions
`CLICKHOUSE_PASSWORD` in prose) is left alone:

```sh
sed -i -E "s|^( +)CLICKHOUSE_PASSWORD: .*|\1CLICKHOUSE_PASSWORD: \"${NEW}\"|" \
  docker-compose.override.yml
grep -cE '^ +CLICKHOUSE_PASSWORD: ' docker-compose.override.yml   # expect 4
```

The file is gitignored precisely because it carries real credentials; it
is not in the repo, and an rsync deploy will not overwrite it unless you
explicitly tell it to.

**4. Validate before touching anything live.**

```sh
docker compose config -q && echo OK
```

If this fails, restore the backup and stop.

**5. Recreate the three long-running services.** This is what applies
the new password — the entrypoint rewrites `default-user.xml` as
`clickhouse` comes up.

```sh
docker compose up -d clickhouse api ingest
```

`clickhouse-migrate` and the other one-shots will run and exit cleanly
as dependencies; that is expected, and is why step 3 had to update their
copy too.

**6. Verify.** All four must pass:

```sh
# a) the OLD password is now rejected -- read it from the backup rather
#    than retyping it anywhere
OLD=$(grep -m1 -E '^ +CLICKHOUSE_PASSWORD: ' docker-compose.override.yml.bak.* \
      | sed -E 's/.*: *"?([^"]*)"?$/\1/')
docker exec cairnobs-clickhouse clickhouse-client --user default \
  --password "$OLD" --query "SELECT 1" && echo "FAILED: old still works"

# b) the new password is accepted
docker exec cairnobs-clickhouse clickhouse-client --user default \
  --password "$NEW" --query "SELECT 1"

# c) the app can still read logs, and the count climbs on a live box
docker exec cairnobs-clickhouse clickhouse-client \
  --query "SELECT count() FROM cairnobs.logs"

# d) no auth failures since the restart
docker compose logs --since 5m api ingest | grep -icE '403|auth.*fail|denied'
```

If (b) passes but (c) or (d) fail, the override still holds the old value
somewhere — recheck every service block from step 3.

## Rollback

Restore the backup from step 1 and recreate the same three services:

```sh
cp -p docker-compose.override.yml.bak.<timestamp> docker-compose.override.yml
docker compose config -q && docker compose up -d clickhouse api ingest
```

Nothing else has to be undone. The rotation changes no schema and no
data — `/var/lib/clickhouse` is a volume and is never rewritten by this
procedure — so rollback is symmetric and carries no data-loss risk.

## Verification status

This procedure was executed end-to-end against `proto.cairnobs.org` on
2026-08-23: old password confirmed rejected, new one accepted,
`default-user.xml` mtime matching the new `StartedAt`, zero auth
failures, and the ingest row count still climbing afterwards. No
downtime beyond the recreate of `clickhouse`, `api`, and `ingest`.

An earlier draft of this runbook had it backwards — it opened with
`ALTER USER` and claimed the entrypoint only reads `CLICKHOUSE_PASSWORD`
at volume-init. Both were wrong, and `/opt/sentry`'s own
`docker-compose.override.yml` header had already recorded the correct
mechanism during the 2026-08-19 rotation. Check that file before
trusting anything here.

## Known gap

The compose path keeps the password in the container environment, where
`docker inspect` exposes it to anyone with Docker access — which on
these boxes is root-equivalent anyway. The Helm path already avoids this
via `secretKeyRef`. Closing it for compose would need a
`CLICKHOUSE_PASSWORD_FILE` variant in the config loaders
(`api/internal/config`, `ingest/internal/config`); only `TLS_*_FILE`
paths support file-sourced values today. Worth doing only if these hosts
ever gain more than single-operator access.
