// This file is a checklist, not a fully passing test suite -- it exists
// so the four adversarial probes /docs/phase-4-isolation-design.md's
// "Verification plan for this design specifically" section names for
// Phase 4 task 8 have a permanent, grep-able home in the test tree.
//
// Three of the four are no longer blocked:
//
//   - Item 1 (fully-qualified cross-tenant raw SQL):
//     enterprise/internal/tenantprovision/tenantprovision_test.go's
//     TestProvisionedUserCannotReadOtherTenantDatabase (raw ClickHouse-user
//     layer) and enterprise/internal/chrunner/chrunner_test.go's
//     TestRegistryTenantCannotReadOtherTenantEvenViaRawSQL (the actual
//     query-execution code path, via enterprise/cmd/enterprise-api).
//   - Item 2 (system.query_log/system.tables/SHOW DATABASES):
//     enterprise/internal/tenantprovision/tenantprovision_test.go's
//     TestProvisionedUserCannotReadSystemTables.
//   - Item 3 (Tantivy cross-tenant search): search/src/registry.rs's
//     tenant_index_is_isolated_from_default_and_other_tenants (Rust, the
//     actual per-tenant index registry) and enterprise/internal/
//     searchclient/searchclient_test.go (the Go client that resolves
//     tenant_id from request identity, wire-level verified against a
//     real in-process gRPC server).
//
// Item 4 remains blocked, for the reason its Skip below states. Note the
// scope boundary all three closed items share: they prove *read*
// isolation given tenant-scoped data exists -- they do not prove
// ingest/write-path tenancy, which doesn't exist yet (every record
// ingest produces lands in the single shared ClickHouse database and
// Tantivy index regardless of tenant) -- see
// /docs/security/threat-model.md.
package queryapi

import "testing"

func TestAdversarial_EvaluatorTickMidProvisioningIsRefusedNotServed(t *testing.T) {
	t.Skip("BLOCKED on enterprise/internal/tenantprovision's ordered " +
		"provisioning state machine (CREATE USER -> GRANT -> mark active): " +
		"needs a tenant row that exists but hasn't reached the active gate " +
		"yet, and a simulated /alerting evaluator tick against it, to " +
		"confirm every tenant-resolution path actually checks tenant " +
		"status server-side rather than inferring readiness from ambient " +
		"connection success. See /docs/phase-4-isolation-design.md's " +
		"verification plan, item 4, and its provisioning-gate requirement.")
}
