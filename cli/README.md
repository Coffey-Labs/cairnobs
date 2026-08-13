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

No CLI framework (cobra/urfave-cli/etc.) — two commands don't need one,
and stdlib `os.Args` handling is boring enough not to need a dependency.
Revisit once there's a real command tree to justify one.

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

```sh
docker build -f Dockerfile -t sentryctl .   # context is cli/, not the repo root
```
