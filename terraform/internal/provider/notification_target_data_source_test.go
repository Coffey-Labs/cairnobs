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
provider "cairnobs" {
  endpoint          = "http://localhost:8080"
  alerting_endpoint = "http://localhost:8081"
}

resource "cairnobs_notification_target" "test" {
  name        = "Data Source Test Target"
  kind        = "webhook"
  webhook_url = "https://example.com/hook"
  secret      = "test-secret"
}

data "cairnobs_notification_target" "test" {
  id = cairnobs_notification_target.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.cairnobs_notification_target.test", "name", "cairnobs_notification_target.test", "name"),
					resource.TestCheckResourceAttrPair("data.cairnobs_notification_target.test", "webhook_url", "cairnobs_notification_target.test", "webhook_url"),
					// Real, not papered over -- secret round-trips
					// unredacted (see the resource test's equivalent
					// comment).
					resource.TestCheckResourceAttrPair("data.cairnobs_notification_target.test", "secret", "cairnobs_notification_target.test", "secret"),
				),
			},
		},
	})
}
