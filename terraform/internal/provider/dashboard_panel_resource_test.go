package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	tfstate "github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Same skip-gated-not-faked posture as TestAccDashboardResource_basic --
// see that test's doc comment.
func TestAccDashboardPanelResource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "sentry" {
  endpoint = "http://localhost:8080"
}

resource "sentry_dashboard" "test" {
  name = "Panel Acceptance Test Dashboard"
}

resource "sentry_dashboard_panel" "test" {
  dashboard_id = sentry_dashboard.test.id
  title        = "Errors over time"
  query        = "status>=500 | timechart count"
  viz_type     = "line"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("sentry_dashboard_panel.test", "dashboard_id", "sentry_dashboard.test", "id"),
					resource.TestCheckResourceAttr("sentry_dashboard_panel.test", "title", "Errors over time"),
					resource.TestCheckResourceAttr("sentry_dashboard_panel.test", "viz_type", "line"),
					resource.TestCheckResourceAttrSet("sentry_dashboard_panel.test", "id"),
					// Left unset in config -- must come back as the
					// API's own default ("{}"), same "API default, not
					// a duplicated Terraform-side one" reasoning
					// sentry_dashboard's default_earliest/default_latest
					// use.
					resource.TestCheckResourceAttr("sentry_dashboard_panel.test", "viz_config", "{}"),
				),
			},
			{
				// Update: unlike sentry_alert_rule/sentry_notification_target,
				// this really is an in-place update -- api/dashboards.Handler
				// has a real PUT for panels.
				Config: `
provider "sentry" {
  endpoint = "http://localhost:8080"
}

resource "sentry_dashboard" "test" {
  name = "Panel Acceptance Test Dashboard"
}

resource "sentry_dashboard_panel" "test" {
  dashboard_id = sentry_dashboard.test.id
  title        = "Errors over time (renamed)"
  query        = "status>=500 | timechart count"
  viz_type     = "line"
}
`,
				Check: resource.TestCheckResourceAttr("sentry_dashboard_panel.test", "title", "Errors over time (renamed)"),
			},
			{
				// "dashboard_id/panel_id" -- see ImportState's doc
				// comment on splitImportID for why a bare panel ID
				// isn't enough.
				ResourceName: "sentry_dashboard_panel.test",
				ImportState:  true,
				ImportStateIdFunc: func(s *tfstate.State) (string, error) {
					rs, ok := s.RootModule().Resources["sentry_dashboard_panel.test"]
					if !ok {
						return "", fmt.Errorf("sentry_dashboard_panel.test not found in state")
					}
					return rs.Primary.Attributes["dashboard_id"] + "/" + rs.Primary.Attributes["id"], nil
				},
				ImportStateVerify: true,
			},
		},
	})
}
