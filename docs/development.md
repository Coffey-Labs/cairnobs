# Working on Cairn OBS

You need Go 1.26+, Node 22+ and, to see containers, Docker with your user in
the `docker` group.

```sh
make            # build the web UI, then bin/obs-beacond and bin/obs-beacon-agent
make check      # gofmt, go vet, go test, and the UI's type check
make dev        # build, then run obs-beacond + helper + agent against this machine
```

`make dev` (dev/run.sh) keeps its state in `dev/state/` and listens on
127.0.0.1 only. Open http://localhost:8150/ and either:

- **Sign in with inbuxa (fake, local)**: the real sign-in flow against a
  stand-in inbuxa on 127.0.0.1:8151 (`dev/fakeinbuxa`). Pick the System
  Administrator to get in, or the tenant admin to see a refusal.
- **Sign in for local development**: a shortcut. obs-beacond only offers it when
  its config says `dev_signin: true`, and refuses to start that way on
  anything but a loopback address.

Sessions, incidents and the audit log are kept in `dev/state/beacon.db`, so
restarting obs-beacond keeps you signed in. Delete that file to start over.

Alerts in `make dev` go to a `log` channel (they print in the terminal)
after a 20-second hold. The dev expected-containers file names one container
that doesn't exist, `beacon-demo-sleeper`, so an incident opens on its own;
start it to watch the incident resolve:

```sh
docker run -d --rm --name beacon-demo-sleeper alpine:3 sleep 600
```

Working on the UI with hot reload: keep `make dev` running and, in `web/`,
run `npm run dev`; it serves on http://localhost:5180/ and passes `/api` and
`/auth` to the obs-beacond on 8150.

The web UI is built into `internal/webui/dist/` and embedded in obs-beacond, so
after changing `web/` run `make` again before testing the binary.

## Dependencies and notices

THIRD_PARTY_NOTICES.md lists every third-party component inside the
binaries with its license text, and the packages install it. `make check`
fails when it's out of date: after adding or bumping a Go module, or a
package the web bundle uses, run `go run ./scripts/notices` and commit the
result. It stops on a license it can't name, so a new one gets a person's
look before it ships. The npm packages in the bundle are listed in
`scripts/notices/main.go` (only `svelte` and `@sveltejs/kit` today; the rest
are build tools).
