-- Local username/password login (single-tenant deployments with no
-- enterprise-auth/SSO configured) -- see api/localauth's package doc
-- comment. Reuses 0017's users table and 0020's tenant_memberships
-- rather than a parallel identity model, so a deployment that later
-- turns on enterprise SSO has one identity graph, not two to reconcile.
--
-- email relaxed to nullable: a local user has no SSO identity to hang
-- an email off of. enterprise/internal/rbacstore.UpsertUserBySSO
-- already requires a non-empty email in Go before ever inserting, so
-- this is a no-op for the SSO path.
ALTER TABLE users
    ALTER COLUMN email DROP NOT NULL;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS username TEXT UNIQUE;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS password_hash TEXT;
