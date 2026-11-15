# Agent protocol (version 1)

How `obs-beacon-agent` talks to `obs-beacond`. The Go types are in
`internal/report/report.go`; this page is the contract.

## Reporting

Every `interval` (15 s by default) the agent sends one report:

```
POST /api/v1/agent/report
Content-Type: application/json
User-Agent: obs-beacon-agent/<version>
```

over TLS with its client certificate (see Transport). On a plain HTTP
listener (`agent_tls.off`) it adds `Authorization: Bearer <agent token>`.

obs-beacond answers:

| Status | Meaning | The agent… |
|---|---|---|
| 204 | stored | sends the next queued report, if any |
| 400 | malformed, or a protocol version obs-beacond doesn't speak | drops this report and logs it |
| 401 | no live certificate (or, on plain HTTP, an unknown token) | keeps the queue, enrolls again if it has a token, retries next interval |
| 413 | over 4 MB | drops it (it can't succeed) |
| 5xx, timeout, no connection | obs-beacond is down or unreachable | keeps the queue and retries next interval |

**Checks.** When obs-beacond has checks for the agent, the 204 carries
`X-Beacon-Checks: <version>`. If that differs from the list the agent is
running, it fetches `GET /api/v1/agent/checks` (same token) after the
report: `{"version": "...", "checks": [...]}`. No header means no checks.
Results go in the next reports' `checks` list (latest result per check).
Older obs-beacond versions send no header, so a newer agent runs nothing.

**Actions** (docs/cert-rotation.md). When an admin has asked the agent
to do something, the 204 carries `X-Beacon-Actions: 1`. The agent then
fetches `GET /api/v1/agent/actions` (same credentials):
`{"actions": [{"id": "...", "kind": "reload", "args": {...}}]}`. Each is
handed over once, by whichever copy the agent asks, and only within 10
minutes of being asked for. The agent decides for itself whether its
config allows each one, and puts what came of every one in its next
report's `actions` list: `{"id", "ok", "detail"}`, with the reason when
it was refused or failed. A kind it doesn't know is refused. obs-beacond
records the result and audits it. Older obs-beacond versions send no
header, and older agents ignore it.

A host agent's one kind is `reload` (cert-rotation.md, step 2), with args
`{"unit": "nginx.service", "addr": "127.0.0.1", "port": 443}`: reload
a unit whose port serves an older certificate than its renewed file.
The agent refuses a unit its `cert_reload` doesn't list or a port the
host doesn't listen on, handshakes the port, and asks the root helper
(`POST /obs/reload` with the unit and the served certificate's names,
fingerprint and end). The helper reads `cert_reload` from agent.yaml
itself and reloads only when one of that unit's files holds a
certificate for the same names that ends later, has started, hasn't
ended and matches its key (in the file, or `privkey.pem` or the same
name with `.key` beside it), and systemd says the unit can reload; it
never restarts. The agent then handshakes again, for up to 15 seconds,
and says done only once the port serves the renewed certificate. Every
report carries the agent's rules as `reloads` (`[{"unit", "files"}]`),
so obs-beacond offers only those; it's empty without a helper or with
`cert_scan: false`.

**Who watches beacon.** An agent with `watch_server` set counts how long
its reports have been failing. Past `after` (3 min) it fetches obs-beacond's
public `/healthz`; if that fails too, obs-beacond is down and the agent sends
"Cairn OBS is down" through its own channels, once, and "resolved" after
its next report gets through. If the public page answers, obs-beacond is up
and only this host's path is broken; obs-beacond itself alerts that the host
went silent, so the agent stays quiet. Set it on hosts other than the one
running obs-beacond.

**Log lines.** For a container in trouble (restarting, unhealthy, dead,
or exited within the last day) the agent adds its last 20 log lines to
the container's entry as `logs`, read again when the status changes or
every two minutes, at most ten containers per report. The helper only
passes a log read as "the last N lines" (N up to 200): no follow streams,
no time ranges. obs-beacond shows them on the Containers and Alerts pages, to admins and
operators only;
notices (ntfy, Discord, email) never carry them, only the link.

**Identity comes from the token, not the body.** obs-beacond looks the token's
SHA-256 up in its `agents` list; the `host` field in the body is only what
the agent calls itself. A token can't report as another host.

**Queue.** While obs-beacond can't be reached the agent keeps up to 240 reports
(an hour at 15 s, and 64 MiB at most) and sends them oldest first when it's
back. A report that doesn't go out at the first try is written to
`state_dir/queue/`, one file each, so an agent restart (an upgrade, a
reboot) loses nothing either; one that goes out at once never touches the
disk. On start the agent drops queued reports older than the queue spans
(240 intervals).

Every report sent from the queue but the newest carries
`X-Beacon-Queued: 1`. obs-beacond adds such a report to the host's history
(the sparklines) and leaves the host's state, which the newest report sets,
alone: an hour-old report never shows as now.

**Transport.** obs-beacond takes reports on its own listener
(`agent_listen`, e.g. `:8461`), never on the web listener. It is TLS 1.3
with mutual certificates from obs-beacond's own **agent CA**, made on its
first start and kept in its database, sealed with the state key:

- The listener's certificate comes from that CA. The agent pins the CA by
  fingerprint (`ca_fingerprint`, printed by `obs-beacond ca`): some
  certificate in the server's chain must have that SHA-256, and the leaf
  must chain to it with the server usage. There is no host name check: the
  CA belongs to this installation alone and signs no other server
  certificate, so a name adds nothing, and leaving it out means NAT,
  changed addresses and unlisted names just work. Agent certificates carry
  only the client usage, so one can't pose as the server.
- Each agent has a client certificate whose common name is its name in
  obs-beacond's `agents` list. Reports and the checks list need it; a token
  alone gets 401. Identity comes from the certificate, never from the body.
- An agent has one live certificate. Enrolling again or renewing revokes
  the one before, and an agent removed from `agents` is refused at once,
  whatever certificate it holds.

The connection only goes from agent to server, so agents behind NAT need
nothing. Keep the agent port's DNS name off proxies such as Cloudflare's:
they end TLS themselves and the client certificate never arrives.

## Enrolling and renewing

```
POST /api/v1/agent/enroll          Authorization: Bearer <agent token>
POST /api/v1/agent/renew           (the current client certificate)
Content-Type: application/pkcs10
<a PEM certificate request for a new ECDSA P-256 key>
```

Both answer 200 with `{"cert": "<PEM>", "ca": "<PEM>", "not_after": "..."}`,
or 401 (unknown token, or no live certificate) or 400 (a bad request). The
agent makes a new key for every request, checks that the answer is for that
key, signed by the pinned CA, for client use, and only then keeps it in
`state_dir` (`agent.key` 0600, `agent.crt`, `ca.pem`). Both are written to
the audit log.

- **Enroll** when there's no certificate, when it has run out, or after a
  401 (the certificate was revoked), as long as a token file is there.
- **Renew** once two thirds of its 90 days have passed. A failed renewal
  keeps the current certificate and tries again next report.

obs-beacond ignores the request's subject: the name comes from the token or
the certificate. The listener's own certificate lasts 90 days and is made
again, in memory, 30 days before it ends; the CA lasts ten years.

## Joining

```
POST /api/v1/agent/join            Authorization: Bearer <join token's secret>
Content-Type: application/pkcs10
<a PEM certificate request for a new ECDSA P-256 key>
```

A join token (`cobsj1_` and base64url JSON: `u` the agent listener's URL,
`f` the agent CA's fingerprint, `s` a random secret) is made by an admin in
the web interface for a name. obs-beacond keeps only the secret's SHA-256;
the secret is good once, for an hour. `obs-beacon-agent join` pins `f`,
sends the request with `s`, and gets the enroll answer plus
`"agent": "<name>"`. obs-beacond checks the request before spending the
token, then adds the agent to its database and signs its certificate, in
that order; a name taken meanwhile spends the token and answers 409. A
joined agent has no token afterwards: it renews with its certificate, and
if that runs out or is revoked it joins again with a new token.

On a plain HTTP agent listener (`agent_tls: {off: true}`, only for a
listener inside a VPN while its agents move over) there is no enrolling:
every request carries the token, as before this change.

## The report

```json
{
  "v": 1,
  "agent": "0.1.0",
  "host": "node2",
  "time": "2026-10-15T18:42:10Z",
  "interval_s": 15,
  "kernel": "6.12.48+deb13-amd64",
  "os": "Debian GNU/Linux 13 (trixie)",
  "uptime_s": 11234.5,
  "cpus": 8,
  "cpu_pct": 41.2,
  "load": [3.1, 2.8, 2.6],
  "mem": { "total": 67108864000, "available": 17448304640, "swap_total": 0, "swap_free": 0 },
  "disks": [
    { "mount": "/", "device": "/dev/md2", "fs": "ext4", "size": 937000000000, "used": 384000000000, "avail": 505000000000 }
  ],
  "net": { "rx_bps": 14500000, "tx_bps": 21000000 },
  "containers": {
    "list": [
      { "id": "3f2a…", "name": "wiki-web", "image": "ghcr.io/…/wiki:v4.5.2", "project": "wiki",
        "state": "running", "status": "Up 3 hours (healthy)", "health": "healthy", "created": 1792000000 }
    ]
  }
}
```

- Sizes are bytes; `net` is bytes per second since the previous report, over
  the host's non-virtual interfaces (container bridges and veths are left out).
- `cpu_pct` is the busy share of all CPUs since the previous report.
- `disks` has one entry per device: bind mounts and btrfs subvolumes don't
  repeat it.
- `containers` is missing when the agent has no helper configured (or the
  host has no Docker), and has an `error` and an empty `list` when it
  couldn't ask Docker. A container's `ports` are its published ports
  (`{"public": 8080, "private": 80, "proto": "tcp"}`) and `ips` its
  addresses on Docker's networks; the maps use them to tie a host port or
  a bridge address to the container behind it.
- `services` (not shown) lists the systemd services running on the host by
  name (`ssh.service`), leaving out per-user instances. The agent refreshes
  it every five minutes; it's missing without systemd. obs-beacond only
  offers it for picking under Configure › Hosts: services are judged from `units`.
- `machine_uuid` (not shown) is the machine's SMBIOS UUID, lower case,
  read once by the root helper (`/sys/class/dmi/id/product_uuid` is
  root's only). A hypervisor shows the same number as the VM's firmware
  UUID, which is how obs-beacond knows a host is a cluster's VM
  (cluster-watcher.md, Linking a VM to its agent). It's missing without a
  helper, and when the firmware gives none or a placeholder that every
  machine of a model shares.
- `vms` (not shown) is the host's libvirt domains, read by the root helper
  from libvirt's read-only socket at every report: `{"list": [...],
  "usage": [...]}`, or `{"error": "..."}` when libvirt is there but didn't
  answer. `list` holds `vm` records as a cluster agent sends them (Workloads,
  below): the domain UUID is the `id` and the `firmware_uuid` fact, no
  namespace, `since` when the agent first saw it in that state. `usage` is
  what the running ones used since the previous report, as a cluster
  agent's usage. Missing where libvirt isn't running, and without a helper.
- `sources` are the names of what this agent reads beside its host (a
  Proxmox cluster, a vCenter, Prism Central, arrays; Workloads, below),
  `[]` for none. obs-beacond forgets a source of this agent's that isn't
  listed, and one under the agent's bare name (sent before sources were
  named). Agents before it send no `sources`; nothing is forgotten for
  them.
- `map` (not shown) is where the host sits and what it talks to, for the
  maps (maps.md). The root helper samples it at every report; the agent
  sends it once a minute, merged: the newest `interfaces`, `gateway` and
  `wireguard`, every listener seen and each connection group at its
  busiest. It's missing without a helper and with `map: false`. Owners are
  `container:<name>` (or a short ID the agent couldn't name),
  `unit:<name>`, `process:<kernel name>`, or empty. Connections in from
  public addresses have no `remote`: an Internet client's address stays on
  the host. At most 500 listeners and 500 connection groups, busiest
  first, with `cut` set when there were more.

```json
"map": {
  "interfaces": [ { "name": "eth1", "kind": "vlan", "addrs": ["10.0.0.2/24"], "up": true } ],
  "gateway": "10.0.0.1",
  "wireguard": [ { "name": "wg0", "public_key": "<base64>", "listen_port": 51820,
    "peers": [ { "public_key": "<base64>", "endpoint": "198.51.100.7:51820", "allowed_ips": ["10.20.0.3/32"],
                 "handshake": "2026-11-13T20:03:34Z", "rx": 18233, "tx": 20112 } ] } ],
  "listen": [ { "owner": "unit:postgresql.service", "proto": "tcp", "addr": "*", "port": 5432 } ],
  "conns": [
    { "owner": "container:webmail", "dir": "out", "remote": "10.0.0.4", "port": 5432, "count": 6 },
    { "owner": "unit:nginx.service", "dir": "in", "port": 443, "count": 41 }
  ]
}
```

- `certs` and `cert_files` (not shown) are the certificates the host's
  ports serve and its files hold, for the certificate inventory
  (cert-inventory.md, steps 2 and 3): each a list with the time it was
  made, sent when it changes and every half hour, and kept by
  obs-beacond between. Each entry has the certificate's public parts
  (`fingerprint`, `subject`, `names`, `issuer`, `not_before`,
  `not_after`, `key`, `sig`) and where it was: `addr`, `port`, `owner`
  and `how` (`tls` or the STARTTLS protocol) for a port, `path` and
  `mod` for a file. A port's may have `wrong`: what's wrong with how
  it's served ("sent without its intermediate certificate"). Never a
  key.

## Workloads (cluster agents)

A host's agent sends what it reads beside its host (a Proxmox cluster, a
vCenter, Prism Central: cluster-watcher.md, steps 7 to 9) the same way,
as well as its reports, with `"source": "<name>"` in every batch: each is
a source of its own, kept as `<agent>:<name>` ("mgmt1:vcsa-01"), and a
card of its own on Clusters. The name is a host name (a-z, 0-9, dots and
dashes; 400 otherwise). Without `source` (a cluster agent, or a host agent
before 0.1.0-rc.2) the source is the agent's name.

A cluster agent (formats.md, Cluster agents) sends no reports. It joins as
a host's agent does (`POST /api/v1/agent/join`, with a token made for a
cluster), proves who it is with its certificate from then on, and sends
what it watches as batches of records:

```
POST /api/v1/agent/workloads
Content-Type: application/json
{"v": 1, "agent": "<version>", "generation": "<id>", "full": "begin" | "page" | "end" | "",
 "records": [{"kind": "pod", "id": "<uid>", "name": "web-7d9f8-x2k4q", "namespace": "shop",
              "owner": "deployment/web", "node": "worker-3", "state": "crashloop",
              "word": "Restarting (CrashLoopBackOff)", "since": "<RFC 3339>", "alert": "",
              "labels": {...}, "facts": {...}}],
 "removed": ["<id>", ...]}
```

- **A full listing** is a `begin` batch, any number of `page` batches and
  an `end`, all with one new `generation`, about 500 records each. When
  `end` arrives, everything from that agent not in the listing is gone.
  The agent lists everything when it starts and every 30 minutes.
- **Changes** come in between (`full` empty, the latest `generation`):
  the latest state of each record that changed, and the ids of the ones
  that went. At least one batch every 30 seconds, empty if nothing
  changed: obs-beacond calls a cluster silent after two minutes. After
  two minutes of sends failing, the agent lists everything again once it
  can.
- `kind` is `cluster`, `node`, `pod`, `controller` (named
  `deployment/web`), `job`, `pvc`, `vm`, `service` or `ingress` (named
  `ingress/web` or `route/web`). Services and ingresses are for the
  service map (maps.md): they're stored and shown, never judged. `state` is `ok`, `starting`,
  `stopped`, `degraded`, `down`, `crashloop`, `pending`, `updating` or
  `unknown`; `word` says it for people. A pod's `owner` is its
  top controller, not the ReplicaSet. `alert` is the
  `obs.cairnobs.org/alert` annotation as it applies to the record (from
  the record, else its controller, else its namespace).
- Records are trimmed in the cluster: labels only `app.kubernetes.io/*`
  and an allowlist, facts small. No spec, environment or annotation
  beyond our own leaves it.
- **Usage** comes with a batch about once a minute, when the cluster
  serves metrics.k8s.io: `"usage_at": "<RFC 3339>", "usage": [{"id":
  "<uid>", "cpu": 1.25, "mem": 6442450944, "cpu_of": 3.5, "mem_of":
  16106127360, "pods": 2}]`. CPU in cores, memory in bytes; `cpu_of` and
  `mem_of` are the ceiling (a node's allocatable, the sum of a
  controller's or VM's pods' limits), left out where there's none;
  `pods` is how many pods were summed. Only nodes, controllers and VMs
  have usage. Each reading is sent once; obs-beacond keeps the newest.
- **Actions** (cert-rotation.md, step 3) work as a host agent's do, with
  batches in place of reports: a batch's 204 carries `X-Beacon-Actions:
  1` while one waits, the agent fetches `GET /api/v1/agent/actions`, and
  the next batch (at most 30 seconds later) carries `"actions": [{"id",
  "ok", "detail"}]`. The one kind is `renew`, args `{"namespace", "name"}`
  of a cert-manager Certificate: the agent sets its `Issuing` condition,
  as `cmctl renew` does, and only with `-cert-renew`; otherwise it
  refuses with the reason. With `-cert-renew` the `cluster` record's
  facts say `"cert_renew": true`, so obs-beacond offers it only there; a
  `cert` record's `"issuing": true` says cert-manager is renewing it
  already.

obs-beacond answers 204 (stored), 400 (malformed: a state or id it
doesn't take; the agent drops the batch and logs it), 401, 403 (a
cluster agent sending a report), 413 (over
16 MB) or 503 (the database didn't take it). On 5xx or no answer the
agent keeps the batch and sends it again, before anything newer.

A record of a kind obs-beacond doesn't know comes from a newer agent: it's
dropped and the rest of the batch is stored. Servers before 0.1.0-rc.2
refused the whole batch instead, so upgrade obs-beacond before the
cluster agents.

## Changing the protocol

Add fields freely; old servers ignore them. Never rename, retype or reuse a
field. Anything incompatible bumps `v`, and obs-beacond answers 400 to a version
it doesn't speak.
