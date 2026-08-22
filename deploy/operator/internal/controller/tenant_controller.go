// Package controller reconciles the Tenant CRD (api/v1alpha1) -- see
// TenantPhase's doc comment for the "lightweight unification" this
// controller is one half of. This reconciler never calls ClickHouse (no
// CREATE DATABASE/CREATE USER/GRANT), never touches the Tantivy index
// filesystem, and never talks to enterprise/internal/rbacstore -- those
// stay enterprise-api -provision-tenant's job (enterprise/internal/
// tenantprovision, enterprise/internal/tenantcrd). This controller's
// job is purely a function of what's already on the object: derive
// Phase and the Ready condition from Spec.Suspended and whether
// Status.ClickHouseDatabaseName has been set by -provision-tenant,
// nothing more. A Tenant reaching PhaseActive here is exactly the same
// claim as rbacstore's tenants.status='active' now, not a
// second, independently-computed one -- see docs/phase-4-isolation-design.md.
//
// Earlier versions of this controller also generated a per-tenant
// ClickHouse credential Secret with a locally-generated random
// password. That Secret authenticated against nothing (nothing in this
// controller ever called ClickHouse to create a matching user) and
// nothing else in the codebase ever read it -- a placeholder that
// actively misled ("looks provisioned") rather than one that honestly
// represented "not yet provisioned." Removed rather than fixed in
// place: the real Secret, with real credentials, is now created by
// -provision-tenant (enterprise/internal/tenantcrd) once ClickHouse
// provisioning actually succeeds.
package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cairnobsv1alpha1 "github.com/cairnobs/cairnobs/deploy/operator/api/v1alpha1"
)

// TenantReconciler reconciles a Tenant object.
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cairnobs.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cairnobs.io,resources=tenants/status,verbs=get;update;patch

func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tenant cairnobsv1alpha1.Tenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted -- the owned Secret -provision-tenant created (if
			// any) is garbage-collected by K8s via its OwnerReference,
			// nothing else to clean up at this layer. Real
			// deprovisioning (revoking ClickHouse grants) isn't this
			// controller's job, or -provision-tenant's today -- see
			// /docs/security/threat-model.md's non-goals.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting tenant: %w", err)
	}

	// provisioned is true once -provision-tenant has confirmed real
	// ClickHouse provisioning by setting this field -- see
	// TenantStatus.ClickHouseDatabaseName's doc comment. This
	// controller treats it as the sole source of truth for "has
	// provisioning actually happened," never claiming PhaseActive on
	// its own say-so the way the pre-unification version did.
	provisioned := tenant.Status.ClickHouseDatabaseName != ""

	condStatus := metav1.ConditionFalse
	reason, message := "AwaitingProvisioning", "waiting for enterprise-api -provision-tenant to provision ClickHouse for this tenant"
	switch {
	case tenant.Spec.Suspended:
		tenant.Status.Phase = cairnobsv1alpha1.PhaseSuspended
		reason, message = "Suspended", "tenant is suspended (spec.suspended=true)"
	case provisioned:
		tenant.Status.Phase = cairnobsv1alpha1.PhaseActive
		condStatus = metav1.ConditionTrue
		reason, message = "Provisioned", fmt.Sprintf("ClickHouse database %q is provisioned", tenant.Status.ClickHouseDatabaseName)
	default:
		tenant.Status.Phase = cairnobsv1alpha1.PhaseProvisioning
	}

	tenant.Status.ObservedGeneration = tenant.Generation
	meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               cairnobsv1alpha1.ConditionReady,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: tenant.Generation,
	})

	if err := r.Status().Update(ctx, &tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating tenant status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cairnobsv1alpha1.Tenant{}).
		Complete(r)
}
