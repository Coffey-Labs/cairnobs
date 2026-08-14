// This file is a checklist, not a fully passing test suite -- it exists
// so the four adversarial probes /docs/phase-4-isolation-design.md's
// "Verification plan for this design specifically" section names for
// Phase 4 task 8 have a permanent, grep-able home in the test tree.
//
// All four are now closed:
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
//   - Item 4 (an evaluator tick mid-provisioning must be refused, not
//     served): closed on both storage engines, though the two needed
//     genuinely different fixes once actually investigated.
//     enterprise/internal/chrunner's fail-closed behavior turned out to
//     already be structural -- a tenant not yet active+credentialed is
//     simply absent from the immutable connection map enterprise-api's
//     main.go builds at startup from
//     rbacstore.ListProvisionedDataSources (itself covered by
//     rbacstore_test.go's
//     TestListProvisionedDataSourcesExcludesUnprovisionedAndInactive),
//     so "mid-provisioning" and "entirely unknown tenant" collapse to
//     the identical RunSQL map-lookup-miss path --
//     enterprise/internal/chrunner/chrunner_test.go's
//     TestRegistryRefusesMidProvisioningTenant proves this without
//     needing Docker (an empty DataSource list never dials ClickHouse),
//     complementing TestRegistryRefusesUnknownTenant's live-ClickHouse
//     version of the same property.
//
//     Tantivy was a real, different gap, not just an unverified
//     assumption: search/src/registry.rs's IndexRegistry opens-or-
//     creates an index for *any* syntactically-valid tenant_id on first
//     request, because it's a separate process with no Postgres access
//     and structurally can't know which tenants are actually
//     provisioned. Without a check upstream, a query against a
//     mid-provisioning tenant would have silently succeeded with zero
//     results from a freshly-created empty index -- "ambient success"
//     indistinguishable from "no matching logs," exactly the failure
//     mode this item worried about. Fixed by adding
//     enterprise/internal/searchclient.TenantChecker (backed by
//     rbacstore.TenantIsActive): Client.Search now refuses before the
//     gRPC call ever goes out if the tenant isn't active. Covered by
//     enterprise/internal/searchclient/searchclient_test.go's
//     TestSearchRefusesMidProvisioningTenant (Docker-free, real
//     in-process gRPC server) and rbacstore_test.go's
//     TestTenantIsActive/TestTenantIsActiveNonexistentTenant
//     (live-Postgres, skip-gated).
//
// Scope boundary all four items share: they prove *read* isolation
// given tenant-scoped data exists -- they do not prove ingest/write-path
// tenancy, which doesn't exist yet (every record ingest produces lands
// in the single shared ClickHouse database and Tantivy index regardless
// of tenant) -- see /docs/security/threat-model.md.
package queryapi
