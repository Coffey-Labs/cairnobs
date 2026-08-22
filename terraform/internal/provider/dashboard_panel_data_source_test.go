package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Same skip-gated-not-faked posture as TestAccDashboardResource_basic --
// see that test's doc comment.
func TestAccDashboardPanelDataSource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "cairnobs" {
  endpoint = "http://localhost:8080"
}

resource "cairnobs_dashboard" "test" {
  name = "Panel Data Source Test Dashboard"
}

resource "cairnobs_dashboard_panel" "test" {
  dashboard_id = cairnobs_dashboard.test.id
  title        = "Errors over time"
  query        = "status>=500 | timechart count"
  viz_type     = "line"
}

data "cairnobs_dashboard_panel" "test" {
  dashboard_id = cairnobs_dashboard.test.id
  id           = cairnobs_dashboard_panel.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.cairnobs_dashboard_panel.test", "title", "cairnobs_dashboard_panel.test", "title"),
					resource.TestCheckResourceAttrPair("data.cairnobs_dashboard_panel.test", "query", "cairnobs_dashboard_panel.test", "query"),
					resource.TestCheckResourceAttrPair("data.cairnobs_dashboard_panel.test", "viz_type", "cairnobs_dashboard_panel.test", "viz_type"),
				),
			},
		},
	})
}
