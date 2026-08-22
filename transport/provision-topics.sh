#!/usr/bin/env bash
# Idempotently creates the topic ingest produces/consumes. Talks to
# Redpanda over the network via `rpk`, not `docker exec` into the broker
# container -- this way the same script works whether it's run from the
# host (against the standalone compose in this directory), from inside a
# sibling container on the root compose's network, or in CI.
set -euo pipefail

# NOTE: confirmed by actually running this against a live Redpanda
# container -- neither `rpk cluster health` nor `rpk topic ...` accept a
# `--brokers` flag in this rpk version. Health checks hit the Admin API
# (-X admin.hosts=..., port 9644); topic commands hit the Kafka API
# (-X brokers=..., port 9092). Getting this wrong doesn't error loudly:
# `rpk cluster health --brokers ...` fails with "unknown flag" but that
# failure was swallowed by this script's own `> /dev/null 2>&1` retry
# loop, which just silently retried the malformed command forever instead
# of ever becoming healthy.
BROKERS="${REDPANDA_BROKERS:-localhost:9092}"
ADMIN_HOSTS="${REDPANDA_ADMIN_HOSTS:-localhost:9644}"
TOPIC="${REDPANDA_TOPIC:-cairnobs.logs.raw}"
PARTITIONS="${REDPANDA_TOPIC_PARTITIONS:-6}"

echo "Waiting for Redpanda admin API at ${ADMIN_HOSTS}..."
until rpk cluster health -X "admin.hosts=${ADMIN_HOSTS}" --exit-when-healthy > /dev/null 2>&1; do
    sleep 1
done

if rpk topic list -X "brokers=${BROKERS}" | awk 'NR>1{print $1}' | grep -qx "${TOPIC}"; then
    echo "Topic '${TOPIC}' already exists, skipping."
else
    echo "Creating topic '${TOPIC}' (${PARTITIONS} partitions)..."
    rpk topic create "${TOPIC}" -X "brokers=${BROKERS}" --partitions "${PARTITIONS}" --replicas 1
fi
