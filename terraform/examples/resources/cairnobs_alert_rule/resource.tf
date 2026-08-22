resource "cairnobs_notification_target" "ops_webhook" {
  name        = "Ops Webhook"
  kind        = "webhook"
  webhook_url = "https://ops.example.com/hooks/cairnobs-alerts"
}

resource "cairnobs_alert_rule" "checkout_5xx" {
  name                    = "Checkout 5xx spike"
  query                   = "service=checkout status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 50
  eval_interval_seconds   = 60
  for_minutes             = 5
  notification_target_id = cairnobs_notification_target.ops_webhook.id
}

# Create/destroy only -- alerting has no PUT /rules/{id} today, so
# changing any attribute above (including this resource block itself)
# destroys and recreates the rule rather than updating it in place. See
# the provider README for why.
