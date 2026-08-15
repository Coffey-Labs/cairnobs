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
provider "sentry" {
  endpoint = "http://localhost:8080"
}

resource "sentry_dashboard" "test" {
  name = "Panel Data Source Test Dashboard"
}

resource "sentry_dashboard_panel" "test" {
  dashboard_id = sentry_dashboard.test.id
  title        = "Errors over time"
  query        = "status>=500 | timechart count"
  viz_type     = "line"
}

data "sentry_dashboard_panel" "test" {
  dashboard_id = sentry_dashboard.test.id
  id           = sentry_dashboard_panel.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.sentry_dashboard_panel.test", "title", "sentry_dashboard_panel.test", "title"),
					resource.TestCheckResourceAttrPair("data.sentry_dashboard_panel.test", "query", "sentry_dashboard_panel.test", "query"),
					resource.TestCheckResourceAttrPair("data.sentry_dashboard_panel.test", "viz_type", "sentry_dashboard_panel.test", "viz_type"),
				),
			},
		},
	})
}
