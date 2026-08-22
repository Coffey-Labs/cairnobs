terraform {
  required_providers {
    cairnobs = {
      source = "registry.terraform.io/cairnobs/cairnobs"
    }
  }
}

variable "cairnobs_api_token" {
  type      = string
  default   = null
  sensitive = true
}

provider "cairnobs" {
  # Both optional -- default to $CAIRNOBS_API_ENDPOINT/$CAIRNOBS_API_TOKEN,
  # then http://localhost:8080/no token, matching cairnobsctl's own
  # defaults (cli/cmd/cairnobsctl/main.go). token is only required once a
  # deployment turns on enterprise-auth enforcement.
  endpoint = "http://localhost:8080"
  token    = var.cairnobs_api_token
}
