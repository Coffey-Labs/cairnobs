#!/usr/bin/env bash
# Applies migrations/*.sql to ClickHouse in filename order, tracking what's
# already been applied in a schema_migrations table. Talks to ClickHouse's
# HTTP interface via curl rather than requiring the clickhouse-client
# binary — nothing to install beyond curl, works identically on a dev
# laptop or in CI.
#
# Convention: exactly one DDL statement per migration file. The ClickHouse
# HTTP interface isn't reliably multi-statement, so keeping migrations to
# one statement each avoids relying on that.
set -euo pipefail

CLICKHOUSE_HTTP="${CLICKHOUSE_HTTP:-http://localhost:8123}"
CLICKHOUSE_USER="${CLICKHOUSE_USER:-default}"
CLICKHOUSE_PASSWORD="${CLICKHOUSE_PASSWORD:-}"
DATABASE="${CLICKHOUSE_DATABASE:-sentry}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATIONS_DIR="${SCRIPT_DIR}/migrations"

ch_exec() {
    # $1 = SQL statement, $2 = optional database to scope the query to.
    local sql="$1"
    local db="${2:-}"
    local url="${CLICKHOUSE_HTTP}/"
    if [[ -n "$db" ]]; then
        url="${CLICKHOUSE_HTTP}/?database=${db}"
    fi
    curl -sS -f -u "${CLICKHOUSE_USER}:${CLICKHOUSE_PASSWORD}" "$url" --data-binary "$sql"
}

echo "Ensuring database '${DATABASE}' exists..."
ch_exec "CREATE DATABASE IF NOT EXISTS ${DATABASE}"

echo "Ensuring schema_migrations table exists..."
ch_exec "CREATE TABLE IF NOT EXISTS schema_migrations (version String, applied_at DateTime DEFAULT now()) ENGINE = MergeTree ORDER BY version" "$DATABASE"

applied="$(ch_exec "SELECT version FROM schema_migrations FORMAT TabSeparated" "$DATABASE")"

shopt -s nullglob
for file in "${MIGRATIONS_DIR}"/*.sql; do
    version="$(basename "$file")"
    if grep -qx "$version" <<< "$applied"; then
        echo "skip  ${version} (already applied)"
        continue
    fi
    echo "apply ${version}"
    ch_exec "$(cat "$file")" "$DATABASE" > /dev/null
    ch_exec "INSERT INTO schema_migrations (version) VALUES ('${version}')" "$DATABASE" > /dev/null
done

echo "Migrations complete."
