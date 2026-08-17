# Tenant isolation design

> **Status:** Design, awaiting sign-off. Task 2 of Phase 4 — the highest-
> risk decision in the project so far, per explicit instruction: stop
> here before any code is written. This document was pressure-tested by
> an adversarial design review before being written up (not just
> reasoned through once and accepted) — several of the "required design
> elements" below exist specifically because that review found concrete
> bypass scenarios in an earlier draft, not because they're generically
> prudent. If implementation reveals this design is wrong somewhere, fix
> this doc in the same change — same discipline as every prior phase's
> design docs.

## Why this design, in one paragraph

Phases 0–3 are entirely single-tenant with zero authentication anywhere.
Phase 4 needs to isolate tenant data with a security-review-credible
guarantee, and the central fact shaping everything below is that Phase
2's query language has a raw-SQL escape hatch that is deliberately
*opaque* — never parsed, never validated against a schema
(`/docs/query-language-design.md`). That single fact rules out row-level
filtering (a `tenant_id` column plus a compiler-injected `WHERE` clause)
as the *sole* isolation mechanism: a filter the compiler injects
categorically cannot apply to a query the compiler never parses. So the
real isolation boundary has to live one layer down, at the database
connection itself — a tenant's ClickHouse user simply has no grant to
read another tenant's database, and no application code, compiled query
or hand-written SQL, can change that. Everything else in this document
is either implementing that connection-layer boundary correctly or
closing a gap the adversarial review found in a naive version of it.

## Module placement: `enterprise/` only, confirmed with the project owner

The tenant-isolation mechanism described here — per-tenant ClickHouse
database/user, per-tenant Tantivy index, the `TenantID` plumbing —
ships entirely in `enterprise/`, not AGPL core. Core (`/api`,
`/alerting`, `/web`) stays genuinely single-tenant: no multi-tenant
mechanism present at all, not merely a missing management UI on top of
otherwise-functional isolation. This was an explicit choice put to the
project owner rather than assumed, because CLAUDE.md's licensing
boundary text names multi-tenancy as enterprise-gated, and a
"mechanism in core, feature in enterprise" split would have let a
sufficiently motivated self-hosting AGPL user wire up real isolation
without ever touching `enterprise/` — undermining that boundary in
substance even while technically respecting the import graph (at the
time, an AGPL/commercial split; as of Phase 6, `enterprise/` is AGPLv3
too, so the import graph is now the whole reason this boundary exists,
not a proxy for a licensing one — see
`/docs/compliance/license-audit-report.md`). Confirmed: enterprise-only.

Mechanically, this works because `api/internal/querylang/executor`
already defines the seam Phase 2 needs regardless of tenancy:

```go
type SQLRunner interface {
    RunSQL(ctx context.Context, sql string) (*Result, error)
}
type SearchClient interface {
    Search(ctx context.Context, query string, limit uint32) ([]string, error)
}
```

`enterprise/` supplies tenant-scoped implementations of these same
core-defined interfaces — Go interfaces don't require an import edge
from core to enterprise, only enterprise importing core's *interface
types*, the allowed direction. Core's `querylang`/`executor` packages
need zero changes for tenancy; `ChRunner` (today's single-connection
implementation, `api/internal/querylang/executor/chrunner.go`) stays
exactly as it is for single-tenant deployments, and `enterprise/`
provides an alternate implementation for multi-tenant ones.

## A framing correction, stated plainly rather than built around

The original task language asks for "compile-time... structurally
impossible to bypass" enforcement in the query compiler. Given the
raw-SQL passthrough above, that specific phrasing isn't achievable in
any module — there's no parse step to inject a filter into. The
achievable, honest version: **every code path, compiled query or raw
SQL, is forced through a tenant-scoped connection that the database's
own access-control system enforces.** The structural guarantee is at
the connection/index layer, not the compiler layer. This document's
"required design elements" are what make that connection-layer
guarantee actually hold under concurrency, partial failure, and
ClickHouse's own default-permissive corners — not decoration on top of
an already-sufficient row filter.

## ClickHouse: database-per-tenant, grant-enforced

One ClickHouse database + one dedicated, narrowly-granted ClickHouse
user per tenant, on the shared cluster by default. A tenant can later be
pinned to dedicated cluster nodes (Phase 4 task 6, a deployment-topology
decision) for large/regulated customers — that changes *where* a
tenant's database physically runs, not this model. `enterprise/` holds a
small map of per-tenant `*ChRunner`s — today's `chrunner.go` shape (one
`driver.Conn`, `Auth.Database` fixed at construction) is already exactly
right, this just needs N of them instead of one. **No `tenant_id`
column on `logs`**: isolation is a connection-level property, so there
is nothing else in a tenant's own database to filter or leak through a
missed `WHERE` clause.

### Required design elements

Each of these closes a specific bypass the adversarial review found in
a naive version of "just give each tenant a database":

**1. No tenant traffic ever authenticates as ClickHouse's `default`
user.** Today's `docker-compose.yml` sets `CLICKHOUSE_PASSWORD` on the
implicit `default` user for the whole stack — a Phase 0–3-appropriate
shortcut that must not carry into tenant-scoped connections. `default`
(or an equivalent broad-access account) is reserved for
migrations/provisioning/ops only, never handed to a request-serving
code path.

**2. `system.*` access is explicitly revoked from every tenant user,
not left at whatever ClickHouse's default template grants.**
`system.query_log` records every query's full text by default, and is
broadly readable unless explicitly revoked — so even with per-database
row isolation working *perfectly*, a tenant able to read
`system.query_log` can see other tenants' query text: predicate values,
field names, sometimes literally sensitive data embedded in a `WHERE`
clause. `SHOW DATABASES`/`system.tables` visibility being properly
grant-scoped is version-dependent on `access_management` actually being
engaged for the account, not the default `users.xml`-style setup. This
is not assumed from documentation — it's verified by an adversarial
integration test (Phase 4 task 8) that, as a tenant-scoped user,
attempts `SELECT * FROM system.query_log`, `SELECT * FROM
system.tables`, `SHOW DATABASES`, and a fully-qualified cross-tenant
`SELECT * FROM <other_tenant_db>.logs`, asserting each is denied or
empty. Provisioning (`enterprise/internal/tenantprovision`) explicitly
revokes/never-grants `system.*` as part of creating a tenant user.

**3. Per-tenant connections are fully separate `driver.Conn`/pool
objects — never one shared pool with session-level `USE tenant_x`.** A
shared-pool-plus-`USE` implementation is a real concurrency bug, not a
theoretical one: a connection recycled between tenants mid-flight can
interleave a `USE` statement for tenant A with a query that actually
executes against tenant B's still-live session state, depending on how
`clickhouse-go/v2` recycles connections under load. This design
mandates N fully separate pools, resolved fresh per request — as a
local variable inside the request-handling goroutine, never cached in a
mutable struct field shared across goroutines — from an
immutable-after-startup `map[TenantID]*ChRunner`. Growing or shrinking
that map (tenant on/offboarding) happens by replacing the map wholesale
(copy-on-write), never by mutating it in place under concurrent readers.

**4. Provisioning is ordered, idempotent, and gated on an explicit
`active` state.** Sequence: `CREATE USER IF NOT EXISTS` with a
zero-privilege base role (not ClickHouse's implicit default profile) →
`GRANT` narrow, tenant-database-scoped access → only *then* mark the
tenant `active` in the `tenants` table (Postgres, `/metadata`). Every
tenant-resolution code path refuses to serve a tenant not in `active`
state, checked against that table server-side — never inferred from "a
connection happened to succeed," which would happily serve traffic
during a half-finished provisioning run. A crashed/retried provisioning
job must not leave a *broader*-than-intended grant live during the
retry window; the ordering above (narrow grant strictly before
`active`) is what prevents that. Deprovisioning must not leave a live
cached connection usable past a revoked grant on some ClickHouse
versions dropping a user doesn't terminate already-open sessions — so
offboarding either explicitly terminates sessions for the tenant's user
or relies on a bounded max lifetime for cached per-tenant connections
(not indefinite reuse).

## Tantivy: index-per-tenant

Same underlying reasoning as ClickHouse, for a sharper reason: Tantivy
has no grant system at all, so "one shared index with a tenant-tagged
field, filtered at query time" would have *zero* structural backing —
purely conventional, exactly the "convention, not structural" failure
mode this whole design exists to avoid. `search`'s current shape (one
`Arc<SearchIndex>` opened once at startup — `search/src/index.rs`,
`search/src/main.rs`) becomes a registry: a `HashMap<TenantID,
Arc<SearchIndex>>` (an LRU if tenant count ever grows large enough that
holding every index open simultaneously is wasteful — not needed for
Phase 4's initial scale), each index a separate directory under a
shared volume, opened or created on demand and resolved only from the
tenant context `enterprise/`'s trusted caller establishes.
`search.proto`'s `SearchRequest` gains a tenant field, populated
exclusively by `enterprise/`'s tenant-scoped `SearchClient`
implementation — never read from anything a remote/external client
supplies.

## The `alerting` ↔ `api` gap

Found by the adversarial review, not present in the first draft — real,
not hypothetical, and it has to be resolved as part of this sign-off
because it shapes Phase 4 task 5's design directly.

**Today's actual behavior**: `alerting`'s evaluator
(`alerting/internal/evaluator/evaluator.go`) claims due rules across
*all* tenants in a single `rulestore.ClaimDueRules` call, then for each
one calls `api`'s `POST /query` via `internal/queryclient/client.go`
with just `{query, language}` — no tenant field, no authentication, at
all, today.

**Why this matters for isolation specifically**: `alerting` is a
machine calling on a schedule, not a human with a session — there is no
session to derive a tenant context from the way a browser request has
one. The tempting, wrong fix is adding a `tenant_id` field to the
`/query` request, populated from the rule's own `TenantID` (which
`rulestore.RuleWithState` already carries after Phase 4's schema
additions). That is *exactly* the client-suppliable tenant identifier
this entire design exists to prevent — `alerting`'s HTTP surface is
unauthenticated today, so anything able to reach it (or spoof a call to
`api` shaped like one) could request any tenant's data by setting that
field.

**The correct fix**, scoped into Phase 4 task 5: a distinct **service
identity** for `alerting` — a signed service token or mTLS client
certificate, not a human session — that `api`/`enterprise/` map to "may
execute the query belonging to rule X," where rule X's tenant is looked
up **server-side** from `alert_rules.tenant_id` (already present after
this phase's schema work), never taken from anything in the request
body. This is a third RBAC category, alongside human roles (Phase 4
task 3), not a variant of session/token handling — it authorizes "run
this one already-persisted, already-tenant-scoped rule," never general
tenant access, so a compromised evaluator can't be used to browse
arbitrary tenant data.

## `TenantID`: an honest framing, not an oversold one

```go
package tenant

type contextKey struct{}       // unexported key type -- closes a
                                // context.WithValue collision gap: an
                                // exported or string-typed key could be
                                // shadowed/overwritten by unrelated code
type ID struct{ value string } // unexported field

func (id ID) String() string { return id.value }
func FromContext(ctx context.Context) (ID, bool)
func WithContext(ctx context.Context, id ID) context.Context

// TrustFromValidatedSession is the only production construction path
// from a raw string. DO NOT CALL OUTSIDE auth middleware -- enforced by
// CI grep (hack/check-tenant-boundary.sh), same mechanism as the
// enterprise/-import-boundary check Phase 4 task 3 adds.
func TrustFromValidatedSession(raw string) ID
```

An unexported field with exactly one production constructor makes
*accidental* misuse cheap to audit — grep for call sites — it does not
make misuse impossible by the Go compiler alone, and this document
states that plainly rather than implying otherwise. Concretely:

- It does not stop a *deliberate* second construction path added later
  inside the `tenant` package itself — e.g. a future `UnmarshalJSON`
  method, added for some unrelated serialization need, which has full
  access to the unexported field from within the package and would
  happily decode a client-supplied JSON body straight into a trusted
  `ID` the moment any handler unmarshals into a struct embedding one.
- **Correction made during implementation**: this design originally
  proposed a test-only constructor in `internal/tenant/testing_test.go`,
  reasoning that Go's exclusion of `_test.go` files from normal imports
  would make it a compiler-enforced constructor reachable by other
  packages' tests but not production code. That reasoning was wrong —
  Go never compiles `_test.go` files into what *any* other package
  imports, including other packages' own tests, so that constructor
  would have been unreachable even from its intended callers. There is
  no separate test constructor: other packages' tests call
  `TrustFromValidatedSession` directly, which is fine, since test code
  isn't attacker-controlled the way a network-facing handler is.
- The actual invariant, stated for what it is: *the constructor has
  exactly one call site in non-test production code, verified by CI
  grep (scanning `*.go`, excluding `*_test.go`) plus code review at
  every change to `internal/tenant/`. The database/index grant layer
  above is the real backstop. This package makes production violations
  visible and rare — it does not make them impossible, and was never
  able to restrict test-time construction either, only make it
  unnecessary to restrict.*

## Provisioning state machine (summary — full detail in task 6's deploy work)

```
provisioning → active → suspended → deprovisioning → (removed)
```

- `provisioning`: `tenants` row exists, ClickHouse user/grants and
  Tantivy index directory are being created. No request is ever served
  for a tenant in this state.
- `active`: fully provisioned, narrow grants confirmed applied. Normal
  serving state.
- `suspended`: grants revoked (e.g. non-payment, policy violation) but
  data retained; no requests served, distinct from `deprovisioning` so
  a suspension can be reversed without re-provisioning from scratch.
- `deprovisioning`: offboarding in progress — sessions/connections being
  terminated, data export/deletion per retention policy (out of scope
  for this document; a compliance/data-retention design, not an
  isolation one).

## What this document deliberately does not solve here

- The RBAC role model and its enforcement (Phase 4 task 3, separate
  sign-off).
- Audit logging mechanics (Phase 4 task 4).
- Exact SSO protocol flows (Phase 4 task 3).
- Deployment topology for pinning large tenants to dedicated cluster
  nodes (Phase 4 task 6) — this document's model is agnostic to where a
  tenant's database physically runs, only that it's a distinct
  database/user regardless of placement.
- Data retention/deletion semantics during deprovisioning.

## Verification plan for this design specifically

Not just unit tests — this design's real risk is in ClickHouse's actual
runtime grant behavior on the pinned version in `docker-compose.yml`,
which is exactly the kind of thing that looks fine in documentation and
isn't in practice (see item 2 above). Phase 4 task 8's adversarial
suite must include, against the live stack:

- A tenant-scoped ClickHouse user attempting to read another tenant's
  database by fully-qualified name in raw SQL.
- The same user attempting `system.query_log`, `system.tables`, `SHOW
  DATABASES`.
- A Tantivy search request for one tenant returning zero cross-tenant
  results even when another tenant's index contains matching terms.
- A simulated evaluator tick firing mid-provisioning (tenant row exists,
  grants not yet confirmed) to confirm it's refused, not silently served
  against a partially-provisioned or default-profile connection.

  **Implementation note (Phase 4 task 8, added after this design was
  signed off):** closed for both storage engines, via
  `api/queryapi/tenant_isolation_gap_test.go`'s pointers. ClickHouse's
  refusal turned out to be structural, not something that needed new
  code: `chrunner.Registry` is built once at startup from
  `rbacstore.ListProvisionedDataSources`, which already excludes
  anything short of active+credentialed, so a mid-provisioning tenant is
  simply absent from the connection map — proven Docker-free
  (`chrunner_test.go`'s `TestRegistryRefusesMidProvisioningTenant`,
  since an empty `DataSource` list never dials ClickHouse). Tantivy was
  a real, different gap: `search/src/registry.rs`'s `IndexRegistry`
  opens-or-creates an index for *any* syntactically-valid `tenant_id` on
  first request, because it's a separate process with no Postgres
  access and structurally cannot know which tenants are provisioned --
  without a check upstream, a mid-provisioning tenant's query would have
  silently succeeded with zero results from a freshly-created empty
  index. Fixed by adding `enterprise/internal/searchclient.
  TenantChecker` (backed by a new `rbacstore.TenantIsActive`): `Client.
  Search` now refuses before the gRPC call goes out if the tenant isn't
  active, verified Docker-free via a real in-process gRPC server
  (`searchclient_test.go`'s `TestSearchRefusesMidProvisioningTenant`).
