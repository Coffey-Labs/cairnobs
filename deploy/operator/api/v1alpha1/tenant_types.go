package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantPhase mirrors the provisioning state machine from
// /docs/phase-4-isolation-design.md: every tenant-resolution path
// elsewhere must refuse to serve a tenant not in PhaseActive, checked
// server-side (today, against enterprise/internal/rbacstore's tenants
// table -- this CR is a K8s-native *view* of the same state machine at
// the deployment-topology layer, not a second source of truth. Reconciling
// the two together is exactly the kind of tenant-provisioning wiring
// named as deferred in /docs/phase-4-runbook.md's task 6 section: today
// this operator only manages the K8s-side artifact (a per-tenant
// ClickHouse credential Secret + a ConfigMap recording the tenant's
// database name/index path), not the actual `CREATE DATABASE`/`CREATE
// USER`/`GRANT` calls against ClickHouse -- that's
// enterprise/internal/tenantprovision, still unbuilt.
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

// TenantStatus is observed state -- only the controller writes this.
type TenantStatus struct {
	// +optional
	Phase TenantPhase `json:"phase,omitempty"`

	// ClickHouseDatabaseName is derived (today: same as the Tenant's own
	// Name) rather than settable in Spec -- see task 2's design: no
	// tenant traffic authenticates as ClickHouse's `default` user, and a
	// database name that could diverge from the tenant identifier is a
	// bookkeeping foot-gun this type avoids by construction.
	// +optional
	ClickHouseDatabaseName string `json:"clickHouseDatabaseName,omitempty"`

	// ClickHouseSecretRef names the Secret (same namespace) holding this
	// tenant's dedicated, narrowly-granted ClickHouse credentials -- see
	// tenant_controller.go's reconcileSecret. Never the cluster-wide
	// CLICKHOUSE_PASSWORD docker-compose.yml uses today.
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
