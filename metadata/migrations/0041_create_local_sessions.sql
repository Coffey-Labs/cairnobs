-- Opaque, revocable session tokens for local login -- only the SHA-256
-- hash of the token is stored, same posture as 0034's
-- ingest_credentials: the server never needs the raw value back, only
-- "does this match." role/tenant_id are a snapshot taken at login, not
-- a live join to tenant_memberships on every request -- api/localauth's
-- Authorizer runs on every HTTP request, so this keeps that path to one
-- indexed lookup. Consequence: a role change doesn't take effect for an
-- existing session until it's revoked/expires and the user logs in
-- again -- api/localauth.Store.SetPasswordHash deliberately revokes a
-- user's sessions for exactly this reason.
CREATE TABLE IF NOT EXISTS local_sessions
(
    id         UUID PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id),
    role       TEXT NOT NULL CHECK (role IN ('viewer', 'editor', 'admin', 'owner')),
    token_hash TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
)
