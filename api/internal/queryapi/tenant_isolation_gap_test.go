// This file is a checklist, not a passing test suite -- it exists so
// the four adversarial probes /docs/phase-4-isolation-design.md's
// "Verification plan for this design specifically" section names for
// Phase 4 task 8 have a permanent, grep-able home in the test tree,
// even though none of them can run for real yet.
//
// Why they can't run: every one of these probes needs a *per-tenant*
// ClickHouse user/database or Tantivy index to attack -- and none
// exist. api/internal/querylang/executor.SQLRunner/SearchClient (the
// only two interfaces api/internal/queryapi.Handler talks to) carry no
// tenant field at all, confirmed by reading both interfaces; neither
// does proto/sentry/search/v1/search.proto's SearchRequest. See
// /docs/security/threat-model.md's "Read this first" section for the
// full writeup -- there is currently exactly one shared ClickHouse
// connection and one shared Tantivy index for every tenant, so "does
// tenant A's connection leak tenant B's data" has no meaningful
// operational answer yet: there's only one connection.
//
// Each Skip below names precisely what has to exist before that test
// can be written for real (enterprise/internal/tenantprovision,
// enterprise/internal/chrunner, enterprise/internal/searchclient -- all
// still unbuilt, per the Phase 4 task 5 summary). Turning a Skip here
// into a real assertion is the acceptance criterion for those packages,
// not a nice-to-have follow-up.
package queryapi

import "testing"

func TestAdversarial_ClickHouseUserCannotReadOtherTenantDatabaseByFullyQualifiedName(t *testing.T) {
	t.Skip("BLOCKED on enterprise/internal/tenantprovision + enterprise/internal/chrunner: " +
		"needs two real per-tenant ClickHouse users/databases to attempt " +
		"`SELECT * FROM other_tenant_db.logs` against. See " +
		"/docs/phase-4-isolation-design.md's verification plan, item 1.")
}

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
