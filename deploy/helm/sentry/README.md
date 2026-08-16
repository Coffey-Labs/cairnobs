# deploy/helm/sentry

A Helm chart covering every `docker-compose.yml` service (Redpanda,
ClickHouse, Postgres, ingest, search, alerting, web) plus, when
`enterprise.enabled: true`: enterprise-auth, the `deploy/operator`
tenant-operator, and `Tenant` CRs from `values.tenants`. See
`/deploy/README.md` for what "multi-tenant-aware" does and doesn't mean
at this layer, and its verification-status section before trusting this
against a real cluster.

This chart never builds images -- push every image its `values.yaml`
references to a registry the cluster can pull from first, same division
of labor as `docker compose build` vs. `docker compose up`.

## `api` vs `enterprise-api`: one Deployment, chosen by `enterprise.enabled`

`templates/api.yaml` and `templates/enterprise-api.yaml` are mutually
exclusive, gated on opposite sides of the same `enterprise.enabled` flag
-- exactly one of them ever renders, both under the same
`{{ .Release.Name }}-api` Service name and port 8080. This is the fix
for what `/docs/security/threat-model.md` named as Phase 4's single
largest remaining gap once both storage engines' isolation mechanisms
were built: previously nothing forced or even flagged whether a
deployment ran the tenant-isolated binary. Now it's not a second knob to
remember -- the same flag that turns on RBAC/audit/SSO also swaps which
query binary actually serves `/query` and `/dashboards` traffic. Every
consumer (`alerting`'s `API_QUERY_URL`, `web`'s build args) needs zero
conditional logic of its own, since both variants answer on the same
name/port.

`enterprise-api` starts with an empty tenant set until
`-provision-tenant` has been run for at least one tenant (see
`/enterprise/README.md`) -- until then it's up and healthy, but every
`/query` request correctly fails closed with no tenant to route to.

## Startup ordering

`docker-compose.yml` uses `depends_on: condition: service_healthy` /
`service_completed_successfully` to sequence startup (e.g. `api` waits
for `clickhouse-migrate` to actually finish, not just for `clickhouse` to
be reachable). This chart approximates that more loosely:

- Migration Jobs (`clickhouse-migrate`, `metadata-migrate`,
  `redpanda-provision`) are plain `Job` resources (not Helm hooks --
  making the StatefulSets they depend on into hooks too, to get
  ordering, would break `helm upgrade`/`helm uninstall`'s normal
  ownership tracking of stateful resources, a worse tradeoff), with
  `backoffLimit: 6` so they retry a few times if their dependency isn't
  up yet.
- App Deployments get an `initContainer` that busy-waits for their
  dependency's **TCP port**, not for a specific Job's completion (see
  `templates/_helpers.tpl`'s `sentry.waitForTCP`) -- this covers "is
  ClickHouse/Postgres/Redpanda up" but not "has the migration Job
  actually finished."
- The gap that leaves (a pod starts before its migration has completed)
  is covered by every Go service here already calling `os.Exit(1)` on a
  failed startup DB ping (see e.g. `api/cmd/api/main.go`) --
  Kubernetes' pod restart policy retries with backoff until the schema
  is ready. This is a real, working, but *looser* guarantee than
  docker-compose's explicit ordering -- documented here rather than
  implied to be equivalent.

## Trying the two-tenant example

```sh
# Quote each --set value -- zsh globs an unquoted tenants[0] as a
# pattern and fails with "no matches found." Also note: no --include-crds
# here -- that's a helm template-only flag (install always installs
# crds/ by default); confirmed the hard way running this against a real
# kind cluster, see /docs/phase-4-runbook.md §7.
helm install sentry . \
  --set enterprise.enabled=true \
  --set tenantOperator.enabled=true \
  --set 'tenants[0].name=acme' --set 'tenants[0].displayName=Acme Corp' \
  --set 'tenants[1].name=globex' --set 'tenants[1].displayName=Globex Corporation'

kubectl get tenants
# expect: both Provisioning -- the Tenant CRs above are just a
# declarative request; nothing has actually provisioned ClickHouse for
# either yet (see below).

kubectl exec -it deploy/sentry-api -- /enterprise-api -provision-tenant=acme -display-name="Acme Corp"
kubectl exec -it deploy/sentry-api -- /enterprise-api -provision-tenant=globex -display-name="Globex Corporation"

kubectl get tenants
# expect: both Active now.
kubectl get secret sentry-tenant-acme-clickhouse sentry-tenant-globex-clickhouse
```

Before any of this: `ingest` needs a real mTLS cert Secret
(`--set ingest.tlsSecretName=...`, see `values.yaml`'s comment on it and
`/docs/phase-4-runbook.md` §7 for the exact `kubectl create secret`
invocation using `hack/dev-certs`) or it crash-loops on startup --
unconditional by design, no disable switch.

**Genuinely run against a real `kind` cluster, not just described**: see
`/docs/phase-4-runbook.md` §7 for the exact steps (image loading into
`kind`, the two chart bugs it found and fixed) and confirmation that
both tenants reached `status.phase: Active` with real credentials.

This proves Phase 4's "two tenants... with their own users, roles,
dashboards" exit criteria (`/CLAUDE.md`) end to end at the deployment-
topology layer: `-provision-tenant` (`enterprise/internal/
tenantprovision`) is what actually creates each tenant's ClickHouse
database/user/grant and marks it active in `rbacstore`; running inside
the `enterprise-api` Deployment's Pod means it automatically syncs that
real result into the `Tenant` CRD too (`enterprise/internal/tenantcrd`,
via the ServiceAccount/Role `tenantOperator.enabled` also grants that
Deployment) -- the credential Secret you see above has *real*
credentials, not a placeholder, and `Tenant.status.phase: Active` means
the same thing `rbacstore.tenants.status='active'` does, not two
different claims about two different systems. The `Tenant` CRD and
`-provision-tenant` used to be genuinely disconnected (a Secret existed
the moment the CR was created, with a password that authenticated
against nothing) -- see `/deploy/README.md`'s "lightweight unification"
section for the full history. OIDC/SAML login still needs a manual
`tenant_memberships` grant (`enterprise-auth -grant-membership-*` --
see `/docs/phase-4-runbook.md` §3a/§3b) before a human can actually
query as either tenant.

## `web`'s image needs rebuilding per environment

`web` is a static SvelteKit build (`adapter-static`) -- its three API
base URLs (`VITE_API_BASE_URL`/`VITE_ALERTING_API_BASE_URL`/
`VITE_ENTERPRISE_AUTH_BASE_URL`) are baked in at **image build time**
(`web/Dockerfile`'s build args), not read from the container's
environment at runtime. `values.yaml`'s `web.builtWithApiBaseURL` etc.
document what the image you point `web.image` at needs to have been
built with (an Ingress hostname, a LoadBalancer IP, etc.) -- this chart
has no Ingress resources and can't itself act on those values; rebuild
`web`'s image with the right build args for wherever this release is
actually reachable from a browser before pointing real users at it.

## Validating without a cluster

```sh
helm lint .
helm template sentry . --include-crds > /tmp/rendered.yaml
```

See `/deploy/README.md`'s verification section for what was checked
this way versus against a real cluster (now done -- see above).
