# sentryctl

Sentry's control CLI. Phase 0: a single command.

```sh
sentryctl ping                              # checks http://localhost:8080/healthz
sentryctl ping --api http://api.internal:8080
SENTRYCTL_API_URL=http://api.internal:8080 sentryctl ping
```

Exits 0 and prints `ok` if `/api`'s `/healthz` responds 200; exits 1 with an
error on `stderr` otherwise.

No CLI framework (cobra/urfave-cli/etc.) — a single command doesn't need
one, and stdlib `os.Args` handling is boring enough not to need a
dependency. Revisit once there's a real command tree to justify one.

## Building & testing

```sh
go build ./...
go vet ./...
go test ./...
```

```sh
docker build -f Dockerfile -t sentryctl .   # context is cli/, not the repo root
```
