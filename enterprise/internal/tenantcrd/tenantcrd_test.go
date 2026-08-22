// Tests use k8s.io/client-go's fake dynamic and typed clientsets, not a
// real or in-cluster API server -- genuinely runnable in an environment
// with no Kubernetes access at all, same "real client library, fake
// transport" shape as enterprise/internal/searchclient's in-process gRPC
// tests. What a fake client can't exercise: real admission/defaulting,
// or that deploy/operator's actual CRD schema accepts what this package
// writes (see /docs/phase-4-runbook.md's verification-status notes for
// the Helm/kubeconform-based schema check that covers that instead).
package tenantcrd

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func newTestSyncer(t *testing.T, namespace string) *Syncer {
	t.Helper()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{tenantGVR: "TenantList"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	clientset := k8sfake.NewSimpleClientset()
	return newForTest(dyn, clientset, namespace)
}

func getTenant(t *testing.T, s *Syncer, tenantID string) *unstructured.Unstructured {
	t.Helper()
	obj, err := s.dynamic.Resource(tenantGVR).Namespace(s.namespace).Get(context.Background(), tenantID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting tenant object: %v", err)
	}
	return obj
}

func TestSyncCreatesTenantObjectWithDisplayName(t *testing.T) {
	s := newTestSyncer(t, "cairnobs")
	err := s.Sync(context.Background(), "acme", "Acme Corp", "/var/lib/cairnobs-search/tenants/acme", Credentials{Username: "tenant_acme", Password: "secret-pw"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	obj := getTenant(t, s, "acme")
	displayName, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
	if displayName != "Acme Corp" {
		t.Fatalf("spec.displayName = %q, want %q", displayName, "Acme Corp")
	}
}

func TestSyncSetsRealStatusFields(t *testing.T) {
	s := newTestSyncer(t, "cairnobs")
	err := s.Sync(context.Background(), "acme", "Acme Corp", "/var/lib/cairnobs-search/tenants/acme", Credentials{Username: "tenant_acme", Password: "secret-pw"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	obj := getTenant(t, s, "acme")
	dbName, _, _ := unstructured.NestedString(obj.Object, "status", "clickHouseDatabaseName")
	if dbName != "acme" {
		t.Fatalf("status.clickHouseDatabaseName = %q, want acme", dbName)
	}
	secretRef, _, _ := unstructured.NestedString(obj.Object, "status", "clickHouseSecretRef")
	if secretRef != "cairnobs-tenant-acme-clickhouse" {
		t.Fatalf("status.clickHouseSecretRef = %q, want cairnobs-tenant-acme-clickhouse", secretRef)
	}
	indexPath, _, _ := unstructured.NestedString(obj.Object, "status", "tantivyIndexPath")
	if indexPath != "/var/lib/cairnobs-search/tenants/acme" {
		t.Fatalf("status.tantivyIndexPath = %q, want /var/lib/cairnobs-search/tenants/acme", indexPath)
	}
}

func TestSyncCreatesSecretOwnedByTenant(t *testing.T) {
	s := newTestSyncer(t, "cairnobs")
	err := s.Sync(context.Background(), "acme", "Acme Corp", "/idx", Credentials{Username: "tenant_acme", Password: "secret-pw"})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	secret, err := s.clientset.CoreV1().Secrets("cairnobs").Get(context.Background(), "cairnobs-tenant-acme-clickhouse", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting secret: %v", err)
	}
	if secret.StringData["username"] != "tenant_acme" || secret.StringData["password"] != "secret-pw" || secret.StringData["database"] != "acme" {
		t.Fatalf("unexpected secret data: %+v", secret.StringData)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != "acme" || secret.OwnerReferences[0].Kind != "Tenant" {
		t.Fatalf("expected the secret to be owned by the Tenant object, got %+v", secret.OwnerReferences)
	}
}

// TestSyncIsIdempotentAndNeverChangesCredentials is the regression test
// for the "safe to retry" property runProvisionTenant's idempotent
// CR-sync-only retry path depends on (see cmd/enterprise-api/main.go):
// running Sync twice for the same tenant must not create a duplicate
// Tenant object, must not error, and must never silently swap in
// different credentials than what was passed.
func TestSyncIsIdempotentAndNeverChangesCredentials(t *testing.T) {
	s := newTestSyncer(t, "cairnobs")
	ctx := context.Background()
	creds := Credentials{Username: "tenant_acme", Password: "secret-pw"}

	if err := s.Sync(ctx, "acme", "Acme Corp", "/idx", creds); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if err := s.Sync(ctx, "acme", "Acme Corp", "/idx", creds); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	list, err := s.dynamic.Resource(tenantGVR).Namespace("cairnobs").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing tenants: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly one Tenant object after two Syncs, got %d", len(list.Items))
	}

	secret, err := s.clientset.CoreV1().Secrets("cairnobs").Get(ctx, "cairnobs-tenant-acme-clickhouse", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting secret: %v", err)
	}
	if secret.StringData["password"] != "secret-pw" {
		t.Fatalf("password changed across a re-sync: %q", secret.StringData["password"])
	}
}

func TestSyncPreservesExistingTenantObjectDisplayName(t *testing.T) {
	s := newTestSyncer(t, "cairnobs")
	ctx := context.Background()

	// A human/GitOps process already created this Tenant object (e.g.
	// via `kubectl apply`, per TenantSpec's doc comment) before
	// -provision-tenant ever ran -- Sync must not overwrite their
	// chosen displayName with its own.
	pre := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cairnobs.io/v1alpha1",
		"kind":       "Tenant",
		"metadata":   map[string]interface{}{"name": "acme", "namespace": "cairnobs"},
		"spec":       map[string]interface{}{"displayName": "Human-Chosen Name"},
	}}
	if _, err := s.dynamic.Resource(tenantGVR).Namespace("cairnobs").Create(ctx, pre, metav1.CreateOptions{}); err != nil {
		t.Fatalf("pre-creating tenant: %v", err)
	}

	if err := s.Sync(ctx, "acme", "Some Other Name -provision-tenant Was Called With", "/idx", Credentials{Username: "u", Password: "p"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	obj := getTenant(t, s, "acme")
	displayName, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
	if displayName != "Human-Chosen Name" {
		t.Fatalf("spec.displayName = %q, want the pre-existing value preserved", displayName)
	}
}
