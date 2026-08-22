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
