# The server in one container (Docker or Podman)

The `obs-beacond` image (kubernetes.md, Build the image) also runs on its
own, with Docker or Podman, on any Linux host. That's one copy on SQLite in
a volume: the simplest install after the packages. For several copies and
no gap during upgrades, run it on PostgreSQL instead (kubernetes.md, More
than one copy).

The image holds only the static binary, a CA bundle, the license and the
third-party notices. It runs as user 1001 in group 0, needs no
capabilities, and works with a read-only root.

## Set up

```sh
mkdir -p /etc/obs-beacon
head -c 32 /dev/urandom | base64 > /etc/obs-beacon/state.key   # keep a copy with your backups
chmod 0640 /etc/obs-beacon/state.key && chgrp 0 /etc/obs-beacon/state.key
mkdir -p /var/lib/obs-beacon && chown 1001:0 /var/lib/obs-beacon && chmod 0770 /var/lib/obs-beacon
```

(Rootless Podman maps user IDs: there, `podman unshare chown 1001:0
/var/lib/obs-beacon`, or mount it with `:U`.)

`/etc/obs-beacon/obs-beacond.yaml`, at least:

```yaml
listen: ":8150"
agent_listen: ":8461"
database: /var/lib/obs-beacon/beacon.db
state_key_file: /etc/obs-beacon/key/state.key
snapshot: /var/lib/obs-beacon/backup/beacon.db   # a consistent copy for the backups
alerts:
  public_url: https://obs.example.org   # how people reach it; turns on passkeys over https
```

Everything else in formats.md works here too.

## Run

```sh
docker run -d --name obs-beacond --restart unless-stopped \
  -p 8150:8150 -p 8461:8461 \
  -v /var/lib/obs-beacon:/var/lib/obs-beacon \
  -v /etc/obs-beacon/obs-beacond.yaml:/etc/obs-beacon/obs-beacond.yaml:ro \
  -v /etc/obs-beacon/state.key:/etc/obs-beacon/key/state.key:ro \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  registry.example.org/ops/obs-beacond:0.1.0
docker logs obs-beacond 2>&1 | grep -A2 'first admin'
```

With Podman, the same line with `podman`; rootless works, since nothing
needs a port below 1024. On RHEL-family hosts with SELinux, add `:Z` to
the data directory's mount (`/var/lib/obs-beacon:/var/lib/obs-beacon:Z`)
and `:z` to the two files'. For HTTPS, put the web port behind your proxy,
or publish 443 and 80 and let obs-beacond get its own certificate
(install.md). The agent port (8461) must reach the container untouched:
agents use TLS with client certificates from Cairn OBS's own CA.

Agents then join as on any install (install.md, Adding a host). The image
doesn't carry agent packages, so set `join.packages_url`.

## Upgrade

```sh
docker pull registry.example.org/ops/obs-beacond:0.2.0   # or load it
docker stop -t 30 obs-beacond && docker rm obs-beacond
docker run -d --name obs-beacond ...the same line, the new tag...
```

Upgrading one copy has a gap: the web interface is away for as long as
the swap takes (0.6 s in testing, from `docker stop` to the new version answering). Nothing is lost:

- Agents keep each report they can't send, on disk, and send the backlog
  once obs-beacond answers again, marked as queued so it adds to the
  history without replacing the newest state. After an outage each waits
  a random few seconds first, so the fleet doesn't arrive at once.
- Alerts held back before the stop are still held when it starts:
  incidents are in the database.
- A host that went quiet only during the gap isn't called silent: for
  `stale_after` after a start, nothing is.

The database schema only ever grows, so the previous release runs on a
database the new one has upgraded: going back is the same steps with the
old tag.

## Back up

- `snapshot` (above) writes a consistent copy of the database at start and
  every `snapshot_every` (6 hours); back that file up, never the live one.
- And, separately, the state key: without it the database's secrets can't
  be read, and obs-beacond won't start on it.

## Tested

2026-11-13, Docker on Debian 13, with the options above (read-only root,
no capabilities) and SQLite in a volume: 200 agents reporting every 15 s,
through an upgrade from one build to the next under load.

- From `docker stop` to the new version answering `/readyz`: 0.6 s.
- 4 reports were refused in that time and sent again: of 2,000 reports
  made, 2,000 were taken, none lost.
- The new version started on the old one's database, snapshot and agent
  CA unchanged.
