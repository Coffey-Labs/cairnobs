# RBAC design

> **Status:** Design, awaiting sign-off. Task 3 of Phase 4 — stop here
> before implementing enforcement/store code (`internal/rbacstore`,
> the `internal/session` middleware, RBAC checks wired into `/api`,
> `/alerting`, `/web`), per explicit instruction, same discipline as
> `/docs/phase-4-isolation-design.md`. Read that document first — this
> one assumes its module-placement decision (isolation and RBAC
> mechanisms live in `enterprise/` only) and its `alerting`↔`api`
> service-identity finding, which this doc's role model has to account
> for as a distinct category, not a variant of human roles.

## Why this design, in one paragraph

A plain three-tier viewer/editor/admin model (the original ask) has two
real gaps once you actually walk through who does what: nothing prevents
admins from locking each other out of a tenant entirely (the last admin
demotes themselves, or two admins race to remove each other — a shape of
bug organizations with real access-control systems hit often enough that
GitHub/GitLab-style products all converge on the same fix), and nothing
answers "who can grant additional access to a specific resource" — if
any editor can, an editor can silently self-escalate. This design adds a
non-removable `Owner` above `Admin` for the first gap, and scopes
per-resource grant management to resource creators + admins/owner for
the second, with every grant change itself audit-logged (task 4) — the
exact thing a security reviewer asks "show me every time access
widened" about. It also formalizes `alerting`'s evaluator as a distinct
**service identity** category, not a point on the human role scale,
because task 2's design doc found it needs categorically narrower
authority ("run this one already-persisted rule's query," never general
tenant browsing) than even a Viewer has.

## Roles

**Owner** → **Admin** → **Editor** → **Viewer**, strictly ordered — each
role's baseline access is a superset of the one below it. Exactly one
Owner per tenant at a time; Owner is non-removable and non-demotable
except by itself (voluntary transfer) or a platform operator
(break-glass, itself audit-logged and out of normal tenant-admin
control). This is the answer to "can admins lock each other out": they
can't lock out the Owner, and the Owner can always recover.

Plus **additive-only per-resource grants** — a `dashboard_permissions`
row lets a specific user exceed their baseline tenant role on one
specific dashboard (e.g. a Viewer given Editor-level access to one
dashboard they need to maintain, without making them a tenant-wide
Editor). No deny-overrides: a grant can never take away access someone's
baseline role already has. Deny-overrides (restricting a specific
Editor from a specific sensitive dashboard, say) are named future work,
not solved here — the additive-only model is simpler to reason about
and implement correctly, and covers the more common real need ("let this
one person help with this one thing").

Plus a distinct **service identity** category, not on the role scale at
all: `alerting`'s evaluator authenticates as itself (a service
credential, task 5), authorized narrowly to "execute the query belonging
to already-persisted rule X, where X's tenant is resolved server-side" —
never general read access to a tenant's dashboards, users, or anything
else. A compromised or buggy evaluator can re-run known rule queries; it
cannot browse.

## Permission matrix

| Action | Viewer | Editor | Admin | Owner |
|---|---|---|---|---|
| View dashboard (baseline or granted access) | ✓ | ✓ | ✓ | ✓ |
| Create dashboard | ✗ | ✓ | ✓ | ✓ |
| Edit/delete dashboard | ✗ | ✓ (own, or granted) | ✓ (any) | ✓ |
| Manage a dashboard's per-user grants | ✗ | ✓ (only if creator) | ✓ | ✓ |
| Run ad hoc query (UI/CLI/API) | ✓ | ✓ | ✓ | ✓ |
| Create/edit/delete alert rule | ✗ | ✓ | ✓ | ✓ |
| Enable/disable alert rule | ✗ | ✓ | ✓ | ✓ |
| View alert delivery log | ✓ | ✓ | ✓ | ✓ |
| Create/edit notification target | ✗ | ✓ | ✓ | ✓ |
| View notification target secret (webhook URL/token) | ✗ | ✗ | ✓ | ✓ |
| Delete notification target | ✗ | ✗ | ✓ | ✓ |
| View data source config | ✓ | ✓ | ✓ | ✓ |
| Manage tenant users / role assignments | ✗ | ✗ | ✓ (not Owner) | ✓ |
| Manage SSO config | ✗ | ✗ | ✓ | ✓ |
| View tenant audit log | own history only | own history only | ✓ | ✓ |
| Transfer tenant Owner | ✗ | ✗ | ✗ | ✓ (or platform break-glass) |

Every row in the "Admin/Owner-only" section that changes *someone else's*
access (role assignment, grant management, SSO config, Owner transfer) is
written to the audit log (task 4) as its own event type, distinct from a
query execution — reviewers ask for this list specifically.

Editors intentionally cannot see notification target secrets (webhook
URLs/tokens often encode credentials) even though they can select and
use a target when creating a rule — this mirrors the existing plaintext-
secret disclosure in `/docs/phase-3-alerting-design.md`'s "Known gaps":
Phase 3 already flagged `notification_targets.secret` as stored
plaintext; RBAC narrows *who can read it back*, it doesn't change how
it's stored (that's still named future hardening work, not solved here).

## `data_sources`: the honest scope of "per-data-source scoping"

Today, and through the end of this phase, every tenant has exactly one
data source: their own ClickHouse database + Tantivy index pair from
`/docs/phase-4-isolation-design.md`. A `data_sources` table (tenant-
scoped, one row auto-created per tenant at provisioning time) exists as
the extension point for a real future multi-source-per-tenant feature —
e.g. a tenant connecting a second ClickHouse cluster, or a distinct log
stream with its own retention. Role grants can reference a
`data_source_id` in the schema now, but with one data source per tenant
there is nothing meaningfully different a per-data-source grant does
yet. Stated plainly so this doesn't read as more built than it is.

## Schema

Lives in `/metadata` (`sentry_metadata`), alongside everything else from
Phase 3, per `/docs/phase-4-isolation-design.md`'s existing schema
additions (`tenants`, the `tenant_id` backfill on `alert_state`/
`delivery_log`). New tables, continuing that migration sequence:

```sql
CREATE TABLE users (
    id            UUID PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    -- Identity provenance, not a password -- SSO is the only login path.
    -- One user row can in principle federate from either an OIDC or a
    -- SAML IdP; which one isn't fixed at the user level, it's determined
    -- per tenant_memberships row via the tenant's configured SSO method.
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tenant_memberships (
    tenant_id  TEXT NOT NULL REFERENCES tenants(id),
    user_id    UUID NOT NULL REFERENCES users(id),
    role       TEXT NOT NULL CHECK (role IN ('viewer', 'editor', 'admin', 'owner')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);
-- Application-level invariant (not a DB constraint -- Postgres has no
-- native "exactly one row matching a predicate per group" check):
-- exactly one 'owner' row per tenant_id at a time. Enforced in
-- internal/rbacstore's transfer/provisioning logic, not the schema.

CREATE TABLE data_sources (
    id                     UUID PRIMARY KEY,
    tenant_id              TEXT NOT NULL REFERENCES tenants(id),
    name                   TEXT NOT NULL DEFAULT 'default',
    clickhouse_database_name TEXT NOT NULL,
    tantivy_index_path       TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE dashboard_permissions (
    id            UUID PRIMARY KEY,
    dashboard_id  UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    user_id       UUID NOT NULL REFERENCES users(id),
    -- Additive only: this grant can only raise access above the user's
    -- tenant-wide baseline role for this one dashboard, never lower it.
    permission    TEXT NOT NULL CHECK (permission IN ('viewer', 'editor')),
    granted_by    UUID NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dashboard_id, user_id)
);
```

`alert_rules`/`notification_targets`/`dashboards` already carry
`tenant_id` (Phase 3); no per-row ownership beyond `created_by` (already
present) is needed for the matrix above — "own vs. any" in the matrix is
`created_by = current user` vs. tenant-wide Admin/Owner authority, not a
separate grants table for those resource types.

**Implementation note (Phase 4 task 5, added after this design was
signed off):** `dashboard_permissions` is now built --
`enterprise/internal/rbacstore`'s CRUD plus a `DashboardPermissions`
adapter implementing a new core interface, `api/dashboards.
PermissionStore`, enforced in `api/dashboards`' handler. The applied
migration (`metadata/migrations/0024_create_dashboard_permissions.sql`)
diverged slightly from the schema above -- it allowed `role='admin'`
and left `granted_by` nullable -- and was reconciled to match this
document via `0033_restrict_dashboard_permissions_role.sql`, found
while wiring the enforcement code up. See `enterprise/README.md` and
`/docs/security/threat-model.md` for verification status.

## Enforcement shape (design only — implementation is task 5)

RBAC checks happen server-side, on every `/api`/`/alerting` endpoint
that touches tenant data — never UI-level button-hiding alone, per the
explicit requirement. The shape: middleware resolves `(tenant.ID, user,
role)` from a validated session (or, for `alerting`, the service
identity) before a handler runs; each handler declares the minimum role
an action requires; a request failing that check gets a 403 before any
tenant-scoped connection is even acquired — RBAC is a gate in front of
the isolation mechanism from `/docs/phase-4-isolation-design.md`, not a
replacement for it. Full middleware/handler wiring is task 5's scope.

## Web UI boundary: a runtime capability check, not a conditional import

Core `web` never bundles enterprise-licensed Svelte components into its
build — that would put commercial-licensed source inside an AGPL
artifact, the UI-layer equivalent of the Go import-boundary problem
`hack/check-tenant-boundary.sh` already guards against. Instead: core
`web` ships a generic settings/admin route
(`web/src/routes/settings/+page.svelte`, added in task 5) that, on load,
calls `GET {enterprise-auth base URL}/auth/features` and renders
sections conditionally based on the response:

```json
{"sso_configured": true, "oidc_enabled": true, "saml_enabled": false}
```

If `enterprise-auth` isn't deployed or configured, that fetch fails or
returns all-`false`, and the settings page simply shows core-only
content — no broken links, no "upgrade to unlock" dead ends, just an
absent section. This is a runtime capability check against a documented
REST contract, the same pattern `web` already uses for its two backend
base URLs (`apiBase`/`alertingBase` in `web/src/lib/api.ts`), not a new
mechanism — just pointed at a third, optional backend.

## What this document deliberately does not solve here

- Deny-override grants (named future work above).
- Enforcement middleware implementation (task 5).
- Audit logging of grant/role changes (task 4 builds the audit log
  itself; this doc only names which actions must be logged).
- SSO-to-role mapping policy (e.g. IdP group claims auto-assigning
  roles) — a real feature, not designed here; Phase 4's baseline is
  manual role assignment by an Admin/Owner after a user's first SSO
  login creates their `users` row.
