CREATE TABLE IF NOT EXISTS users
(
    id           UUID PRIMARY KEY,
    email        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    -- Set on first successful SSO login (OIDC "sub" or SAML NameID) --
    -- nullable because a user row can exist before their first login in
    -- principle (e.g. pre-provisioned by an Admin), though Phase 4's
    -- baseline flow always creates the row and the SSO subject together.
    sso_subject  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
)
