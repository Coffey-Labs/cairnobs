resource "cairnobs_dashboard" "checkout_errors" {
  name = "Checkout Errors"
}

resource "cairnobs_dashboard_panel" "error_rate" {
  dashboard_id = cairnobs_dashboard.checkout_errors.id
  title        = "5xx rate over time"
  query        = "service=checkout status>=500 | timechart count"
  viz_type     = "line"
  position_x   = 0
  position_y   = 0
  width        = 6
  height       = 4
}

# Unlike cairnobs_alert_rule/cairnobs_notification_target, this resource
# supports a real in-place update -- api/dashboards.Handler has a real
# PUT /dashboards/{id}/panels/{panelId}. Only dashboard_id forces a
# destroy-and-recreate (there's no API operation to move a panel between
# dashboards).
