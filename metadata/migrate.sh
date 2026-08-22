#!/usr/bin/env bash
# Applies migrations/*.sql to Postgres in filename order, tracking what's
# already been applied in a schema_migrations table -- same shape as
# /storage/migrate.sh, adapted for psql instead of curl since Postgres
# supports real transactions per file (kept to one DDL object per file
# anyway, for repo-wide consistency of what a migration "version" means).
set -euo pipefail

POSTGRES_HOST="${POSTGRES_HOST:-localhost}"
POSTGRES_PORT="${POSTGRES_PORT:-5432}"
POSTGRES_USER="${POSTGRES_USER:-cairnobs}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-}"
POSTGRES_DATABASE="${POSTGRES_DATABASE:-cairnobs_metadata}"
# Password for the restricted audit-log-writer Postgres role (Phase 4
# task 4, see /docs/phase-4-isolation-design.md's audit logging
# section) -- a second, narrower-granted role, not the shared
# POSTGRES_PASSWORD above. Passed to psql via -v so the migration SQL
# file can reference it as :'audit_writer_password' without ever
# hardcoding a credential in a file checked into git.
AUDIT_WRITER_PASSWORD="${AUDIT_WRITER_PASSWORD:-audit-writer-dev-only}"

export PGPASSWORD="$POSTGRES_PASSWORD"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATIONS_DIR="${SCRIPT_DIR}/migrations"

psql_exec() {
    psql -v ON_ERROR_STOP=1 -X -q -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" -U "$POSTGRES_USER" -d "$POSTGRES_DATABASE" \
        -v audit_writer_password="$AUDIT_WRITER_PASSWORD" "$@"
}

echo "Ensuring schema_migrations table exists..."
psql_exec -c "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"

applied="$(psql_exec -t -A -c "SELECT version FROM schema_migrations")"

shopt -s nullglob
for file in "${MIGRATIONS_DIR}"/*.sql; do
    version="$(basename "$file")"
    if grep -qx "$version" <<< "$applied"; then
        echo "skip  ${version} (already applied)"
        continue
    fi
    echo "apply ${version}"
    psql_exec -f "$file"
    psql_exec -c "INSERT INTO schema_migrations (version) VALUES ('${version}')"
done

echo "Migrations complete."
