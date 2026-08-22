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
provider "cairnobs" {
  endpoint = "http://localhost:8080"
}

resource "cairnobs_dashboard" "test" {
  name = "Panel Acceptance Test Dashboard"
}

resource "cairnobs_dashboard_panel" "test" {
  dashboard_id = cairnobs_dashboard.test.id
  title        = "Errors over time"
  query        = "status>=500 | timechart count"
  viz_type     = "line"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("cairnobs_dashboard_panel.test", "dashboard_id", "cairnobs_dashboard.test", "id"),
					resource.TestCheckResourceAttr("cairnobs_dashboard_panel.test", "title", "Errors over time"),
					resource.TestCheckResourceAttr("cairnobs_dashboard_panel.test", "viz_type", "line"),
					resource.TestCheckResourceAttrSet("cairnobs_dashboard_panel.test", "id"),
					// Left unset in config -- must come back as the
					// API's own default ("{}"), same "API default, not
					// a duplicated Terraform-side one" reasoning
					// cairnobs_dashboard's default_earliest/default_latest
					// use.
					resource.TestCheckResourceAttr("cairnobs_dashboard_panel.test", "viz_config", "{}"),
				),
			},
			{
				// Update: unlike cairnobs_alert_rule/cairnobs_notification_target,
				// this really is an in-place update -- api/dashboards.Handler
				// has a real PUT for panels.
				Config: `
provider "cairnobs" {
  endpoint = "http://localhost:8080"
}

resource "cairnobs_dashboard" "test" {
  name = "Panel Acceptance Test Dashboard"
}

resource "cairnobs_dashboard_panel" "test" {
  dashboard_id = cairnobs_dashboard.test.id
  title        = "Errors over time (renamed)"
  query        = "status>=500 | timechart count"
  viz_type     = "line"
}
`,
				Check: resource.TestCheckResourceAttr("cairnobs_dashboard_panel.test", "title", "Errors over time (renamed)"),
			},
			{
				// "dashboard_id/panel_id" -- see ImportState's doc
				// comment on splitImportID for why a bare panel ID
				// isn't enough.
				ResourceName: "cairnobs_dashboard_panel.test",
				ImportState:  true,
				ImportStateIdFunc: func(s *tfstate.State) (string, error) {
					rs, ok := s.RootModule().Resources["cairnobs_dashboard_panel.test"]
					if !ok {
						return "", fmt.Errorf("cairnobs_dashboard_panel.test not found in state")
					}
					return rs.Primary.Attributes["dashboard_id"] + "/" + rs.Primary.Attributes["id"], nil
				},
				ImportStateVerify: true,
			},
		},
	})
}
