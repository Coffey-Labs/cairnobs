# sentryctl

Sentry's control CLI.

```sh
sentryctl ping                              # checks http://localhost:8080/healthz
sentryctl ping --api http://api.internal:8080
SENTRYCTL_API_URL=http://api.internal:8080 sentryctl ping
```

Exits 0 and prints `ok` if `/api`'s `/healthz` responds 200; exits 1 with an
error on `stderr` otherwise.

```sh
sentryctl query 'service=api | where status>=500 | stats count by host'
sentryctl query 'SELECT * FROM logs LIMIT 10' --language sql
sentryctl query 'message:"connection refused"' --json
```

Quote the query in your shell — pipe syntax uses `|`, which your shell
interprets as an actual pipe if you don't. Hits the exact same `POST
/query` endpoint the web UI does (`internal/querylang` in `/api` does the
compiling; there's no separate query logic here to drift out of sync —
see `/docs/query-language-reference.md`). `--language` overrides
auto-detection, same optional override the HTTP API itself exposes.
Prints a table by default (stdlib `text/tabwriter`, no new dependency);
`--json` prints the raw `{columns, rows}` response instead.

```sh
sentryctl dashboards list
sentryctl dashboards get <id>
sentryctl dashboards apply dashboard.json    # imports a dashboard exported via the web UI's "Export JSON" button

sentryctl dashboards permissions list <dashboard-id>
sentryctl dashboards permissions grant <dashboard-id> <user-id> viewer|editor
sentryctl dashboards permissions revoke <dashboard-id> <user-id>

sentryctl alerts list
sentryctl alerts get <id>
sentryctl alerts apply rule.json             # creates a rule from a JSON file shaped like POST /rules's body
```

`dashboards permissions` is Phase 4's per-resource dashboard grant
(`api/dashboards.PermissionStore`) — additive-only, raises someone to
`viewer` or `editor` on one specific dashboard (Admin/Owner already have
tenant-wide access, so the server rejects any other role). On plain
`api` (no `enterprise-api`, no permission service wired in) every
`permissions` call fails with a 501 whose message says so explicitly —
that's the deployment telling you this feature isn't available, not a
CLI bug.

`dashboards` talks to `/api` (`--api`, same override as `query`/`ping`).
`alerts` talks to `/alerting`, a separate service with its own base URL
(`--alerting-api`, or `$SENTRYCTL_ALERTING_API_URL`, default
`http://localhost:8081`) — see `/docs/phase-3-alerting-design.md`'s
component boundary for why alerting isn't just another `/api` route.
`apply` in both cases sends the file's JSON as-is to the corresponding
create/import endpoint — no reshaping, since the file's shape already
matches what the endpoint expects (the same JSON the web UI's export
button downloads, or the same shape `GET /rules/{id}` returns). This is
deliberately the seed of a future Terraform provider: one JSON contract,
multiple callers (web export, CLI apply, eventually a provider), not
three different formats to keep in sync.

No CLI framework (cobra/urfave-cli/etc.) — six commands split across a
few files (`cmd_ping.go`, `cmd_query.go`, `cmd_dashboards.go`,
`cmd_alerts.go`) is still boring enough not to need one; stdlib
`os.Args` handling plus a hand-rolled `switch` in `main.go` covers it.
Revisit once there's a real command tree (nested subcommands, nontrivial
flag parsing) to justify a dependency.

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

```sh
docker build -f Dockerfile -t sentryctl .   # context is cli/, not the repo root
```
