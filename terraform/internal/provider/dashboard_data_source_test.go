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
provider "cairnobs" {
  endpoint = "http://localhost:8080"
}

resource "cairnobs_dashboard" "test" {
  name        = "Data Source Test Dashboard"
  description = "created by TestAccDashboardDataSource_basic"
}

data "cairnobs_dashboard" "test" {
  id = cairnobs_dashboard.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.cairnobs_dashboard.test", "name", "cairnobs_dashboard.test", "name"),
					resource.TestCheckResourceAttrPair("data.cairnobs_dashboard.test", "tenant_id", "cairnobs_dashboard.test", "tenant_id"),
					resource.TestCheckResourceAttrPair("data.cairnobs_dashboard.test", "default_earliest", "cairnobs_dashboard.test", "default_earliest"),
				),
			},
		},
	})
}
