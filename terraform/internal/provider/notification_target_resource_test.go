package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Same skip-gated-not-faked posture as TestAccDashboardResource_basic /
// TestAccAlertRuleResource_basic -- see those tests' doc comments.
func TestAccNotificationTargetResource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "sentry" {
  endpoint          = "http://localhost:8080"
  alerting_endpoint = "http://localhost:8081"
}

resource "sentry_notification_target" "test" {
  name        = "Acceptance Test Target"
  kind        = "webhook"
  webhook_url = "https://example.com/hook"
  secret      = "test-secret"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sentry_notification_target.test", "name", "Acceptance Test Target"),
					resource.TestCheckResourceAttr("sentry_notification_target.test", "kind", "webhook"),
					resource.TestCheckResourceAttr("sentry_notification_target.test", "secret", "test-secret"),
					resource.TestCheckResourceAttrSet("sentry_notification_target.test", "id"),
					resource.TestCheckResourceAttrSet("sentry_notification_target.test", "tenant_id"),
				),
			},
			{
				// Same create/destroy-only proof as
				// TestAccAlertRuleResource_basic's second step --
				// notifystore.Store has no Update either.
				Config: `
provider "sentry" {
  endpoint          = "http://localhost:8080"
  alerting_endpoint = "http://localhost:8081"
}

resource "sentry_notification_target" "test" {
  name        = "Renamed Target"
  kind        = "webhook"
  webhook_url = "https://example.com/hook"
  secret      = "test-secret"
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sentry_notification_target.test", plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.TestCheckResourceAttr("sentry_notification_target.test", "name", "Renamed Target"),
			},
			{
				// No ImportStateVerifyIgnore for "secret" -- GET
				// really does return it unredacted (see client.go's
				// doc comment and TestGetNotificationTargetReturnsSecretUnredacted),
				// so import-time equality is a real, meaningful
				// assertion here, not one this test has to paper over.
				ResourceName:      "sentry_notification_target.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}
