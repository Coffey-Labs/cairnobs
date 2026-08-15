package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Same skip-gated-not-faked posture as TestAccDashboardResource_basic --
// see that test's doc comment.
func TestAccDashboardDataSource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "sentry" {
  endpoint = "http://localhost:8080"
}

resource "sentry_dashboard" "test" {
  name        = "Data Source Test Dashboard"
  description = "created by TestAccDashboardDataSource_basic"
}

data "sentry_dashboard" "test" {
  id = sentry_dashboard.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.sentry_dashboard.test", "name", "sentry_dashboard.test", "name"),
					resource.TestCheckResourceAttrPair("data.sentry_dashboard.test", "tenant_id", "sentry_dashboard.test", "tenant_id"),
					resource.TestCheckResourceAttrPair("data.sentry_dashboard.test", "default_earliest", "sentry_dashboard.test", "default_earliest"),
				),
			},
		},
	})
}
