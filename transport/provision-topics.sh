#!/usr/bin/env bash
# Idempotently creates the topic ingest produces/consumes. Talks to
# Redpanda over the network via `rpk`, not `docker exec` into the broker
# container -- this way the same script works whether it's run from the
# host (against the standalone compose in this directory), from inside a
# sibling container on the root compose's network, or in CI.
set -euo pipefail

BROKERS="${REDPANDA_BROKERS:-localhost:9092}"
TOPIC="${REDPANDA_TOPIC:-sentry.logs.raw}"
PARTITIONS="${REDPANDA_TOPIC_PARTITIONS:-6}"

echo "Waiting for Redpanda at ${BROKERS}..."
until rpk cluster health --brokers "${BROKERS}" --exit-when-healthy > /dev/null 2>&1; do
    sleep 1
done

if rpk topic list --brokers "${BROKERS}" | awk 'NR>1{print $1}' | grep -qx "${TOPIC}"; then
    echo "Topic '${TOPIC}' already exists, skipping."
else
    echo "Creating topic '${TOPIC}' (${PARTITIONS} partitions)..."
    rpk topic create "${TOPIC}" --brokers "${BROKERS}" --partitions "${PARTITIONS}" --replicas 1
fi
