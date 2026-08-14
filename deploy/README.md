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
the **application layer** inside `enterprise/` (one `api` process holds a
map of per-tenant ClickHouse connection pools; one `search` process holds
a map of per-tenant Tantivy indices) -- not at the Kubernetes layer. This
directory is **not** "one Deployment per tenant" or a general
multi-cluster system; that's an explicit Phase 4 non-goal (see
`/CLAUDE.md`). What it *does* add, matching that same document's exit
criteria ("real per-tenant secret management, replacing today's single
shared `CLICKHOUSE_PASSWORD`"):

- A `Tenant` CRD + controller that generates and manages one dedicated
  ClickHouse credential Secret per tenant (`operator/internal/controller`).
- A Helm chart that can install zero-or-more `Tenant` CRs
  (`values.tenants`) alongside the rest of the stack.

The Operator does **not** call ClickHouse (no `CREATE DATABASE`/`CREATE
USER`/`GRANT`) and does not touch the Tantivy filesystem or
`enterprise/internal/rbacstore` -- that's `enterprise/internal/
tenantprovision`, still unbuilt (see the Phase 4 task 5 summary). A
`Tenant` reaching `status.phase: Active` here means "this tenant has a
K8s Secret," not "this tenant's ClickHouse database/grants exist" --
those are two different systems' state machines that aren't reconciled
together yet, named explicitly rather than implied.

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
  built-in resource kind (22-29 resources depending on values, 0
  invalid) -- this catches schema mistakes (wrong field names, wrong
  types) but not whether the resources actually reconcile correctly
  together on a live cluster (Job/StatefulSet startup ordering, PVC
  provisioning, actual pod scheduling).
- Docker image builds (`operator/Dockerfile` and every other
  `Dockerfile` this chart references) were **not** verified in this
  session -- Docker's daemon wasn't reachable here either (see the
  Phase 4 task 5 conversation for why). Build and push every image this
  chart's `values.yaml` references before installing it.

Before relying on this in production: `kind create cluster`, `helm
install` with `--include-crds`, and walk through
`helm/sentry/README.md`'s two-tenant example end to end.
