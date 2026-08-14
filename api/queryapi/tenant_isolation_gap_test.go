// This file is a checklist, not a fully passing test suite -- it exists
// so the four adversarial probes /docs/phase-4-isolation-design.md's
// "Verification plan for this design specifically" section names for
// Phase 4 task 8 have a permanent, grep-able home in the test tree.
//
// Item 1 (fully-qualified cross-tenant raw SQL) is no longer blocked:
// enterprise/internal/tenantprovision and enterprise/internal/chrunner
// now exist, and both have real, passing (when run against a live
// ClickHouse) tests for exactly this probe --
// enterprise/internal/tenantprovision/tenantprovision_test.go's
// TestProvisionedUserCannotReadOtherTenantDatabase (at the raw
// ClickHouse-user layer) and enterprise/internal/chrunner/
// chrunner_test.go's TestRegistryTenantCannotReadOtherTenantEvenViaRawSQL
// (through the actual query-execution code path api/queryapi.Handler
// calls in production, when fronted by enterprise/cmd/enterprise-api
// instead of plain api/cmd/api). Nothing to assert here anymore for
// item 1 -- see those two tests instead.
//
// Items 2-4 remain blocked, for the reasons each Skip below states.
// Note the scope boundary this leaves: even with chrunner wired in,
// there is still exactly one shared Tantivy index for every tenant
// (enterprise/internal/searchclient, the Tantivy-side equivalent of
// chrunner, is unbuilt) -- see /docs/security/threat-model.md.
package queryapi

import "testing"

func TestAdversarial_ClickHouseUserCannotReadSystemTables(t *testing.T) {
	t.Skip("BLOCKED on enterprise/internal/tenantprovision: needs a real per-tenant " +
		"ClickHouse user to attempt `SELECT * FROM system.query_log`, " +
		"`system.tables`, `SHOW DATABASES` against, and confirm system.* " +
		"access was actually revoked (not just assumed from ClickHouse's " +
		"default template -- task 2's finding was that this is " +
		"version-dependent and must be checked live, not read from docs). " +
		"See /docs/phase-4-isolation-design.md's verification plan, item 2.")
}

func TestAdversarial_TantivySearchExcludesOtherTenantsMatchingResults(t *testing.T) {
	t.Skip("BLOCKED on enterprise/internal/searchclient: needs two real " +
		"per-tenant Tantivy indices, one seeded with a term, to confirm a " +
		"search scoped to the other tenant returns zero hits for that term " +
		"even though the term exists in the other index. See " +
		"/docs/phase-4-isolation-design.md's verification plan, item 3.")
}

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
