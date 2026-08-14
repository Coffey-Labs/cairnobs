// Package controller reconciles the Tenant CRD (api/v1alpha1) into the
// K8s-native artifacts task 2/CLAUDE.md's Phase 4 exit criteria calls
// for: "real per-tenant secret management (replacing today's single
// shared CLICKHOUSE_PASSWORD)" -- see docker-compose.yml's
// CLICKHOUSE_PASSWORD comment for what that shared-secret shape looks
// like today.
//
// What this reconciler does NOT do, named explicitly rather than
// implied: it never calls ClickHouse (no CREATE DATABASE/CREATE USER/
// GRANT), never touches the Tantivy index filesystem, and never talks to
// enterprise/internal/rbacstore. Those are enterprise/internal/
// tenantprovision's job -- unbuilt, per the task 5 summary. This
// controller's job stops at "does a K8s Secret with this tenant's
// ClickHouse credentials exist, and does the Tenant's status reflect
// that" -- the deployment-topology half of tenant provisioning, not the
// database-side half. A Tenant reaching PhaseActive here is NOT the same
// claim as rbacstore's tenants.status='active' (the actual gate every
// tenant-resolution code path checks per
// /docs/phase-4-isolation-design.md) -- reconciling those two into one
// state machine is exactly the kind of follow-up work
// /docs/phase-4-runbook.md's task 6 section names as deferred.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sentryv1alpha1 "github.com/sentry/sentry/deploy/operator/api/v1alpha1"
)

// TenantReconciler reconciles a Tenant object.
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// clickHouseSecretName is deterministic from the tenant name -- never
// randomly suffixed -- so a re-run of Reconcile (or a controller
// restart) finds the same Secret it created before, rather than losing
// track of it and creating a second one.
func clickHouseSecretName(tenant *sentryv1alpha1.Tenant) string {
	return fmt.Sprintf("sentry-tenant-%s-clickhouse", tenant.Name)
}

// tantivyIndexPath mirrors /docs/phase-4-isolation-design.md's Tantivy
// section: one directory per tenant under the shared search-index
// volume (search-index-data in docker-compose.yml; a PVC in the Helm
// chart -- see deploy/helm/sentry/templates/search-deployment.yaml).
func tantivyIndexPath(tenant *sentryv1alpha1.Tenant) string {
	return "/var/lib/sentry-search/tenants/" + tenant.Name
}

// generatePassword returns a 32-byte random value, base64-encoded --
// same "narrowly-granted, per-tenant, never the shared default user"
// framing as /docs/phase-4-isolation-design.md's ClickHouse section,
// applied to how the credential itself is generated (crypto/rand, not
// math/rand -- this becomes a real ClickHouse user's password once
// internal/tenantprovision consumes it).
func generatePassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// +kubebuilder:rbac:groups=sentry.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sentry.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var tenant sentryv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted -- owned Secret is garbage-collected by K8s via
			// its OwnerReference (set in reconcileSecret below), nothing
			// else to clean up at this layer. See this file's package
			// doc comment: real deprovisioning (revoking ClickHouse
			// grants) isn't this controller's job.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting tenant: %w", err)
	}

	secretName, err := r.reconcileSecret(ctx, &tenant)
	if err != nil {
		logger.Error(err, "reconciling clickhouse secret")
		return ctrl.Result{}, err
	}

	desiredPhase := sentryv1alpha1.PhaseActive
	if tenant.Spec.Suspended {
		desiredPhase = sentryv1alpha1.PhaseSuspended
	}

	tenant.Status.ClickHouseDatabaseName = tenant.Name
	tenant.Status.ClickHouseSecretRef = secretName
	tenant.Status.TantivyIndexPath = tantivyIndexPath(&tenant)
	tenant.Status.Phase = desiredPhase
	tenant.Status.ObservedGeneration = tenant.Generation
	meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               sentryv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "SecretReconciled",
		Message:            fmt.Sprintf("ClickHouse credential secret %q is present", secretName),
		ObservedGeneration: tenant.Generation,
	})

	if err := r.Status().Update(ctx, &tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating tenant status: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileSecret creates the tenant's ClickHouse credential Secret if
// it doesn't already exist. Deliberately never updates an existing
// Secret's password -- rotating a live tenant's ClickHouse credential
// out from under it (without first updating the ClickHouse-side grant,
// which this controller doesn't do) would just break every open
// connection for no benefit; credential rotation is real future work
// that needs to be coordinated with internal/tenantprovision, not
// something this reconcile loop can safely do alone.
func (r *TenantReconciler) reconcileSecret(ctx context.Context, tenant *sentryv1alpha1.Tenant) (string, error) {
	name := clickHouseSecretName(tenant)

	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: tenant.Namespace, Name: name}, &existing)
	if err == nil {
		return name, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("getting secret: %w", err)
	}

	password, err := generatePassword()
	if err != nil {
		return "", err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: tenant.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sentry-tenant-operator",
				"sentry.io/tenant":             tenant.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": "tenant_" + tenant.Name,
			"password": password,
			"database": tenant.Name,
		},
	}
	if err := controllerutil.SetControllerReference(tenant, secret, r.Scheme); err != nil {
		return "", fmt.Errorf("setting owner reference: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("creating secret: %w", err)
	}
	return name, nil
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sentryv1alpha1.Tenant{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
