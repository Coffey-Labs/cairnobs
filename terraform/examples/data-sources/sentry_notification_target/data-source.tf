# secret comes back unredacted (see the sentry_notification_target
# resource's schema doc comment) -- this data source's "secret"
# attribute is Sensitive for the same reason.
data "sentry_notification_target" "ops_webhook" {
  id = "an-existing-target-id"
}

resource "sentry_alert_rule" "checkout_5xx" {
  name                    = "Checkout 5xx spike"
  query                   = "service=checkout status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 50
  eval_interval_seconds   = 60
  notification_target_id  = data.sentry_notification_target.ops_webhook.id
}
