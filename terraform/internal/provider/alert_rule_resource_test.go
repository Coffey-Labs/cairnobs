package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Same skip-gated-not-faked posture as TestAccDashboardResource_basic --
// see that test's doc comment. notification_target_id below is a
// placeholder: no cairnobs_notification_target resource exists yet (see
// the provider README), so a real run of this test would need a
// pre-existing target id supplied some other way; not a blocker for
// what this test actually proves, since it has never run against a
// live stack in this environment regardless.
func TestAccAlertRuleResource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "cairnobs" {
  endpoint          = "http://localhost:8080"
  alerting_endpoint = "http://localhost:8081"
}

resource "cairnobs_alert_rule" "test" {
  name                    = "Acceptance Test Rule"
  query                   = "status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 5
  eval_interval_seconds   = 60
  notification_target_id  = "placeholder-target-id"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("cairnobs_alert_rule.test", "name", "Acceptance Test Rule"),
					resource.TestCheckResourceAttr("cairnobs_alert_rule.test", "comparator", "gt"),
					resource.TestCheckResourceAttr("cairnobs_alert_rule.test", "threshold_value", "5"),
					resource.TestCheckResourceAttrSet("cairnobs_alert_rule.test", "id"),
					resource.TestCheckResourceAttrSet("cairnobs_alert_rule.test", "tenant_id"),
					// Left unset in config -- must come back as the
					// server's own default (true), same "API default,
					// not a duplicated Terraform-side one" reasoning
					// cairnobs_dashboard's default_earliest/default_latest
					// use.
					resource.TestCheckResourceAttr("cairnobs_alert_rule.test", "enabled", "true"),
					resource.TestCheckResourceAttr("cairnobs_alert_rule.test", "for_minutes", "0"),
				),
			},
			{
				// Proves the "create/destroy only" design decision is
				// real, not just documented: alerting has no PUT
				// /rules/{id}, so every attribute is RequiresReplace,
				// and changing one (here, the threshold) must plan a
				// destroy-then-create, never an in-place update.
				Config: `
provider "cairnobs" {
  endpoint          = "http://localhost:8080"
  alerting_endpoint = "http://localhost:8081"
}

resource "cairnobs_alert_rule" "test" {
  name                    = "Acceptance Test Rule"
  query                   = "status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 10
  eval_interval_seconds   = 60
  notification_target_id  = "placeholder-target-id"
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("cairnobs_alert_rule.test", plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.TestCheckResourceAttr("cairnobs_alert_rule.test", "threshold_value", "10"),
			},
			{
				ResourceName:      "cairnobs_alert_rule.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}
