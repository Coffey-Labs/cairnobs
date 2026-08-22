resource "cairnobs_notification_target" "ops_webhook" {
  name        = "Ops Webhook"
  kind        = "webhook"
  webhook_url = "https://ops.example.com/hooks/cairnobs-alerts"

  # Optional. Sensitive -- not printed in plan/apply output, but note
  # it's still stored in Terraform state in plaintext (alerting's own
  # GET /targets/{id} returns it unredacted; see the provider README).
  secret = var.ops_webhook_secret

  # Optional -- must be a JSON object string.
  headers = jsonencode({
    "X-Team" = "platform"
  })
}

variable "ops_webhook_secret" {
  type      = string
  sensitive = true
}

# Create/destroy only -- alerting has no PUT /targets/{id} today, so
# changing any attribute above destroys and recreates the target rather
# than updating it in place. See the provider README for why.
