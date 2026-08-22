# deploy/operator

A small `controller-runtime` Operator managing one CRD: `Tenant`
(`cairnobs.io/v1alpha1`). See `internal/controller/tenant_controller.go`'s
doc comment for exactly what it reconciles and -- just as importantly --
what it deliberately doesn't (no ClickHouse calls, no Tantivy filesystem
access, no `enterprise/internal/rbacstore` wiring; those are
`enterprise/internal/tenantprovision`, still unbuilt).

## Not kubebuilder-scaffolded

No `kubebuilder`/`controller-gen` binary was available in this
environment, so this package is hand-written rather than generated:

- `api/v1alpha1/zz_generated.deepcopy.go` -- normally `controller-gen
  object` output; hand-written here, covered by
  `api/v1alpha1/api_test.go`'s round-trip tests (mutate a copy, assert
  the original is untouched -- exactly the class of bug a hand-written
  `DeepCopy` is prone to).
- `config/crd/cairnobs.io_tenants.yaml` -- normally `controller-gen crd`
  output from the `+kubebuilder:validation:*` markers on
  `api/v1alpha1/tenant_types.go`; hand-written here and only as strong as
  keeping the two in sync by hand. Validated by strict-unmarshaling it
  into the real `k8s.io/apiextensions-apiserver` Go type (see
  `/deploy/README.md`'s verification section) -- catches YAML/structural
  mistakes, not a drift between the CRD's field *descriptions* and the
  Go doc comments.
- `+kubebuilder:rbac` markers on `internal/controller/tenant_controller.go`
  are present as documentation/intent (matching kubebuilder convention)
  but were never run through `controller-gen rbac` -- the actual
  ClusterRole is hand-written in
  `/deploy/helm/cairnobs/templates/tenant-operator.yaml`, kept in sync with
  those markers by hand, same caveat as the CRD above.

## Layout

```
api/v1alpha1/          Tenant, TenantSpec, TenantStatus -- the CRD's Go types
internal/controller/    TenantReconciler -- see its doc comment
cmd/tenant-operator/     main.go -- manager setup, matches every other
                         service's cmd/<name>/main.go convention in this repo
config/crd/               hand-written CRD YAML (see above)
```

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

Tests use `sigs.k8s.io/controller-runtime/pkg/client/fake`, not
`envtest` -- `envtest` needs a real `kube-apiserver`/`etcd` binary pair
(`setup-envtest`) not available in this environment. The fake client
exercises real reconcile logic (object CRUD, owner references, status
writes) but not anything a real apiserver does for you (admission,
garbage collection, watch-triggered re-reconciliation) -- see
`internal/controller/tenant_controller_test.go`'s doc comment.

```sh
docker build -f Dockerfile -t cairnobs-tenant-operator .   # context is deploy/operator/, not the repo root
```

Not verified in this session -- see `/deploy/README.md`.

## Trying it against a real cluster

```sh
kubectl apply -f config/crd/cairnobs.io_tenants.yaml
kubectl apply -f - <<'EOF'
apiVersion: cairnobs.io/v1alpha1
kind: Tenant
metadata:
  name: acme
spec:
  displayName: "Acme Corp"
EOF
kubectl get tenant acme -o yaml   # status.phase should reach Active
kubectl get secret cairnobs-tenant-acme-clickhouse -o yaml
```
