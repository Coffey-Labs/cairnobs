# deploy

Kubernetes deployment for Cairn OBS, added in Phase 4 (`/deploy` was
deliberately stubbed through Phase 3 -- see `/CLAUDE.md`'s Phase 3
non-goals). Two pieces:

- `operator/` -- a small Go controller-runtime Operator managing one CRD
  (`Tenant`). See `operator/README.md`.
- `helm/cairnobs/` -- a Helm chart covering every `docker-compose.yml`
  service, plus the operator and `Tenant` CRs when
  `enterprise.enabled=true`. See `helm/cairnobs/README.md`.

## What "multi-tenant-aware" means here, precisely

Per `/docs/phase-4-isolation-design.md`, tenant isolation itself lives at
the **application layer** inside `enterprise/` (`enterprise-api` holds a
map of per-tenant ClickHouse connection pools via `internal/chrunner`;
`search` holds a map of per-tenant Tantivy indices via
`src/registry.rs`) -- not at the Kubernetes layer. This directory is
**not** "one Deployment per tenant" or a general multi-cluster system;
that's an explicit Phase 4 non-goal (see `/CLAUDE.md`). What it *does*
add:

- A `Tenant` CRD + controller (`operator/internal/controller`) that
  reflects real provisioning state onto `status.phase`/a `Ready`
  condition, derived from whether `enterprise-api -provision-tenant` has
  reported real ClickHouse provisioning.
- A Helm chart that can install zero-or-more `Tenant` CRs
  (`values.tenants`) alongside the rest of the stack, and swaps `api`'s
  Deployment for `enterprise-api`'s whenever `enterprise.enabled` is
  true, so which query binary actually serves traffic is no longer a
  separately-forgettable decision (see `helm/cairnobs/README.md`'s "`api`
  vs `enterprise-api`" section).

**Now unified, in a deliberately lightweight way**: `enterprise-api
-provision-tenant=<id>` stays the sole real actor — it's the only thing
that calls ClickHouse (`CREATE DATABASE`/`CREATE USER`/`GRANT`, via
`enterprise/internal/tenantprovision`) and writes `rbacstore`. What
changed: once it succeeds, it also syncs the result into the `Tenant`
CRD (`enterprise/internal/tenantcrd`) — creating the Secret with *real*
credentials (the controller no longer generates a placeholder one that
authenticated against nothing) and setting the status fields the
controller reads to compute `Phase`/`Ready`. The controller itself
gained no new credentials and still never touches ClickHouse/Postgres --
it's a pure function of `spec.suspended` and whatever
`-provision-tenant` has reported, never an independent second guess at
"is this tenant really provisioned." A `Tenant` reaching
`status.phase: Active` now means the same thing `rbacstore.tenants.
status='active'` does, not two different claims — see
`enterprise/internal/tenantcrd`'s and `operator/internal/controller/
tenant_controller.go`'s doc comments for the full split, and
`enterprise-api -provision-tenant`'s `TENANT_CRD_NAMESPACE` env var
(set automatically by the Helm chart when `tenantOperator.enabled`) to
turn this on. Deliberately not built: the operator's reconcile loop
itself calling ClickHouse/rbacstore directly (a "full unification"
option considered and set aside — it would give the operator two new
credential sets and require real reconcile-loop idempotency design for
an inherently one-shot external side effect, a bigger and riskier
change than this repo's provisioning story needed to close the actual
gap, which was two *disconnected* sources of truth, not two actors).

## Verification status -- read before trusting this against a real cluster

**Now verified against a real live Kubernetes cluster.** `kind`/
`kubectl`/`helm` were installed without root (static binaries into
`~/.local/bin`), a real local cluster was created, every image this
chart references was built and loaded into it, and the full two-tenant
walkthrough (`helm/cairnobs/README.md`) was run end to end -- both tenants
reached `Tenant.status.phase: Active` with real generated ClickHouse
credentials in their Secrets. See `/docs/phase-4-runbook.md` §7 for the
exact commands and the two real chart bugs this run found and fixed
(`enterprise-auth` missing its Postgres connection env vars entirely,
ClickHouse missing the env var that grants `CREATE USER` privilege) --
neither `helm lint`, `helm template`, nor the `kubeconform` schema check
below could have caught either, since both only manifest once real pods
actually try to start and connect to each other.

What was verified offline, before real cluster access existed (still
true, kept as additional evidence, not superseded by the above):

- `deploy/operator`: `go build`/`go vet`/`go test ./...` all pass,
  including reconciler tests against controller-runtime's fake client
  (`internal/controller/tenant_controller_test.go`) -- real reconcile
  logic exercised, but not against a real apiserver (no `envtest`
  binaries available; see that test file's doc comment).
- `enterprise/internal/tenantcrd` (the "lightweight unification"
  half `-provision-tenant` runs): `go test` passes against
  `k8s.io/client-go`'s fake dynamic and typed clientsets -- real client
  library, fake transport, same shape as `enterprise/internal/
  searchclient`'s in-process gRPC tests. What this doesn't prove: that
  `cairnobs.io/v1alpha1.Tenant`'s real CRD schema (a real apiserver's
  OpenAPI validation) accepts exactly what this package writes -- the
  `helm template`/kubeconform check below covers the schema shape, not
  a live write against it.
- `deploy/operator/config/crd/cairnobs.io_tenants.yaml`: parsed with
  `sigs.k8s.io/yaml` + strict-unmarshaled into the real
  `k8s.io/apiextensions-apiserver` `CustomResourceDefinition` Go type --
  catches YAML syntax errors and structural mistakes, not a live-cluster
  admission check.
- `deploy/helm/cairnobs`: `helm lint` passes; `helm template` renders
  cleanly under both default values and a `enterprise.enabled: true` +
  two-tenant override; the rendered output was checked with `kubeconform
  -strict` against the real Kubernetes 1.31 OpenAPI schema for every
  built-in resource kind (22-31 resources depending on values, 0
  invalid) -- this catches schema mistakes (wrong field names, wrong
  types) but not whether the resources actually reconcile correctly
  together on a live cluster (Job/StatefulSet startup ordering, PVC
  provisioning, actual pod scheduling). Specifically confirmed by
  parsing the rendered YAML (not just eyeballing it): exactly one
  `Deployment`/`Service` named `cairnobs-api` renders in each mode, with
  the `enterprise.enabled: true` render using the `enterprise-api` image
  and the default render using plain `api`'s. Also confirmed for the
  `tenantOperator.enabled: true` case: `enterprise-api` gets its own
  ServiceAccount/Role/RoleBinding (exactly `tenants`/`tenants/status`/
  `secrets`, no more), `tenant-operator`'s own ClusterRole no longer
  grants `secrets` at all, and `TENANT_CRD_NAMESPACE` is set on
  `enterprise-api`'s container only when `tenantOperator.enabled` is
  true.
- Docker image builds (`operator/Dockerfile` and every other
  `Dockerfile` this chart references) are now confirmed working too --
  all twelve images this chart needs were built and loaded into the test
  `kind` cluster above.

Before relying on this in production: `ingest` needs a real cert-manager
(or equivalent) issued Secret, not `hack/dev-certs`'s throwaway dev
certs; ClickHouse/Postgres data isn't backed by anything durable beyond
the cluster's own PVC provisioner in this chart; and only Auth0 has been
tried as a real external IdP so far (see
`/docs/phase-4-runbook.md` §3a/§3b).
