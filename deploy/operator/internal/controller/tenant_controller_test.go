// Tests use controller-runtime's fake client (sigs.k8s.io/
// controller-runtime/pkg/client/fake), not envtest -- envtest needs a
// real kube-apiserver/etcd binary pair (setup-envtest) that isn't
// available in this environment (see package doc comment and
// deploy/README.md's verification section). A fake client exercises
// Reconcile's actual logic (object CRUD, owner references, status
// writes) against an in-memory tracker; what it can't exercise is
// anything a real apiserver would do for you (defaulting, admission,
// actual garbage collection of owned objects, watch-triggered
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

func TestReconcileCreatesSecretAndSetsActivePhase(t *testing.T) {
	tenant := testTenant("acme", false)
	r := newFakeReconciler(t, tenant)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "sentry-tenant-acme-clickhouse", Namespace: "default"}, &secret); err != nil {
		t.Fatalf("expected a ClickHouse secret to be created: %v", err)
	}
	if secret.StringData["username"] != "tenant_acme" || secret.StringData["database"] != "acme" {
		t.Fatalf("unexpected secret data: %+v", secret.StringData)
	}
	if secret.StringData["password"] == "" {
		t.Fatal("expected a non-empty generated password")
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != "acme" {
		t.Fatalf("expected secret to be owned by the Tenant, got %+v", secret.OwnerReferences)
	}

	var got sentryv1alpha1.Tenant
	if err := r.Get(ctx, types.NamespacedName{Name: "acme", Namespace: "default"}, &got); err != nil {
		t.Fatalf("getting tenant: %v", err)
	}
	if got.Status.Phase != sentryv1alpha1.PhaseActive {
		t.Fatalf("Phase = %q, want Active", got.Status.Phase)
	}
	if got.Status.ClickHouseDatabaseName != "acme" {
		t.Fatalf("ClickHouseDatabaseName = %q, want acme", got.Status.ClickHouseDatabaseName)
	}
	if got.Status.ClickHouseSecretRef != "sentry-tenant-acme-clickhouse" {
		t.Fatalf("ClickHouseSecretRef = %q", got.Status.ClickHouseSecretRef)
	}
	if got.Status.TantivyIndexPath != "/var/lib/sentry-search/tenants/acme" {
		t.Fatalf("TantivyIndexPath = %q", got.Status.TantivyIndexPath)
	}
}

func TestReconcileSuspendedSetsSuspendedPhaseButKeepsSecret(t *testing.T) {
	tenant := testTenant("acme", true)
	r := newFakeReconciler(t, tenant)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme", Namespace: "default"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got sentryv1alpha1.Tenant
	if err := r.Get(ctx, types.NamespacedName{Name: "acme", Namespace: "default"}, &got); err != nil {
		t.Fatalf("getting tenant: %v", err)
	}
	if got.Status.Phase != sentryv1alpha1.PhaseSuspended {
		t.Fatalf("Phase = %q, want Suspended", got.Status.Phase)
	}

	// A suspended tenant's credential Secret is NOT deleted -- suspension
	// is reversible and this controller doesn't manage ClickHouse-side
	// grants, so there's nothing at this layer to actually enforce
	// suspension; deleting the Secret would just be theater.
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "sentry-tenant-acme-clickhouse", Namespace: "default"}, &secret); err != nil {
		t.Fatalf("expected secret to still exist for a suspended tenant: %v", err)
	}
}

func TestReconcileIsIdempotentAndNeverRotatesPassword(t *testing.T) {
	tenant := testTenant("acme", false)
	r := newFakeReconciler(t, tenant)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme", Namespace: "default"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	var first corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "sentry-tenant-acme-clickhouse", Namespace: "default"}, &first); err != nil {
		t.Fatalf("getting secret after first reconcile: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	var second corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "sentry-tenant-acme-clickhouse", Namespace: "default"}, &second); err != nil {
		t.Fatalf("getting secret after second reconcile: %v", err)
	}

	if first.StringData["password"] != second.StringData["password"] {
		t.Fatal("password changed across a re-reconcile -- would break every live connection for this tenant")
	}
}

func TestReconcileMissingTenantIsNoOp(t *testing.T) {
	r := newFakeReconciler(t)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile on a missing tenant should be a no-op, got error: %v", err)
	}
}
