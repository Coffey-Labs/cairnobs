#!/usr/bin/env bash
# Nightly reset for the demo.cairnobs.org stack: wipes every data volume
# and re-seeds from scratch, so the demo's timestamps stay recent, its
# incidents stay at the same recent offsets, and storage doesn't grow
# forever. Mirrors the teardown docs/phase-0-runbook.md and
# docs/phase-2-runbook.md already document (`docker compose down -v`),
# plus the seed sequence below.
#
# Seeding is data-first, config-second: dashboards and alert rules are
# applied after the backfill exists, so nothing renders an empty panel or
# evaluates against an empty table on its first pass.
#
# The live half of the demo (/hack/demo-simulator running as
# cairnobs-demo-simulator.service) is stopped for the duration and
# started again at the end -- it must not be pushing records into a stack
# that's being torn down, and it must re-register its agents against the
# fresh, empty `agents` table afterwards.
set -euo pipefail

DEMO_ROOT=${DEMO_ROOT:-/home/john/cairnobs-demo}
SEED_DIR="$DEMO_ROOT/hack/demo-seed"
CERTS="$DEMO_ROOT/hack/dev-certs/out"
BACKFILL=${BACKFILL:-168h}
RATE_SCALE=${RATE_SCALE:-0.5}
SIMULATOR_UNIT=cairnobs-demo-simulator.service

cd "$DEMO_ROOT"

ADMIN_PASSWORD_FILE="$DEMO_ROOT/.admin-password"
# Changing DEMO_PASSWORD means rebuilding the web image too: the login
# page prefills this account's credentials, and they're baked into the
# bundle at build time from VITE_DEMO_USERNAME/VITE_DEMO_PASSWORD in the
# demo host's docker-compose.override.yml. Change one without the other
# and the demo's own login form stops working.
DEMO_PASSWORD='CairnDemo_2026!'
# Generated fresh every run, never committed. This account exists only
# to mint the ALERTING_SERVICE_TOKEN a few lines below, and `docker
# compose down -v` above has already destroyed the previous one, so the
# value never needs to outlive a single reset -- there is nothing to
# remember and therefore no reason to hardcode it. It used to be a
# literal in this file, which put a working service-account password in
# the repo: anyone who could read the source could mint an
# alerting-evaluator token against the live demo at will, and rotating
# the token achieved nothing while the password that mints it stayed
# published.
#
# Unlike DEMO_PASSWORD above, this one is never shown to a user and is
# not baked into the web bundle, so randomising it breaks nothing.
EVALUATOR_PASSWORD="$(openssl rand -base64 24)"

echo "=== $(date -u +%FT%TZ) reset starting ==="

sudo systemctl stop "$SIMULATOR_UNIT" || true

docker compose down -v
docker compose up -d
echo "waiting for api to report healthy..."
until [ "$(docker inspect -f '{{.State.Health.Status}}' cairnobs-api 2>/dev/null)" = "healthy" ]; do sleep 2; done

# -seed-admin is idempotent and prints the password once -- capture it
# fresh each run rather than reusing a stale one from a prior reset.
ADMIN_PASSWORD=$(docker compose run --rm api -seed-admin 2>&1 | grep '^  password:' | awk '{print $2}')
echo "$ADMIN_PASSWORD" > "$ADMIN_PASSWORD_FILE"
chmod 600 "$ADMIN_PASSWORD_FILE"

export CAIRNOBSCTL_API_URL=http://localhost:8080
export CAIRNOBSCTL_ALERTING_API_URL=http://localhost:8081
ADMIN_TOKEN=$(echo "$ADMIN_PASSWORD" | ./bin/cairnobsctl users login admin)
export CAIRNOBSCTL_TOKEN="$ADMIN_TOKEN"

# The password reaches the CLI on stdin only -- `--password <value>` was
# removed deliberately (see cli/cmd/cairnobsctl/cmd_users.go).
echo "$DEMO_PASSWORD" | ./bin/cairnobsctl users create demo --role viewer >/dev/null
echo "$EVALUATOR_PASSWORD" | ./bin/cairnobsctl users create alerting-evaluator --role viewer >/dev/null

SVCTOKEN=$(echo "$EVALUATOR_PASSWORD" | ./bin/cairnobsctl users login alerting-evaluator)
printf 'COMPOSE_PROFILES=single-tenant\nALERTING_SERVICE_TOKEN=%s\n' "$SVCTOKEN" > .env && chmod 600 .env
docker compose up -d alerting

# Three notification targets so the Alerts page shows rules routed to
# different destinations, the way a real deployment splits ops/security/
# platform. The URLs are deliberately inert placeholders on a domain
# reserved for documentation -- nothing is actually notified.
create_target() {
  curl -s -X POST http://localhost:8081/targets \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$1\",\"kind\":\"webhook\",\"webhook_url\":\"$2\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}
TARGET_OPS=$(create_target 'Ops on-call (placeholder)' 'https://example.com/webhooks/ops-oncall')
TARGET_SECURITY=$(create_target 'Security team (placeholder)' 'https://example.com/webhooks/security')
TARGET_PLATFORM=$(create_target 'Platform team (placeholder)' 'https://example.com/webhooks/platform')

echo "backfilling $BACKFILL of synthetic history..."
./bin/demo-simulator \
  -addr 127.0.0.1:4317 -ca "$CERTS/ca.pem" -cert "$CERTS/client.pem" -key "$CERTS/client-key.pem" \
  -backfill "$BACKFILL" -rate-scale "$RATE_SCALE" -live=false

# A handful of Windows-shaped records from the dedicated fixture as well:
# it's the tool the Windows ingest path is actually verified with, so
# keeping its output present means the demo and that check agree.
docker run --rm --network host -v "$DEMO_ROOT":/src -w /src/hack/windows-fixture -e GOCACHE=/tmp/gocache golang:1.25-bookworm \
  go run . --addr 127.0.0.1:4317 --ca /src/hack/dev-certs/out/ca.pem --cert /src/hack/dev-certs/out/client.pem --key /src/hack/dev-certs/out/client-key.pem --count 5

echo "applying dashboards..."
for f in "$SEED_DIR"/dashboards/*.json; do
  ./bin/cairnobsctl dashboards apply "$f" >/dev/null
  echo "  $(basename "$f")"
done

echo "applying alert rules..."
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
for f in "$SEED_DIR"/alerts/*.json.template; do
  out="$TMP/$(basename "${f%.template}")"
  sed -e "s/__TARGET_OPS__/$TARGET_OPS/" \
      -e "s/__TARGET_SECURITY__/$TARGET_SECURITY/" \
      -e "s/__TARGET_PLATFORM__/$TARGET_PLATFORM/" "$f" > "$out"
  ./bin/cairnobsctl alerts apply "$out" >/dev/null
  echo "  $(basename "$out")"
done

sudo systemctl start "$SIMULATOR_UNIT"

echo "=== $(date -u +%FT%TZ) reset complete ==="
