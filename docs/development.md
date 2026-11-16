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

## The demo fleet

For a UI with an enterprise's worth of everything on it (screenshots, the
docs, working on pages at scale), `scripts/fleet` fakes the agents, the
clusters and the outside services they'd watch. Build it with
`go build -o bin/fleet ./scripts/fleet`, then:

```
fleet config -n 120 -out demo -demo
```

writes `demo/agents.yaml` and `demo/tokens`, and with `-demo` the checks,
expected units, backups and data layer for three sites of 50 hosts:
`demo/demo.yaml` has the lines to add to obs-beacond.yaml. Then run

```
fleet run -server http://127.0.0.1:8150 -tokens demo/tokens -demo \
  -cloudflare demo/cf -mail demo/mail -channels demo/channels
```

with the same number of agents. Each of `-cloudflare`, `-mail`, `-channels`,
`-akamai` and `-vault` serves a stand-in on loopback and writes the section
for obs-beacond.yaml into its directory (`cloudflare.yaml`, `mail.yaml`,
`channels.yaml`, …); add those once, and start fleet before obs-beacond so
the first readings find them. `-channels` catches what the alert channels
send (ntfy, Discord, PagerDuty, a webhook and email) and lists it on
http://127.0.0.1:58450/. `fleet run -h` lists the rest: clusters, VMs,
Proxmox, vSphere, Nutanix and Pure arrays.

Some things are wrong on purpose, so every page has something to show,
and most of the fleet is fine, as a real one is. `scripts/fleet/scenario.go`
has the plan: three stories with a cause and its effects (a FlashArray
lost a drive; a PostgreSQL replica falls behind and the shop's checkout
fails; the legacy datacenter drops out of Akamai GTM), a handful of
lasting warnings (a failover nobody moved back, a weak key, an old VM
snapshot, a zone on Flexible SSL, and so on), and problems that come and
go: a unit that fails and is restarted, a container that exits, a check
that flaps, a late backup, a host being patched. Each of those is a
*spell*: on for a while once every so often, counted from the Unix epoch,
so every part of the simulator agrees on what's happening now and a
restarted demo carries on where the clock is. Incidents open and resolve
through the day, and a dozen or two are open at any moment. The mail
server is healthy, to show what normal looks like.

What a host uses is a function of its name and the clock
(`scripts/fleet/load.go`), not a dice roll, so `-history <database>
-state-key <file>` can write the last 30 days (`-history-days`) before
anything reports (`history.go`): hosts' and workloads' hours, mail rounds,
statuses, and an incident for each past spell of the problems in
`pastProblems` (scenario.go), whose keys and titles are obs-beacond's
own. When you change what a spell's problem says in obs-beacond, change
its line there too.

## Dependencies and notices

THIRD_PARTY_NOTICES.md lists every third-party component inside the
binaries with its license text, and the packages install it. `make check`
fails when it's out of date: after adding or bumping a Go module, or a
package the web bundle uses, run `go run ./scripts/notices` and commit the
result. It stops on a license it can't name, so a new one gets a person's
look before it ships. The npm packages in the bundle are listed in
`scripts/notices/main.go` (only `svelte` and `@sveltejs/kit` today; the rest
are build tools).
