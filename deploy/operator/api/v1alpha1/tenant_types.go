package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantPhase mirrors the provisioning state machine from
// /docs/phase-4-isolation-design.md: every tenant-resolution path
// elsewhere must refuse to serve a tenant not in PhaseActive, checked
// server-side (today, against enterprise/internal/rbacstore's tenants
// table -- this CR is a K8s-native *view* of that same state machine,
// kept honest rather than a second, independently-guessed source of
// truth. `enterprise-api -provision-tenant` (the actual actor -- real
// `CREATE DATABASE`/`CREATE USER`/`GRANT` calls against ClickHouse, and
// the real rbacstore writes) is the only writer of
// TenantStatus.ClickHouseDatabaseName/ClickHouseSecretRef/
// TantivyIndexPath, once real provisioning succeeds -- see
// internal/controller/tenant_controller.go's doc comment for how
// TenantReconciler derives Phase/Conditions from those fields rather
// than fabricating its own "provisioned" claim. This is the "lightweight
// unification" named in CLAUDE.md/docs/phase-4-runbook.md's "two
// independent provisioning mechanisms" gap: -provision-tenant stays the
// real actor; this operator's reconcile loop never touches Postgres or
// ClickHouse and gained no new credentials.
type TenantPhase string

const (
	PhaseProvisioning   TenantPhase = "Provisioning"
	PhaseActive         TenantPhase = "Active"
	PhaseSuspended      TenantPhase = "Suspended"
	PhaseDeprovisioning TenantPhase = "Deprovisioning"
)

// TenantSpec is the desired state -- an operator/admin's intent, set via
// `kubectl apply` or (per the Helm chart's templates/tenants.yaml) a
// values.yaml `tenants:` entry.
type TenantSpec struct {
	// DisplayName is human-readable only -- the Tenant object's own Name
	// (metav1.ObjectMeta) is the stable identifier, matching
	// rbacstore.Tenant.ID's "slug, not UUID" reasoning (see
	// /docs/phase-4-rbac-design.md's schema section) so this CRD's name
	// can be the same string used elsewhere (ClickHouse database name,
	// rbacstore tenant ID) without a translation layer.
	// +kubebuilder:validation:Required
	DisplayName string `json:"displayName"`

	// Suspended is the admin-facing lever for the Suspended phase (e.g.
	// an incident-response or billing action) -- distinct from
	// Provisioning/Deprovisioning, which the controller drives from
	// object lifecycle (creation, deletion), not from this field.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
}

// TenantStatus is observed state, with split ownership as of the
// "lightweight unification" (see TenantPhase's doc comment):
// ClickHouseDatabaseName/ClickHouseSecretRef/TantivyIndexPath are
// written only by enterprise-api's -provision-tenant, once real
// ClickHouse provisioning actually succeeds -- this controller only
// reads them (to compute Phase/Conditions) and never invents a value
// for them. Phase/Conditions/ObservedGeneration remain
// controller-written, computed fresh on every reconcile from Spec plus
// whatever -provision-tenant has (or hasn't) reported.
type TenantStatus struct {
	// +optional
	Phase TenantPhase `json:"phase,omitempty"`

	// ClickHouseDatabaseName is set by enterprise-api -provision-tenant
	// once ClickHouse provisioning for this tenant actually succeeds --
	// empty means "not yet provisioned," which TenantReconciler reads as
	// PhaseProvisioning (see tenant_controller.go). Today it's always
	// equal to the Tenant's own Name (see task 2's design: no tenant
	// traffic authenticates as ClickHouse's `default` user, and a
	// database name that could diverge from the tenant identifier is a
	// bookkeeping foot-gun this type avoids by construction) but isn't
	// itself computed by this package -- -provision-tenant sets it
	// directly from what it actually created.
	// +optional
	ClickHouseDatabaseName string `json:"clickHouseDatabaseName,omitempty"`

	// ClickHouseSecretRef names the Secret (same namespace) holding this
	// tenant's dedicated, narrowly-granted ClickHouse credentials --
	// created by enterprise-api -provision-tenant (enterprise/internal/
	// tenantcrd), owned by this Tenant object via an OwnerReference so
	// K8s garbage-collects it on Tenant deletion regardless of which
	// process created it. This controller no longer creates or manages
	// any Secret itself -- see tenant_controller.go's doc comment for
	// why a controller-generated placeholder credential (the pre-
	// unification behavior) was actively misleading, not just
	// incomplete. Never the cluster-wide CLICKHOUSE_PASSWORD
	// docker-compose.yml uses today.
	// +optional
	ClickHouseSecretRef string `json:"clickHouseSecretRef,omitempty"`

	// TantivyIndexPath is this tenant's index directory under the shared
	// search-index PVC -- see /docs/phase-4-isolation-design.md's
	// Tantivy index-per-tenant section.
	// +optional
	TantivyIndexPath string `json:"tantivyIndexPath,omitempty"`

	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ConditionReady is the one condition type this controller sets today --
// more (e.g. ClickHouseProvisioned, once internal/tenantprovision
// exists) are additive future work, not a breaking change to this type.
const ConditionReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tenant is the K8s-native representation of one Sentry tenant's
// deployment-topology state -- see this file's package-level doc
// comment for what it does and does not manage today.
type Tenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantSpec   `json:"spec,omitempty"`
	Status TenantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantList is a list of Tenant.
type TenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tenant{}, &TenantList{})
}
