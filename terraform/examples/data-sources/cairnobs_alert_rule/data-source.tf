data "cairnobs_alert_rule" "checkout_5xx" {
  id = "an-existing-rule-id"
}

output "checkout_5xx_notification_target" {
  value = data.cairnobs_alert_rule.checkout_5xx.notification_target_id
}
