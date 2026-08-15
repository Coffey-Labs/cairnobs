package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Same skip-gated-not-faked posture as
// TestAccNotificationTargetResource_basic -- see that test's doc
// comment.
func TestAccNotificationTargetDataSource_basic(t *testing.T) {
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
  name        = "Data Source Test Target"
  kind        = "webhook"
  webhook_url = "https://example.com/hook"
  secret      = "test-secret"
}

data "sentry_notification_target" "test" {
  id = sentry_notification_target.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.sentry_notification_target.test", "name", "sentry_notification_target.test", "name"),
					resource.TestCheckResourceAttrPair("data.sentry_notification_target.test", "webhook_url", "sentry_notification_target.test", "webhook_url"),
					// Real, not papered over -- secret round-trips
					// unredacted (see the resource test's equivalent
					// comment).
					resource.TestCheckResourceAttrPair("data.sentry_notification_target.test", "secret", "sentry_notification_target.test", "secret"),
				),
			},
		},
	})
}
