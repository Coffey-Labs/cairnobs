# terraform-provider-cairnobs

Cairn OBS's Terraform provider -- `PROJECT-SPEC.md`'s "Repo conventions" section
names this a first-class deliverable alongside `cairnobsctl`
("CLI and Terraform provider are first-class, not afterthoughts"), but
no phase before this one had actually built any of it. Four resources
so far (`cairnobs_dashboard`, `cairnobs_dashboard_panel`, `cairnobs_alert_rule`,
`cairnobs_notification_target`), each paired with a read-only data source
of the same name, not a finished provider, built on
[HashiCorp's `terraform-plugin-framework`][framework] (the
actively-developed library, not the legacy SDKv2 -- there's no existing
provider code here to migrate, so there's no reason to start on the
framework HashiCorp itself steers new providers away from).

[framework]: https://developer.hashicorp.com/terraform/plugin/framework

## Why these four resources first

`cli/README.md` already frames the dashboards REST contract this way:
`POST /dashboards`, `GET`/`PUT`/`DELETE /dashboards/{id}` are "the seed
of a future Terraform provider: one JSON contract, multiple callers (web
export, CLI apply, eventually a provider)." This provider is that third
caller -- `internal/provider/client.go` talks the exact same JSON shape
`cairnobsctl dashboards apply` and the web UI's Export JSON button already
use against `api/dashboards.Handler`, not a new contract invented for
Terraform's sake. `cairnobs_dashboard_panel` follows the same contract's
panel endpoints, as its own resource rather than a block nested inside
`cairnobs_dashboard` -- see "Panels are their own resource" below.
`cairnobs_alert_rule` follows against `alerting`'s own `POST`/`GET`/
`DELETE /rules[/{id}]` -- the natural next resource, and a real second
service (`alerting` is a genuinely separate deployment from `api`, its
own base URL), so building it exercised that this provider can talk to
more than one Cairn OBS service, not just repeat the dashboards pattern
against the same endpoint. `cairnobs_notification_target` rounds these
out -- `cairnobs_alert_rule.notification_target_id` needs something to
actually point at, and without this resource that id could only ever
come from outside Terraform (`cairnobsctl`, `curl`, the web UI),
undermining the point of managing rules as code at all.

## What's built

```hcl
terraform {
  required_providers {
    cairnobs = {
      source = "registry.terraform.io/cairnobs/cairnobs"
    }
  }
}

provider "cairnobs" {
  endpoint = "http://localhost:8080" # or $CAIRNOBS_API_ENDPOINT
  token    = var.cairnobs_api_token    # or $CAIRNOBS_API_TOKEN -- optional, only needed once enterprise-auth enforcement is on
}

resource "cairnobs_dashboard" "example" {
  name        = "Checkout Errors"
  description = "5xx rate and latency for the checkout service"
  # default_earliest/default_latest are optional -- left unset, the API
  # itself defaults them ("-1h"/"now"); this resource deliberately
  # doesn't hardcode a matching Terraform-side default, so the API stays
  # the one source of truth for what "unset" means (see the schema's
  # doc comment in internal/provider/dashboard_resource.go).
}
```

Supports `terraform import cairnobs_dashboard.example <dashboard-id>`.

**Panels are their own resource, not a nested block.** `api/dashboards.
Handler` exposes panel CRUD as its own endpoints (`POST`/`PUT`/
`DELETE /dashboards/{id}/panels[/{panelId}]`) -- a panel belongs to
exactly one dashboard, has its own lifecycle, and is created/updated/
deleted independently, never by rewriting a dashboard's whole panel
list, so `cairnobs_dashboard_panel` follows that shape rather than a
nested list block (which would force every panel to be rewritten on any
single panel's change, hiding fine-grained diffs a separate resource
shows naturally):

```hcl
resource "cairnobs_dashboard_panel" "example" {
  dashboard_id = cairnobs_dashboard.example.id
  title        = "5xx rate over time"
  query        = "status>=500 | timechart count"
  viz_type     = "line" # table, line, bar, single_stat, or top_n
  # query_language never accepts "sql" for panels -- the API rejects it
  # outright (dashboards only support pipe-syntax queries, since the
  # time-range picker is injected as leading query terms). Unlike
  # cairnobs_alert_rule/cairnobs_notification_target, this resource
  # supports a real in-place update (api/dashboards.Handler has a real
  # PUT for panels) -- only dashboard_id forces a destroy-and-recreate,
  # since there's no API operation to move a panel between dashboards.
}
```

Supports `terraform import cairnobs_dashboard_panel.example
<dashboard-id>/<panel-id>` -- a bare panel ID isn't enough on its own,
since `Read` needs the parent `dashboard_id` to know where to look (see
`client.go`'s `getPanel` doc comment for why: there's no standalone
`GET` for a single panel).

```hcl
provider "cairnobs" {
  endpoint          = "http://localhost:8080" # or $CAIRNOBS_API_ENDPOINT
  alerting_endpoint = "http://localhost:8081" # or $CAIRNOBS_ALERTING_API_ENDPOINT -- alerting is a separate service, own base URL
  token             = var.cairnobs_api_token    # or $CAIRNOBS_API_TOKEN -- shared by both services, same as cairnobsctl's one $CAIRNOBSCTL_TOKEN
}

resource "cairnobs_notification_target" "ops" {
  name        = "Ops Webhook"
  kind        = "webhook"
  webhook_url = "https://ops.example.com/hooks/cairnobs-alerts"
}

resource "cairnobs_alert_rule" "example" {
  name                    = "Checkout 5xx spike"
  query                   = "service=checkout status>=500 | stats count"
  condition_type          = "threshold"
  comparator              = "gt"
  threshold_value         = 50
  eval_interval_seconds   = 60
  notification_target_id  = cairnobs_notification_target.ops.id
}
```

Supports `terraform import cairnobs_alert_rule.example <rule-id>` and
`terraform import cairnobs_notification_target.example <target-id>`.

**`cairnobs_alert_rule` and `cairnobs_notification_target` are both
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

**`cairnobs_notification_target`'s `secret` attribute is `Sensitive` but
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

## Data sources

Each resource above has a matching read-only data source (`data
"cairnobs_dashboard"`, `data "cairnobs_dashboard_panel"`, `data
"cairnobs_alert_rule"`, `data "cairnobs_notification_target"`) -- a lookup
against the same endpoint the matching resource's own `Read` already
uses, nothing new added to `client.go` beyond that. Mechanical and
low-risk by design: no new architectural question, no new external
service, no new write path -- just reusing the resource's own model/
conversion functions (`dashboardModelFromAPI` etc.) against `Required`
input instead of a full config. Three of the four take a single
`Required` `id`; `cairnobs_dashboard_panel`'s takes both `dashboard_id`
and `id` (both `Required`), matching `getPanel`'s own two-argument shape
-- there's no standalone lookup for a panel by ID alone.

```hcl
data "cairnobs_notification_target" "ops" {
  id = "target-abc123"
}

resource "cairnobs_alert_rule" "checkout_5xx" {
  # ...
  notification_target_id = data.cairnobs_notification_target.ops.id
}
```

`cairnobs_notification_target`'s data source has the same `secret`
caveat its resource does -- `Sensitive`, but a real value visible in
Terraform state; see "`cairnobs_notification_target`'s `secret`
attribute" above.

**Also not built, all real and disclosed, not attempted here:**
- Tenant/RBAC resources -- **not planned**, since multi-tenancy came off
  the roadmap (see the README's "Multi-tenancy is not the plan"). The
  original accounting stands for anyone who picks it up anyway:
  (`enterprise-auth`'s tenant/membership/grant
  surface) -- meaningfully different auth model (offline operator flags
  today, not a stable REST API a provider could safely drive
  idempotently -- see `/enterprise/README.md`'s "Bootstrapping a tenant"
  section), so this would need its own design pass, not just "add
  another resource file." (Not a licensing question as of Phase 6 --
  `enterprise/` is AGPLv3 same as this provider module; the blocker is
  purely that the underlying API isn't idempotent-safe yet.)
- Publishing to the real Terraform Registry -- `main.go`'s `Address`
  (`registry.terraform.io/cairnobs/cairnobs`) is the address a real
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
`httptest.Server` (same pattern `cli/cmd/cairnobsctl`'s own tests use
against the same `api/dashboards`/`alerting` endpoints) -- real request
construction (method, path, `Authorization` header, JSON body), real
response parsing, including the 404-vs-other-error distinction
`Read`/`Delete` need to implement Terraform's "resource deleted
out-of-band" convention correctly, (for rules) proving the
`GET /rules/{id}` response's promoted `RuleWithState` fields plus an
extra `"state"` key this client deliberately has no field for still
parse cleanly, (for targets) proving `secret` really does come back
unredacted -- documenting real `alerting` behavior with a test, not just
a comment, so a future change to that behavior would be caught here too
-- and (for panels) proving `getPanel` finds the right panel within a
real parent dashboard's `panels` array, and returns a recognizable
not-found both when the panel is missing from an otherwise-real
dashboard response and when the dashboard itself is gone.
`internal/provider/provider_test.go` validates the provider's, all four
resources', and all four data sources' schemas are internally
well-formed (attribute names, the `Required`/`Computed`/`Sensitive`
split -- for data sources, that every attribute except the lookup key(s)
is `Computed`) without needing a Terraform binary or a live
`api`/`alerting` service at all.

Each resource and data source pair has a matching `TestAcc*_basic` in
its own `_test.go` file (`dashboard_resource_test.go`/
`dashboard_data_source_test.go`, and likewise for the other three) --
eight real acceptance tests total, using
[`terraform-plugin-testing`][testing] -- skipped unless `TF_ACC=1` is
set, that framework's own standard convention, the same shape every
other live-infrastructure test in this repo uses (`docker`-gated env
vars for Postgres/ClickHouse tests elsewhere). The rule and target
*resource* tests both use a `plancheck.ExpectResourceAction` assertion
proving a config change actually plans a destroy-then-create, not an
in-place update -- the concrete, checked version of the "create/destroy
only" design decision documented above, not just a claim in a comment;
the panel resource test instead proves a genuine in-place update
(a `title` change with no `plancheck` needed, since the default
expectation -- update, not replace -- is exactly what should happen);
the *data source* tests each create a resource then look it up via
`resource.TestCheckResourceAttrPair`, proving the data source's `Read`
actually agrees with what the resource wrote, not just that both
compile. Even with `TF_ACC=1` all eight tests also need real running
`api`/`alerting` services (Postgres + ClickHouse) to apply against,
which this environment has no Docker access to bring up -- **not run
here**, same disclosed gap as every other live-infra test across this
repo (see `/docs/phase-4-runbook.md`'s "Verification status" section
for the project-wide version of this same caveat). "The test exists and
is correct Go" is not the same claim as "this resource has been applied
for real."

[testing]: https://developer.hashicorp.com/terraform/plugin/testing

```sh
# local dev override, so `terraform` picks up a locally-built binary
# instead of trying to download from the registry (which nothing has
# been published to -- see "What's built" above)
go build -o terraform-provider-cairnobs .
cat <<'EOF' >> ~/.terraformrc
provider_installation {
  dev_overrides {
    "registry.terraform.io/cairnobs/cairnobs" = "/absolute/path/to/this/directory"
  }
  direct {}
}
EOF
```
