# terraform-provider-sentry

Sentry's Terraform provider -- `CLAUDE.md`'s "Repo conventions" section
names this a first-class deliverable alongside `sentryctl`
("CLI and Terraform provider are first-class, not afterthoughts"), but
no phase before this one had actually built any of it. Three resources
so far (`sentry_dashboard`, `sentry_alert_rule`,
`sentry_notification_target`), not a finished provider, built on
[HashiCorp's `terraform-plugin-framework`][framework] (the
actively-developed library, not the legacy SDKv2 -- there's no existing
provider code here to migrate, so there's no reason to start on the
framework HashiCorp itself steers new providers away from).

[framework]: https://developer.hashicorp.com/terraform/plugin/framework

## Why these three resources first

`cli/README.md` already frames the dashboards REST contract this way:
`POST /dashboards`, `GET`/`PUT`/`DELETE /dashboards/{id}` are "the seed
of a future Terraform provider: one JSON contract, multiple callers (web
export, CLI apply, eventually a provider)." This provider is that third
caller -- `internal/provider/client.go` talks the exact same JSON shape
`sentryctl dashboards apply` and the web UI's Export JSON button already
use against `api/dashboards.Handler`, not a new contract invented for
Terraform's sake. `sentry_alert_rule` follows against `alerting`'s own
`POST`/`GET`/`DELETE /rules[/{id}]` -- the natural second resource, and
a real second service (`alerting` is a genuinely separate deployment
from `api`, its own base URL), so building it second exercised that this
provider can talk to more than one Sentry service, not just repeat the
dashboards pattern against the same endpoint. `sentry_notification_target`
rounds these two out -- `sentry_alert_rule.notification_target_id` needs
something to actually point at, and without this resource that id could
only ever come from outside Terraform (`sentryctl`, `curl`, the web UI),
undermining the point of managing rules as code at all.

## What's built

```hcl
terraform {
  required_providers {
    sentry = {
      source = "registry.terraform.io/sentry/sentry"
    }
  }
}

provider "sentry" {
  endpoint = "http://localhost:8080" # or $SENTRY_API_ENDPOINT
  token    = var.sentry_api_token    # or $SENTRY_API_TOKEN -- optional, only needed once enterprise-auth enforcement is on
}

resource "sentry_dashboard" "example" {
  name        = "Checkout Errors"
  description = "5xx rate and latency for the checkout service"
  # default_earliest/default_latest are optional -- left unset, the API
  # itself defaults them ("-1h"/"now"); this resource deliberately
  # doesn't hardcode a matching Terraform-side default, so the API stays
  # the one source of truth for what "unset" means (see the schema's
  # doc comment in internal/provider/dashboard_resource.go).
}
```

Supports `terraform import sentry_dashboard.example <dashboard-id>`.

**Panels are not managed by this resource.** `api/dashboards.Handler`
exposes panel CRUD as its own endpoints
(`POST`/`PUT`/`DELETE /dashboards/{id}/panels[/{panelId}]`), a
genuinely separate resource shape (a panel belongs to exactly one
dashboard, has its own lifecycle, and the query-language/viz-config
fields deserve their own attribute validation) -- scoped out of this
first pass deliberately, not an oversight. A `sentry_dashboard_panel`
resource (or a panels list block on this one -- an open design question,
not yet decided) is real, disclosed future work.

```hcl
provider "sentry" {
  endpoint          = "http://localhost:8080" # or $SENTRY_API_ENDPOINT
  alerting_endpoint = "http://localhost:8081" # or $SENTRY_ALERTING_API_ENDPOINT -- alerting is a separate service, own base URL
  token             = var.sentry_api_token    # or $SENTRY_API_TOKEN -- shared by both services, same as sentryctl's one $SENTRYCTL_TOKEN
}

resource "sentry_notification_target" "ops" {
  name        = "Ops Webhook"
  kind        = "webhook"
  webhook_url = "https://ops.example.com/hooks/sentry-alerts"
}

resource "sentry_alert_rule" "example" {
  name                    = "Checkout 5xx spike"
  query                   = "service=checkout status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 50
  eval_interval_seconds   = 60
  notification_target_id  = sentry_notification_target.ops.id
}
```

Supports `terraform import sentry_alert_rule.example <rule-id>` and
`terraform import sentry_notification_target.example <target-id>`.

**`sentry_alert_rule` and `sentry_notification_target` are both
create/destroy only, not update-in-place.** `alerting`'s REST API has no
`PUT /rules/{id}` or `PUT /targets/{id}` at all -- confirmed down to
`rulestore.Store`/`notifystore.Store`, both of which have
`Create`/`List`/`Get`/`Delete` but no `Update` method to even wire one
to, a real, pre-existing gap in `alerting`'s own API, not something
specific to Terraform. Every attribute on both resources carries a
`RequiresReplace` plan modifier, so changing anything (a rule's query or
threshold, a target's webhook URL, even just a `description`) destroys
and recreates it rather than updating it in place -- for rules, that
also resets `alert_state`/delivery-log continuity, a real operational
side effect worth knowing about before relying on this in a pipeline
that changes rules often. Faking an in-place update via
delete-then-recreate inside either resource was considered and rejected
for the same reason: it would hide that side effect instead of
surfacing it in the plan output the way `RequiresReplace` does. Adding
real `PUT` endpoints to `alerting` would remove this constraint, but is
a change to a different module's REST API, out of scope for this pass.

**`sentry_notification_target`'s `secret` attribute is `Sensitive` but
still lands in Terraform state in plaintext.** `alerting`'s own
`GET /targets/{id}` returns `secret` unredacted (confirmed in
`notifystore/store.go` -- no redaction at the store or handler layer, an
existing property of `alerting`'s API, not introduced by this
provider), and a resource has to store whatever `Read` returns to avoid
Terraform showing a permanent diff. `Sensitive: true` keeps it out of
plan/apply console output; it does not keep it out of the state file --
the standard, well-known Terraform caveat for any sensitive attribute
(encrypt the state backend, restrict who can read it), named here rather
than left implicit.

**Also not built, all real and disclosed, not attempted here:**
- Tenant/RBAC resources (`enterprise-auth`'s tenant/membership/grant
  surface) -- meaningfully different auth model (offline operator flags
  today, not a stable REST API a provider could safely drive
  idempotently -- see `/enterprise/README.md`'s "Bootstrapping a tenant"
  section) and Phase 4 commercial licensing, so this would need its own
  design pass, not just "add another resource file."
- Data sources (read-only lookup by ID/name) for any of the three
  resources -- straightforward given the resources already exist, just
  not built yet.
- Dashboard panels (see "Panels are not managed by this resource" above).
- Publishing to the real Terraform Registry -- `main.go`'s `Address`
  (`registry.terraform.io/sentry/sentry`) is the address a real
  publication would use, but nothing has actually been published; local
  use is via `~/.terraformrc`'s `dev_overrides` (see "Building &
  testing" below) or a local provider mirror.

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

`internal/provider/client_test.go` runs real HTTP round trips against a
`httptest.Server` (same pattern `cli/cmd/sentryctl`'s own tests use
against the same `api/dashboards`/`alerting` endpoints) -- real request
construction (method, path, `Authorization` header, JSON body), real
response parsing, including the 404-vs-other-error distinction
`Read`/`Delete` need to implement Terraform's "resource deleted
out-of-band" convention correctly, (for rules) proving the
`GET /rules/{id}` response's promoted `RuleWithState` fields plus an
extra `"state"` key this client deliberately has no field for still
parse cleanly, and (for targets) proving `secret` really does come back
unredacted -- documenting real `alerting` behavior with a test, not just
a comment, so a future change to that behavior would be caught here too.
`internal/provider/provider_test.go` validates the provider and all
three resource schemas are internally well-formed (attribute names, the
`Required`/`Computed`/`Sensitive` split) without needing a Terraform
binary or a live `api`/`alerting` service at all.

`internal/provider/dashboard_resource_test.go`'s
`TestAccDashboardResource_basic`,
`internal/provider/alert_rule_resource_test.go`'s
`TestAccAlertRuleResource_basic`, and
`internal/provider/notification_target_resource_test.go`'s
`TestAccNotificationTargetResource_basic` are real acceptance tests
using [`terraform-plugin-testing`][testing] -- skipped unless `TF_ACC=1`
is set, that framework's own standard convention, the same shape every
other live-infrastructure test in this repo uses (`docker`-gated env
vars for Postgres/ClickHouse tests elsewhere). The rule and target tests
both use a `plancheck.ExpectResourceAction` assertion proving a config
change actually plans a destroy-then-create, not an in-place update --
the concrete, checked version of the "create/destroy only" design
decision documented above, not just a claim in a comment. Even with
`TF_ACC=1` all three tests also need real running `api`/`alerting`
services (Postgres + ClickHouse) to apply against, which this
environment has no Docker access to bring up -- **not run here**, same
disclosed gap as every other live-infra test across this repo (see
`/docs/phase-4-runbook.md`'s "Verification status" section for the
project-wide version of this same caveat). "The test exists and is
correct Go" is not the same claim as "this resource has been applied
for real."

[testing]: https://developer.hashicorp.com/terraform/plugin/testing

```sh
# local dev override, so `terraform` picks up a locally-built binary
# instead of trying to download from the registry (which nothing has
# been published to -- see "What's built" above)
go build -o terraform-provider-sentry .
cat <<'EOF' >> ~/.terraformrc
provider_installation {
  dev_overrides {
    "registry.terraform.io/sentry/sentry" = "/absolute/path/to/this/directory"
  }
  direct {}
}
EOF
```
