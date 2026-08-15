# notification_target_id has to name an already-existing target --
# no sentry_notification_target resource exists yet (see the provider
# README), so create one via sentryctl/curl/the web UI first and pass
# its id in here.
resource "sentry_alert_rule" "checkout_5xx" {
  name                   = "Checkout 5xx spike"
  query                  = "service=checkout status>=500 | stats count"
  condition_type         = "threshold"
  comparator             = "gt"
  threshold_value        = 50
  eval_interval_seconds  = 60
  for_minutes            = 5
  notification_target_id = "target-abc123"
}

# Create/destroy only -- alerting has no PUT /rules/{id} today, so
# changing any attribute above (including this resource block itself)
# destroys and recreates the rule rather than updating it in place. See
# the provider README for why.
