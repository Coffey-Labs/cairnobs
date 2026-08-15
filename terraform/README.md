# terraform-provider-sentry

Sentry's Terraform provider -- `CLAUDE.md`'s "Repo conventions" section
names this a first-class deliverable alongside `sentryctl`
("CLI and Terraform provider are first-class, not afterthoughts"), but
no phase before this one had actually built any of it. Two resources so
far (`sentry_dashboard`, `sentry_alert_rule`), not a finished provider,
built on [HashiCorp's `terraform-plugin-framework`][framework] (the
actively-developed library, not the legacy SDKv2 -- there's no existing
provider code here to migrate, so there's no reason to start on the
framework HashiCorp itself steers new providers away from).

[framework]: https://developer.hashicorp.com/terraform/plugin/framework

## Why these two resources first

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
dashboards pattern against the same endpoint.

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

resource "sentry_alert_rule" "example" {
  name                    = "Checkout 5xx spike"
  query                   = "service=checkout status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 50
  eval_interval_seconds   = 60
  notification_target_id  = "target-abc123" # must already exist -- see "Also not built" below
}
```

Supports `terraform import sentry_alert_rule.example <rule-id>`.

**`sentry_alert_rule` is create/destroy only, not update-in-place.**
`alerting`'s REST API has no `PUT /rules/{id}` at all -- confirmed down
to `rulestore.Store`, which has `Create`/`List`/`Get`/`Delete` but no
`Update` method to even wire one to, a real, pre-existing gap in
`alerting`'s own API, not something specific to Terraform. Every
attribute on this resource carries a `RequiresReplace` plan modifier, so
changing anything (the query, the threshold, even just `description`)
destroys and recreates the rule rather than updating it -- which also
resets `alert_state`/delivery-log continuity for that rule, a real
operational side effect worth knowing about before relying on this in
a pipeline that changes rules often. Faking an in-place update via
delete-then-recreate inside the resource itself was considered and
rejected for the same reason: it would hide that side effect instead of
surfacing it in the plan output the way `RequiresReplace` does. Adding a
real `PUT /rules/{id}` to `alerting` would remove this constraint, but
is a change to a different module's REST API, out of scope for this
pass.

**Also not built, all real and disclosed, not attempted here:**
- Notification targets (`/alerting`'s `POST`/`GET`/`DELETE /targets`) --
  a `sentry_alert_rule`'s `notification_target_id` has to name a target
  created some other way (`sentryctl`, `curl`, the web UI) until this
  exists.
- Tenant/RBAC resources (`enterprise-auth`'s tenant/membership/grant
  surface) -- meaningfully different auth model (offline operator flags
  today, not a stable REST API a provider could safely drive
  idempotently -- see `/enterprise/README.md`'s "Bootstrapping a tenant"
  section) and Phase 4 commercial licensing, so this would need its own
  design pass, not just "add another resource file."
- Data sources (read-only lookup by ID/name) for either resource --
  straightforward given the resources already exist, just not built
  yet.
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
out-of-band" convention correctly, and (for rules) proving the
`GET /rules/{id}` response's promoted `RuleWithState` fields plus an
extra `"state"` key this client deliberately has no field for still
parse cleanly.
`internal/provider/provider_test.go` validates the provider and both
resource schemas are internally well-formed (attribute names, the
`Required`/`Computed` split) without needing a Terraform binary or a
live `api`/`alerting` service at all.

`internal/provider/dashboard_resource_test.go`'s
`TestAccDashboardResource_basic` and
`internal/provider/alert_rule_resource_test.go`'s
`TestAccAlertRuleResource_basic` are real acceptance tests using
[`terraform-plugin-testing`][testing] -- skipped unless `TF_ACC=1` is
set, that framework's own standard convention, the same shape every
other live-infrastructure test in this repo uses (`docker`-gated env
vars for Postgres/ClickHouse tests elsewhere). The alert rule test also
uses a `plancheck.ExpectResourceAction` assertion proving a config
change actually plans a destroy-then-create, not an in-place update --
the concrete, checked version of the "create/destroy only" design
decision documented above, not just a claim in a comment. Even with
`TF_ACC=1` both tests also need real running `api`/`alerting` services
(Postgres + ClickHouse) to apply against, which this environment has no
Docker access to bring up -- **not run here**, same disclosed gap as
every other live-infra test across this repo (see
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
