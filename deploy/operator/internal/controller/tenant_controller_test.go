// Tests use controller-runtime's fake client (sigs.k8s.io/
// controller-runtime/pkg/client/fake), not envtest -- envtest needs a
// real kube-apiserver/etcd binary pair (setup-envtest) that isn't
// available in this environment (see package doc comment and
// deploy/README.md's verification section). A fake client exercises
// Reconcile's actual logic (status derivation, condition writes) against
// an in-memory tracker; what it can't exercise is anything a real
// apiserver would do for you (defaulting, admission, watch-triggered
// re-reconciliation) -- so passing here is real signal about this
// reconciler's logic, not proof it behaves correctly against a live
// cluster.
package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/sentry/sentry/deploy/operator/api/v1alpha1"
)

func newFakeReconciler(t *testing.T, objs ...client.Object) *TenantReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := sentryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding sentryv1alpha1 to scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&sentryv1alpha1.Tenant{}).
		Build()
	return &TenantReconciler{Client: fakeClient, Scheme: scheme}
}

func testTenant(name string, suspended bool) *sentryv1alpha1.Tenant {
	return &sentryv1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       sentryv1alpha1.TenantSpec{DisplayName: name, Suspended: suspended},
	}
}

func reconcile(t *testing.T, r *TenantReconciler, name string) sentryv1alpha1.Tenant {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got sentryv1alpha1.Tenant
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err != nil {
		t.Fatalf("getting tenant: %v", err)
	}
	return got
}

// TestReconcileUnprovisionedTenantIsProvisioningNotActive is the
// regression test for the pre-unification bug: this controller must
// never claim PhaseActive just because a Tenant object exists -- only
// enterprise-api -provision-tenant setting Status.ClickHouseDatabaseName
// (real ClickHouse provisioning having actually succeeded) earns that.
func TestReconcileUnprovisionedTenantIsProvisioningNotActive(t *testing.T) {
	tenant := testTenant("acme", false)
	r := newFakeReconciler(t, tenant)

	got := reconcile(t, r, "acme")

	if got.Status.Phase != sentryv1alpha1.PhaseProvisioning {
		t.Fatalf("Phase = %q, want Provisioning (nothing has provisioned this tenant yet)", got.Status.Phase)
	}
	cond := readyCondition(got)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition = %+v, want status False", cond)
	}
}

// TestReconcileReflectsProvisioningStateProvisionTenantSets is the core
// "lightweight unification" behavior: once something external (in
// production, -provision-tenant; here, simulated directly against the
// fake client the way a real K8s API server write would land) sets
// ClickHouseDatabaseName, this controller must report PhaseActive.
func TestReconcileReflectsProvisioningStateProvisionTenantSets(t *testing.T) {
	tenant := testTenant("acme", false)
	r := newFakeReconciler(t, tenant)
	ctx := context.Background()

	var toUpdate sentryv1alpha1.Tenant
	if err := r.Get(ctx, types.NamespacedName{Name: "acme", Namespace: "default"}, &toUpdate); err != nil {
		t.Fatalf("getting tenant: %v", err)
	}
	toUpdate.Status.ClickHouseDatabaseName = "acme"
	toUpdate.Status.ClickHouseSecretRef = "sentry-tenant-acme-clickhouse"
	toUpdate.Status.TantivyIndexPath = "/var/lib/sentry-search/tenants/acme"
	if err := r.Status().Update(ctx, &toUpdate); err != nil {
		t.Fatalf("simulating -provision-tenant's status write: %v", err)
	}

	got := reconcile(t, r, "acme")

	if got.Status.Phase != sentryv1alpha1.PhaseActive {
		t.Fatalf("Phase = %q, want Active", got.Status.Phase)
	}
	if got.Status.ClickHouseDatabaseName != "acme" || got.Status.ClickHouseSecretRef != "sentry-tenant-acme-clickhouse" || got.Status.TantivyIndexPath != "/var/lib/sentry-search/tenants/acme" {
		t.Fatalf("reconcile must not clobber the fields -provision-tenant set: %+v", got.Status)
	}
	cond := readyCondition(got)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v, want status True", cond)
	}
}

func TestReconcileSuspendedOverridesProvisionedState(t *testing.T) {
	tenant := testTenant("acme", true)
	tenant.Status.ClickHouseDatabaseName = "acme" // already provisioned
	r := newFakeReconciler(t, tenant)

	got := reconcile(t, r, "acme")

	if got.Status.Phase != sentryv1alpha1.PhaseSuspended {
		t.Fatalf("Phase = %q, want Suspended even though the tenant is provisioned", got.Status.Phase)
	}
}

// TestReconcileUnsuspendingReturnsToActiveNotProvisioning is the
// regression test for why Phase is *derived* fresh every reconcile
// (from Spec.Suspended + whether ClickHouseDatabaseName is set) rather
// than toggled in place: un-suspending an already-provisioned tenant
// must return it straight to Active, not demote it to Provisioning just
// because the last observed Phase happened to be Suspended.
func TestReconcileUnsuspendingReturnsToActiveNotProvisioning(t *testing.T) {
	tenant := testTenant("acme", true)
	tenant.Status.ClickHouseDatabaseName = "acme"
	r := newFakeReconciler(t, tenant)
	ctx := context.Background()

	_ = reconcile(t, r, "acme") // establishes Suspended

	var toUpdate sentryv1alpha1.Tenant
	if err := r.Get(ctx, types.NamespacedName{Name: "acme", Namespace: "default"}, &toUpdate); err != nil {
		t.Fatalf("getting tenant: %v", err)
	}
	toUpdate.Spec.Suspended = false
	if err := r.Update(ctx, &toUpdate); err != nil {
		t.Fatalf("unsuspending: %v", err)
	}

	got := reconcile(t, r, "acme")
	if got.Status.Phase != sentryv1alpha1.PhaseActive {
		t.Fatalf("Phase = %q, want Active after unsuspending an already-provisioned tenant", got.Status.Phase)
	}
}

func TestReconcileMissingTenantIsNoOp(t *testing.T) {
	r := newFakeReconciler(t)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile on a missing tenant should be a no-op, got error: %v", err)
	}
}

func readyCondition(tenant sentryv1alpha1.Tenant) *metav1.Condition {
	for i := range tenant.Status.Conditions {
		if tenant.Status.Conditions[i].Type == sentryv1alpha1.ConditionReady {
			return &tenant.Status.Conditions[i]
		}
	}
	return nil
}
