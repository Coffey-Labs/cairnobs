data "sentry_dashboard" "checkout_errors" {
  id = "an-existing-dashboard-id"
}

output "checkout_errors_default_earliest" {
  value = data.sentry_dashboard.checkout_errors.default_earliest
}
