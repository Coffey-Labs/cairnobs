resource "sentry_dashboard" "checkout_errors" {
  name        = "Checkout Errors"
  description = "5xx rate and latency for the checkout service"

  # Optional -- left unset, the API defaults these to "-1h"/"now"
  # server-side (api/dashboards/store.go). Set explicitly here only to
  # show the attribute; omit it entirely in real usage unless you want
  # something other than the default.
  default_earliest = "-24h"
  default_latest   = "now"
}
