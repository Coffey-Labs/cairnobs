# deploy

Kubernetes deployment for Sentry, added in Phase 4 (`/deploy` was
deliberately stubbed through Phase 3 -- see `/CLAUDE.md`'s Phase 3
non-goals). Two pieces:

- `operator/` -- a small Go controller-runtime Operator managing one CRD
  (`Tenant`). See `operator/README.md`.
- `helm/sentry/` -- a Helm chart covering every `docker-compose.yml`
  service, plus the operator and `Tenant` CRs when
  `enterprise.enabled=true`. See `helm/sentry/README.md`.

## What "multi-tenant-aware" means here, precisely

Per `/docs/phase-4-isolation-design.md`, tenant isolation itself lives at
the **application layer** inside `enterprise/` (`enterprise-api` holds a
map of per-tenant ClickHouse connection pools via `internal/chrunner`;
`search` holds a map of per-tenant Tantivy indices via
`src/registry.rs`) -- not at the Kubernetes layer. This directory is
**not** "one Deployment per tenant" or a general multi-cluster system;
that's an explicit Phase 4 non-goal (see `/CLAUDE.md`). What it *does*
add:

- A `Tenant` CRD + controller that generates and manages one dedicated
  ClickHouse credential Secret per tenant (`operator/internal/controller`).
- A Helm chart that can install zero-or-more `Tenant` CRs
  (`values.tenants`) alongside the rest of the stack, and — the newer
  piece — swaps `api`'s Deployment for `enterprise-api`'s whenever
  `enterprise.enabled` is true, so which query binary actually serves
  traffic is no longer a separately-forgettable decision (see
  `helm/sentry/README.md`'s "`api` vs `enterprise-api`" section).

**Two still-separate mechanisms, not yet unified**: the Operator's
`Tenant` CRD manages only the K8s-side credential Secret — it does not
call ClickHouse (no `CREATE DATABASE`/`CREATE USER`/`GRANT`) or touch
the Tantivy filesystem. `enterprise-api -provision-tenant=<id>` is what
actually does that (`enterprise/internal/tenantprovision`, built and
tested — see `/enterprise/README.md`), driven independently via
`rbacstore`, not from the `Tenant` CRD's reconcile loop. A `Tenant`
reaching `status.phase: Active` here means "this tenant has a K8s
Secret," not "this tenant's ClickHouse database/grants exist" — running
both mechanisms for the same tenant ID today requires two separate
operator actions, named explicitly rather than implied to be one.

## Verification status -- read before trusting this against a real cluster

**Not verified against a live Kubernetes cluster.** This environment has
no `kubectl`/`kind`/`minikube`/`kubebuilder`/cluster reachable, so
nothing here has been `kubectl apply`'d or `helm install`'d for real.
Same disclosed-limitation shape as `/agent/README.md`'s "Windows-specific
agent code remains unverified on real Windows" from Phase 1 -- a real gap
to close before shipping, not swept under the rug.

What **was** actually verified, offline, in this environment (network
access was available to fetch these tools, but no cluster):

- `deploy/operator`: `go build`/`go vet`/`go test ./...` all pass,
  including reconciler tests against controller-runtime's fake client
  (`internal/controller/tenant_controller_test.go`) -- real reconcile
  logic exercised, but not against a real apiserver (no `envtest`
  binaries available; see that test file's doc comment).
- `deploy/operator/config/crd/sentry.io_tenants.yaml`: parsed with
  `sigs.k8s.io/yaml` + strict-unmarshaled into the real
  `k8s.io/apiextensions-apiserver` `CustomResourceDefinition` Go type --
  catches YAML syntax errors and structural mistakes, not a live-cluster
  admission check.
- `deploy/helm/sentry`: `helm lint` passes; `helm template` renders
  cleanly under both default values and a `enterprise.enabled: true` +
  two-tenant override; the rendered output was checked with `kubeconform
  -strict` against the real Kubernetes 1.31 OpenAPI schema for every
  built-in resource kind (22-31 resources depending on values, 0
  invalid) -- this catches schema mistakes (wrong field names, wrong
  types) but not whether the resources actually reconcile correctly
  together on a live cluster (Job/StatefulSet startup ordering, PVC
  provisioning, actual pod scheduling). Specifically confirmed by
  parsing the rendered YAML (not just eyeballing it): exactly one
  `Deployment`/`Service` named `sentry-api` renders in each mode, with
  the `enterprise.enabled: true` render using the `enterprise-api` image
  and the default render using plain `api`'s.
- Docker image builds (`operator/Dockerfile` and every other
  `Dockerfile` this chart references) were **not** verified in this
  session -- Docker's daemon wasn't reachable here either (see the
  Phase 4 task 5 conversation for why). Build and push every image this
  chart's `values.yaml` references before installing it.

Before relying on this in production: `kind create cluster`, `helm
install` with `--include-crds`, and walk through
`helm/sentry/README.md`'s two-tenant example end to end.
