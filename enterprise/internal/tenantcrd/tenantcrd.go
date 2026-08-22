// Package tenantcrd syncs enterprise-api -provision-tenant's real
// provisioning result into the Tenant CRD (deploy/operator/api/
// v1alpha1) -- the "lightweight unification" named in CLAUDE.md's "two
// independent provisioning mechanisms" gap: -provision-tenant stays the
// sole real actor (rbacstore + tenantprovision.ProvisionClickHouse, see
// runProvisionTenant's doc comment in cmd/enterprise-api/main.go); this
// package's only job is making that result observable via `kubectl get
// tenants` too, for a deployment that also runs deploy/operator's
// tenant-operator. deploy/operator/internal/controller.TenantReconciler
// derives Phase/Conditions from what this package writes -- it never
// invents "provisioned" on its own.
//
// Deliberately uses the K8s dynamic client (unstructured.Unstructured +
// a GroupVersionResource) rather than importing deploy/operator/api/
// v1alpha1's typed Tenant struct: deploy/operator is a separate Go
// module (its own go.mod), and adding a cross-module `replace` directive
// between two independently-versioned modules is exactly the kind of
// coupling this codebase has otherwise avoided (enterprise/ already only
// imports api/, never deploy/operator/). The typed Secret API
// (k8s.io/client-go/kubernetes) is used for the credential Secret
// itself, since Secret is a stable, well-known built-in type with no
// such module-boundary concern.
package tenantcrd

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// tenantGVR identifies deploy/operator/config/crd/cairnobs.io_tenants.yaml's
// resource -- kept as a plain schema.GroupVersionResource (not the typed
// package) for the reason this file's doc comment explains.
var tenantGVR = schema.GroupVersionResource{Group: "cairnobs.io", Version: "v1alpha1", Resource: "tenants"}

// Syncer talks to the K8s API. Construction (New) is the only place
// that can fail for "no cluster reachable" reasons -- Sync itself
// assumes a working client.
type Syncer struct {
	dynamic   dynamic.Interface
	clientset kubernetes.Interface
	namespace string
}

// New builds a Syncer for namespace, trying in-cluster config first (the
// real deployment shape: -provision-tenant runs via `kubectl exec` into
// the already-running enterprise-api Pod, which carries its own
// ServiceAccount) and falling back to KUBECONFIG/the default kubeconfig
// path for local/dev convenience. Returns an error if neither is
// reachable -- callers decide whether that's fatal (see
// cmd/enterprise-api/main.go's runProvisionTenant: it is, once CR sync
// has been explicitly requested via TENANT_CRD_NAMESPACE, since a
// silent skip would leave the CRD stale/wrong again, the exact bug this
// package exists to fix).
func New(namespace string) (*Syncer, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("tenantcrd: no in-cluster config and no usable kubeconfig: %w", err)
		}
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("tenantcrd: building dynamic client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("tenantcrd: building clientset: %w", err)
	}
	return &Syncer{dynamic: dyn, clientset: clientset, namespace: namespace}, nil
}

// newForTest builds a Syncer directly from fake clients -- production
// code always goes through New, but tests need to inject
// k8s.io/client-go/dynamic/fake and kubernetes/fake without a real or
// in-cluster API server.
func newForTest(dyn dynamic.Interface, clientset kubernetes.Interface, namespace string) *Syncer {
	return &Syncer{dynamic: dyn, clientset: clientset, namespace: namespace}
}

// Credentials is the minimal shape Sync needs -- deliberately not
// tenantprovision.Credentials itself, matching chrunner.DataSource's
// "narrow, not the storage type" precedent.
type Credentials struct {
	Username string
	Password string
}

// SecretName is deterministic from the tenant ID -- never randomly
// suffixed -- so a re-run of Sync (idempotent retry, see
// runProvisionTenant) finds and updates the same Secret rather than
// creating a second one.
func SecretName(tenantID string) string {
	return fmt.Sprintf("cairnobs-tenant-%s-clickhouse", tenantID)
}

// Sync upserts the Tenant object (creating it with spec.displayName if
// absent) and its credential Secret, then patches status to reflect
// real, already-confirmed provisioning -- called only after
// tenantprovision.ProvisionClickHouse has actually succeeded, never
// before. Safe to call repeatedly for the same tenant (idempotent):
// re-running overwrites the Secret with the same credentials rbacstore
// already has on file (never rotates to a *new*, different credential --
// that would break every open connection for no benefit, same
// reasoning the operator's now-removed reconcileSecret documented) and
// re-applies the same status fields.
func (s *Syncer) Sync(ctx context.Context, tenantID, displayName, tantivyIndexPath string, creds Credentials) error {
	uid, err := s.upsertTenant(ctx, tenantID, displayName)
	if err != nil {
		return fmt.Errorf("tenantcrd: upserting tenant object: %w", err)
	}
	secretName := SecretName(tenantID)
	if err := s.upsertSecret(ctx, tenantID, secretName, uid, creds); err != nil {
		return fmt.Errorf("tenantcrd: upserting credential secret: %w", err)
	}
	if err := s.patchStatus(ctx, tenantID, secretName, tantivyIndexPath); err != nil {
		return fmt.Errorf("tenantcrd: patching tenant status: %w", err)
	}
	return nil
}

func (s *Syncer) upsertTenant(ctx context.Context, tenantID, displayName string) (types.UID, error) {
	client := s.dynamic.Resource(tenantGVR).Namespace(s.namespace)

	existing, err := client.Get(ctx, tenantID, metav1.GetOptions{})
	if err == nil {
		return existing.GetUID(), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("getting existing tenant object: %w", err)
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cairnobs.io/v1alpha1",
		"kind":       "Tenant",
		"metadata": map[string]interface{}{
			"name":      tenantID,
			"namespace": s.namespace,
		},
		"spec": map[string]interface{}{
			"displayName": displayName,
		},
	}}
	created, err := client.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating tenant object: %w", err)
	}
	return created.GetUID(), nil
}

func (s *Syncer) upsertSecret(ctx context.Context, tenantID, secretName string, tenantUID types.UID, creds Credentials) error {
	secrets := s.clientset.CoreV1().Secrets(s.namespace)
	controllerTrue := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: s.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cairnobs-enterprise-api",
				"cairnobs.io/tenant":           tenantID,
			},
			// Owned by the Tenant object even though a different
			// process (this one, not the operator's controller)
			// created it -- K8s's garbage collector honors
			// OwnerReferences regardless of which actor set them, so
			// deleting the Tenant still cleans this Secret up.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "cairnobs.io/v1alpha1",
				Kind:               "Tenant",
				Name:               tenantID,
				UID:                tenantUID,
				Controller:         &controllerTrue,
				BlockOwnerDeletion: &controllerTrue,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": creds.Username,
			"password": creds.Password,
			"database": tenantID,
		},
	}

	existing, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	switch {
	case err == nil:
		secret.ResourceVersion = existing.ResourceVersion
		_, err := secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	case apierrors.IsNotFound(err):
		_, err := secrets.Create(ctx, secret, metav1.CreateOptions{})
		return err
	default:
		return fmt.Errorf("getting existing secret: %w", err)
	}
}

func (s *Syncer) patchStatus(ctx context.Context, tenantID, secretName, tantivyIndexPath string) error {
	client := s.dynamic.Resource(tenantGVR).Namespace(s.namespace)
	existing, err := client.Get(ctx, tenantID, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting tenant object for status update: %w", err)
	}
	if err := unstructured.SetNestedField(existing.Object, tenantID, "status", "clickHouseDatabaseName"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(existing.Object, secretName, "status", "clickHouseSecretRef"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(existing.Object, tantivyIndexPath, "status", "tantivyIndexPath"); err != nil {
		return err
	}
	_, err = client.UpdateStatus(ctx, existing, metav1.UpdateOptions{})
	return err
}
