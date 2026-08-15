terraform {
  required_providers {
    sentry = {
      source = "registry.terraform.io/sentry/sentry"
    }
  }
}

variable "sentry_api_token" {
  type      = string
  default   = null
  sensitive = true
}

provider "sentry" {
  # Both optional -- default to $SENTRY_API_ENDPOINT/$SENTRY_API_TOKEN,
  # then http://localhost:8080/no token, matching sentryctl's own
  # defaults (cli/cmd/sentryctl/main.go). token is only required once a
  # deployment turns on enterprise-auth enforcement.
  endpoint = "http://localhost:8080"
  token    = var.sentry_api_token
}
