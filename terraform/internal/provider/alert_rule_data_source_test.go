package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Same skip-gated-not-faked posture as TestAccAlertRuleResource_basic --
// see that test's doc comment. notification_target_id is a placeholder,
// same caveat as the resource test.
func TestAccAlertRuleDataSource_basic(t *testing.T) {
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
  name                    = "Data Source Test Rule"
  query                   = "status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 5
  eval_interval_seconds   = 60
  notification_target_id  = "placeholder-target-id"
}

data "cairnobs_alert_rule" "test" {
  id = cairnobs_alert_rule.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.cairnobs_alert_rule.test", "name", "cairnobs_alert_rule.test", "name"),
					resource.TestCheckResourceAttrPair("data.cairnobs_alert_rule.test", "query", "cairnobs_alert_rule.test", "query"),
					resource.TestCheckResourceAttrPair("data.cairnobs_alert_rule.test", "threshold_value", "cairnobs_alert_rule.test", "threshold_value"),
				),
			},
		},
	})
}
