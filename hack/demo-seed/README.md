# demo-seed

Everything the public demo deployment (`demo.cairnobs.org`) is built out
of, kept in the repo rather than only on the box so a demo can be rebuilt
from scratch and so its dashboards and rules are reviewable like any
other code.

- `reset-demo.sh` — the nightly reset. Wipes every volume, brings the
  stack back up, re-seeds users, notification targets, a week of
  synthetic history, dashboards, and alert rules. Run from cron at 04:00
  on the demo host.
- `dashboards/*.json` — one file per dashboard, in the shape
  `POST /dashboards/import` consumes (identical to what
  `GET /dashboards/{id}/export` and the web UI's Export JSON button
  produce, so a dashboard edited in the UI can be exported straight back
  into this directory).
- `alerts/*.json.template` — one file per alert rule, in the shape
  `POST /rules` consumes. `__TARGET_OPS__` / `__TARGET_SECURITY__` /
  `__TARGET_PLATFORM__` are substituted at apply time with the IDs of the
  three notification targets `reset-demo.sh` creates: every reset starts
  from an empty database, so the IDs can't be baked in.
- `cairnobs-demo-simulator.service` — systemd unit for the live half of
  the demo, `/hack/demo-simulator`. Installed at `/etc/systemd/system/`
  on the demo box.

## The fleet

Fifty hosts, shaped like an estate rather than a stack: thirty-one Linux,
eighteen Windows, and one Linux host whose agent is gone so the Agents
page has something stale to show.

| Tier | Hosts |
|---|---|
| Edge and proxy | `lb-01/02` (HAProxy), `edge-01/02` (nginx), `proxy-01` (Squid) |
| Application | `api-01`–`04`, `worker-01`–`03`, `arm-build-01` (aarch64) |
| Data | `db-01/02` (Postgres), `mysql-01`, `cache-01/02` (Redis), `mq-01/02` (RabbitMQ), `search-01/02` (Elasticsearch) |
| Platform | `k8s-node-01`–`03` (kubelet), `ci-01` (Jenkins), `vault-01`, `ldap-01` (OpenLDAP), `dns-01` (BIND), `backup-01`, `mail-01` |
| Windows | `DC-01/02`, `IIS-01`–`03`, `WIN-SQL-01/02`, `EXCH-01/02`, `FS-01/02`, `RDS-01/02`, `WIN-APP-01/02`, `PRINT-01`, `WSUS-01`, `SCCM-01` |

The Windows share is the point of the proportions. An enterprise looking
at this should recognise its own estate, which means Windows carrying
real services -- Active Directory, IIS, SQL Server, Exchange, file
shares, Remote Desktop, print, WSUS and SCCM -- rather than appearing
only as a Security channel on one box.

Two hosts carry stories the alert rules fire on and must not be moved:
`worker-02`'s disk fills at 0.04 of the volume per day, which is what
`worker-disk-filling` thresholds against, and `legacy-01` checks in once
and goes quiet, which is what `agent-legacy-01-unavailable` catches.
`api-02` is the host the outage window hits.

**Volume.** Fifty hosts generate about 316 records/minute at
`-rate-scale 1`, and the nightly reset runs at `RATE_SCALE=0.5` over a
168-hour backfill -- roughly **1.9M records per reset**, against about
0.5M when the fleet was twelve hosts. ClickHouse is untroubled by that;
what it costs is reset time and disk on the demo box. `RATE_SCALE` is the
lever if either becomes a problem, and lowering it keeps every host and
service present rather than dropping any of them.

## Prefilled login

The demo's login page comes up with the read-only `demo` account already
in both fields, so a visitor doesn't need credentials handed to them.
That's a build-time opt-in, off everywhere else: the web image is built
with `VITE_DEMO_USERNAME`/`VITE_DEMO_PASSWORD` (set in the demo host's
`docker-compose.override.yml`), and the login page prefills only when it
has both. Any deployment that doesn't set them gets the ordinary empty
form -- see `web/src/lib/api.ts`'s `demoUsername`.

The password is baked into the static bundle, which is fine for exactly
this case and nothing else: a Viewer-role account on a deployment whose
database is wiped and reseeded nightly. It has to match `DEMO_PASSWORD`
in `reset-demo.sh`, and changing that means rebuilding the web image.

## Why the demo needs a long-running process

Three things the demo has to show are only true if data keeps arriving,
and no amount of one-shot seeding fixes any of them:

- **Agents.** The Agents page is populated by the `AgentControl.CheckIn`
  RPC, and marks a host stale once it stops calling in. A fleet seeded
  once at 04:00 is entirely stale by 04:10.
- **Alerts.** Rules evaluate over trailing windows (`earliest=-5m`).
  Against a frozen dataset every rule settles into a permanent state
  within minutes and the Alerts page never moves again.
- **Recent views.** A "last 15 minutes" dashboard, or a query for what
  just happened, is empty on a dataset that stopped growing overnight.

So the demo runs `demo-simulator` continuously, and `reset-demo.sh` only
handles the parts that genuinely are one-shot: the history behind the
present, and the dashboards and rules themselves.

## Editing a dashboard

Change it in the web UI, hit Export JSON, and drop the file in
`dashboards/` — the export shape and the import shape are the same one.
The next reset picks it up. (Note that panel IDs and the dashboard ID are
not part of that shape: every reset creates them fresh.)

## What the queries can't do yet

Every panel here uses `table`, `bar`, `top_n`, `single_stat`, or
`heatmap`. None uses `line`, because a line chart needs a time axis and
the query language has no time-bucketing function — `stats count by
<field>` groups by literal column values, so there's no equivalent of
Splunk's `bin`/`timechart` to group by hour or minute. Raw SQL could
express it (`toStartOfHour(timestamp)`), but dashboard panels reject the
SQL escape hatch by design, since the time-range picker works by
prepending `earliest=`/`latest=` terms to a pipe-syntax query
(see `api/dashboards/types.go`'s `validatePanel`).

That gap is the one real thing standing between these dashboards and a
conventional observability overview screen.
