-- Per-tenant bearer credentials an agent presents to `ingest` (see
-- ingest/internal/grpcserver's TenantResolver) so a record can be
-- attributed to a tenant at the point it enters the system, rather than
-- landing in the one shared ClickHouse database/Tantivy index every
-- record lands in today. Only the SHA-256 hash of the token is stored --
-- same reasoning a password gets hashed, not stored raw: enterprise-auth
-- only ever needs to check "does the presented token match," never to
-- recover the plaintext, so there's no reason to keep it recoverable.
-- Losing the plaintext means issuing a new credential, not resetting
-- this one -- the plaintext is returned exactly once, at creation.
CREATE TABLE IF NOT EXISTS ingest_credentials
(
    id         UUID PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)
