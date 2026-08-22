package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// testAccProtoV6ProviderFactories wires this package's own provider
// implementation into terraform-plugin-testing's acceptance-test
// runner -- HashiCorp's standard pattern, one factory reused by every
// acceptance test in this package.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"cairnobs": providerserver.NewProtocol6WithError(New("test")()),
}

// The acceptance test below is gated the same way every other live-
// infrastructure test in this repo is (skip-gated, not deleted or
// faked) -- terraform-plugin-testing's own resource.Test already skips
// unless TF_ACC=1 is set, the framework's standard convention, and it
// additionally needs a real running api service (Docker/Postgres this
// environment doesn't have access to -- see /docs/phase-4-runbook.md's
// "Verification status" section for the same disclosed gap everywhere
// else in this codebase). "The test exists and is correct Go" is not
// the same claim as "this resource has been applied for real," per this
// repo's established honesty discipline.
func TestAccDashboardResource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "cairnobs" {
  endpoint = "http://localhost:8080"
}

resource "cairnobs_dashboard" "test" {
  name        = "Acceptance Test Dashboard"
  description = "created by TestAccDashboardResource_basic"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("cairnobs_dashboard.test", "name", "Acceptance Test Dashboard"),
					resource.TestCheckResourceAttr("cairnobs_dashboard.test", "description", "created by TestAccDashboardResource_basic"),
					resource.TestCheckResourceAttrSet("cairnobs_dashboard.test", "id"),
					resource.TestCheckResourceAttrSet("cairnobs_dashboard.test", "tenant_id"),
					// Left unset in config -- must come back as the
					// server's own defaults (store.go: "-1h"/"now"), not
					// an empty string, proving the Optional+Computed
					// schema round-trips the server's default rather
					// than fighting it with a Terraform-side one.
					resource.TestCheckResourceAttr("cairnobs_dashboard.test", "default_earliest", "-1h"),
					resource.TestCheckResourceAttr("cairnobs_dashboard.test", "default_latest", "now"),
				),
			},
			{
				// Update: name change should apply in place, not
				// replace (no RequiresReplace plan modifier on name).
				Config: `
provider "cairnobs" {
  endpoint = "http://localhost:8080"
}

resource "cairnobs_dashboard" "test" {
  name        = "Renamed Dashboard"
  description = "created by TestAccDashboardResource_basic"
}
`,
				Check: resource.TestCheckResourceAttr("cairnobs_dashboard.test", "name", "Renamed Dashboard"),
			},
			{
				// Import: re-reads by ID alone and must match what's in
				// state, proving Read()'s server round trip agrees with
				// what Create()/Update() last wrote.
				ResourceName:      "cairnobs_dashboard.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}
