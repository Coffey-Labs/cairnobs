# Scale and availability: design note

Status: proposal, 2026-11-12; being built in the order under Plan. Steps
1 to 5 are done: several copies run on one PostgreSQL database, only one
of them sends alerts, and they upgrade one at a time
(`deploy/kubernetes/*-ha`). Step 6, testing all this under load, is
next. PostgreSQL as the cluster datastore is decided.

## What we're aiming for

- **Small:** one Docker container, one SQLite file, nothing else to run,
  as today.
- **Large:** several thousand VMs, pods and containers on a Kubernetes or
  OpenShift cluster. The web interface and the endpoints agents report to
  scale out by adding copies.
- **A datastore that grows live.** Add space, add storage and add read
  capacity without stopping anything.
- **Zero downtime, upgrades and growth included.** No lost reports, no
  failed requests during a rolling upgrade, and no missed or doubled alerts
  when a copy dies.

The single container can't upgrade without a short gap, since it's one
process. It can upgrade without losing anything: agents keep their reports
until the server is back. Zero downtime needs two or more copies.

## What's there today

obs-beacond is one process. The parts that stop it from running as more
than one copy:

| State | Where it is now |
|---|---|
| Latest report per host, last hour's sparklines | memory (`Store`) |
| Open incidents, acknowledgements | memory, saved to the database on change |
| Sessions | memory, saved to the database |
| Settings (checks, channels, windows, what should run) | database, cached in memory |
| Agent CA, joined agents, agent certificates | database (CA sealed with the state key) |
| Sign-ins out at a provider (OAuth state) | memory only |
| Password sign-in failure counts | memory only |
| Cloudflare, mail, data layer and digest pollers | run inside the process, results in memory |
| The database itself | SQLite, one file, `CREATE TABLE IF NOT EXISTS` plus small hand-written `ALTER TABLE` steps |

Agents already help: each one queues up to 240 reports on disk (an hour at
the default 15 s interval) and sends them oldest first when the server
answers again. Their history counts; only the newest updates "now".

## The shape

One program, `obs-beacond`, doing three jobs:

| Job | Copies | Holds |
|---|---|---|
| **Ingest**: agent reports, enroll and renew over mTLS | any number | nothing of its own |
| **Web**: the interface and the browser API | any number | a read cache, rebuilt from the database |
| **Evaluator**: alert decisions, notices, checks' agreement, pollers (Cloudflare, mail, data layer, digest), retention | one active, others on standby | nothing; it works from the database |

`roles: [ingest, web, evaluator]` in obs-beacond.yaml, all three by default.
On Kubernetes one Deployment runs all three in every pod; every pod can
take over as evaluator. Splitting them into separate Deployments is
possible later and isn't needed to start.

The single container is all three jobs on SQLite, which is what runs now.

## Storage

### One set of SQL, two back ends

`internal/state` stays one implementation:

- The SQL is written once, in what SQLite and PostgreSQL share.
- A small dialect layer (`dialect.go`) covers the differences:
  placeholders, the schema's DDL, how flags are passed, and what orders
  rows that tie.
- **SQLite** for one container; **PostgreSQL** for clusters, through `pgx`
  (MIT).
- Every query goes through the dialect, so no query can bypass it.

The same tests run against both: CI starts a PostgreSQL service container.
Two separate implementations would have doubled the code and let the two
drift apart.
`database:` takes either a path (SQLite) or a `postgres://` URL, with the
password in a file as with every other secret.

`obs-beacond store copy --from <sqlite> --to <postgres>` moves an install
across. It's run once while the old server is stopped, and is the only step
in the whole design that needs a stop. It's for moving off the small setup,
not for upgrades.

### Running PostgreSQL

- On OpenShift and Kubernetes I recommend **CloudNativePG** (Apache 2.0, in
  OperatorHub): three instances, one primary and two replicas, on Pure block
  volumes.
- It covers what zero downtime needs from the database:
  - automatic failover;
  - switchover for the database's own upgrades;
  - backups to object storage (FlashBlade S3);
  - online volume growth;
  - adding replicas.
- Crunchy PGO would also work.
- `deploy/kubernetes` gets an optional CloudNativePG overlay. Anyone with
  their own PostgreSQL points `database:` at it.
- **Version:** PostgreSQL 16 or newer.

### Growing it live

| Need | How |
|---|---|
| More space | grow the volume: Pure's CSI driver supports online expansion; CloudNativePG resizes its volumes without restarting |
| More storage, somewhere else | history tables are partitioned by time; new partitions can go in a new tablespace on a new volume (CloudNativePG declares tablespaces) |
| Less space | retention drops whole partitions: instant, no `DELETE` storms, no bloat |
| More read capacity | add replicas; the web job can send history and report queries to a read-only URL (`database_read:`) |
| Cold history | later: export old partitions to object storage before dropping them |

Partitioned (PostgreSQL only): `status_log`, `host_hours`, `mail_rounds`
and `audit`, by month (UTC), made two months ahead, each with a default
partition that catches a row whose month has none yet (it moves into the
month when that's made). `incidents` isn't partitioned: open and resolved
ones share a key, and the table stays small next to the others; that can
change with the alert grouping work if it needs to. SQLite keeps plain
tables and `DELETE`-based retention, which is fine at its size.

### Changing the schema without stopping

- Numbered migrations, recorded in a `schema_migrations` table, replace
  `CREATE TABLE IF NOT EXISTS` and the hand-written `ALTER TABLE` steps.
- Only one copy runs a migration at a time, under an advisory lock, and
  each checks again under the lock that no other copy got there first.
- **Expand, then contract.** A release may add tables, add nullable columns
  and build indexes `CONCURRENTLY`. It can't rename, drop or rewrite
  anything the previous release still reads. Removing something takes two
  releases: stop using it, then drop it.
- Then old and new copies run side by side during a rolling upgrade, on the
  same database.
- Migrations that would lock a big table (rewrites, `NOT NULL` on a full
  column, an index built without `CONCURRENTLY`) aren't allowed. A test
  (`TestMigrationsOnlyExpand`) reads every migration's DDL for them, and
  for drops and renames outside a contract migration.

## Sharing live state between copies

| State | Change |
|---|---|
| Latest report per host | `hosts_live` (done), written by whichever copy took the report, only if newer than what's there; `NOTIFY` names the host; each copy reads the row and updates its cache and sparklines. A queued report's numbers go in the notice itself |
| Open incidents | written only by the evaluator (it already saves them); web copies read them and hear of changes by `NOTIFY` |
| Sessions | already in the database; a copy that hasn't seen one reads it (done); an ended one is announced so every copy drops it at once (done) |
| Settings | already in the database; a change is announced and every copy reloads that section (done) |
| Joined hosts | a join or removal is announced and every copy rereads the list (done) |
| Sign-ins in progress (OAuth state, passkey ceremonies, TOTP secrets being set up) | in the database (`ceremonies`, done), so the callback can land on any copy; keys stored as SHA-256, values sealed |
| Password failure counts | in the database (`signin_failures`, done), so limits hold across copies |
| Agent CA and certificates | already in the database; the state key is one Kubernetes Secret mounted by every copy |
| Poller results (Cloudflare, mail, data layer) | written by the evaluator (`readings`, migration 8, done), announced as `reading`, read by the other copies |

`NOTIFY` payloads carry only names, and the sending copy's id, so it
skips its own. The data comes from the table, so a missed notice costs
only a delay: every copy catches up when its listening connection
(re)connects and every 30 s. The channel is named after the schema, so
two installs sharing a database in different schemas don't hear each
other.
SQLite mode keeps the live state in memory as now (sign-ins in progress
use the database on both, since it costs nothing). The same interface
underneath means the server code doesn't branch on the back end.

Load, for scale: 1,000 agents every 15 s is about 70 reports a second.
That's an upsert each, which PostgreSQL handles easily. Pods and VMs from
the cluster watcher arrive as Kubernetes watch events and are stored as
changes, not polled.

## One evaluator at a time

- **Lease:** a `leases` row in PostgreSQL holds the evaluator's name, an
  epoch, and a time it's good until. The evaluator renews it every 5 s, and
  it lapses after 15 s. A standby takes over by bumping the epoch.
- Kubernetes Leases would also work, but this way also works on plain
  Docker or VMs, and needs no Kubernetes API permissions.
- **Fencing:** every incident write carries the epoch, and the database
  refuses a lower one. An evaluator that was paused and wakes up after
  losing the lease can't write.
- **Notices aren't doubled:**
  - Incident ids live in the database, so the new evaluator continues the
    same incidents.
  - Each incident records which notices went out, so the new evaluator
    doesn't send a "firing" again.
  - PagerDuty's dedup key absorbs a repeat that slips through anyway.
- **Takeover time:** at most about 20 s. The `for` delay before anyone is
  told is 2 minutes, so a takeover delays nothing anyone notices.
- **SQLite mode:** a single process is always the evaluator.

As built (step 4):

- The lease is the `evaluator` row of `leases` (migration 5). Every copy
  tries for it every 5 s; whoever holds it renews it, and it lapses after
  15 s. A copy that can't renew within 15 s stops judging alerts, even if
  it can't reach the database to find out who has it.
- The epoch only goes up when the holder changes. Incident saves and the
  incident counter carry the fence (`WHERE EXISTS` the lease at that
  holder and epoch), so a save from a copy whose epoch has passed changes
  nothing and returns `ErrFenced`.
- A round's notices are held until its saves are in, and dropped if one
  was refused: the copy that lost the lease says nothing and steps down.
- A new evaluator reads the incidents as stored. `fired` and `notified`
  are the record of what went out, so a problem still there doesn't fire
  again, and repeats keep their timing.
- On the evaluator only: judging alerts and sending notices, the history
  recorder and its retention, the weekly digest, and polling Cloudflare,
  Patroni and the mail round trip. After each round that changed
  something it announces `incidents`, and the other copies reread them.
- An acknowledgement can be taken on any copy: it's written to the
  incident on its own, and the evaluator's saves never touch the ack
  columns, so neither overwrites the other. A copy that isn't the
  evaluator announces `ack`, and the evaluator tells the channels that
  follow incidents (PagerDuty).
- On the way out (SIGTERM) the evaluator releases the lease before it
  drains and announces `lease`, so another copy takes over at once
  (6 ms in a two-copy run) instead of after 15 s.

The outbox (2026-11-13): a notice goes into the `outbox` table in the
same transaction as the incident it's about, under the same fence, so a
refused save queues nothing and an allowed one can't lose its notice.
The evaluator sends from the outbox, one row per notice and channel,
deleting each once it's sent; a failure is tried again after 5 s and
30 s, then given up on, as before. A copy that takes over sends what's
left. So a notice is sent at least once, never not at all: a copy that
dies after sending but before deleting means it goes twice (PagerDuty
folds a repeat into the same incident; other channels show it twice).
Without a database, or with the database away, notices go from memory
as they did.

Readings (2026-11-13): Cloudflare, Patroni and the mail round trip are
polled by the evaluator alone. After each round it stores the reading in
`readings`, one row per poller, and announces `reading`; the other copies
read it back. So each API is asked once however many copies run, the
round trip sends one test message a round, and every copy's pages agree.
A copy that takes over, or a single obs-beacond that restarts, starts
from the stored reading and polls when the next round is due, not at
once: since when Cloudflare has been failing, its DNS records as last
seen and the round trip's day of history carry over, and an upgrade
sends no extra test message. A reading taken with other settings (a zone
or a Patroni member added) is polled again at once. A round cut short
because the copy lost the lease is dropped, not shown. PostgreSQL leader
changes go in the audit log from the evaluator only, so once.

Known gaps, for later steps:

- Lease times come from each copy's clock. NTP keeps them close enough,
  and fencing covers the rest.

## Upgrading and failing without downtime

### The server

- **Rolling upgrade:**
  - `maxUnavailable: 0`, a PodDisruptionBudget, and readiness probes that
    only pass once the caches are loaded.
  - On SIGTERM a copy stops being ready, finishes the requests it has, then
    exits. A grace period covers the longest request.
  - The evaluator gives up its lease on the way out, so a standby takes over
    at once instead of after 15 s.
- **Version skew:**
  - During a rollout, old and new copies serve at once.
  - **Browser:** a page from the new version might fetch a script that only
    the old copy has, or the reverse. Each copy therefore stores its
    `_app/immutable` files in the database (`ui_assets`, migration 6) when
    it starts, and serves from there what it doesn't carry. The newest
    three versions to have started are kept, which covers a rollout and a
    rollback. The names are hashes, so they never clash, and a script no
    version has is a 404, never the page. This replaced the first plan of
    building the previous release's files into the image: it needs nothing
    at build time and works across any versions that overlap.
  - **API:** stays compatible across one release, like the schema.
  - **Agents:** obs-beacond accepts report versions N and N-1, as it does
    now.
- **Database failover or switchover:**
  - Writes fail for a few seconds and the ingest job answers 503. A
    deleted primary first waits for clients to leave, so the `Cluster`
    caps that at 15 s (`smartShutdownTimeout`).
  - Agents queue and resend, so nothing is lost.
  - Web copies serve from cache and reconnect.
  - The evaluator's lease survives a few seconds without renewing. A longer
    outage hands over cleanly when the database is back.

### Agents

- They already queue to disk and resend. They gain:
  - jitter on retry (done): with a backlog to send, an agent first waits
    a random part of half its interval (at most 10 s), so a thousand
    agents don't all come back in the same second;
  - `server` may list more than one address; behind a Service or Route,
    one address is enough.
- **Agent upgrades** are package upgrades, host by host. A restart loses
  nothing, because the queue is on disk.

### Growing the fleet

- More reports means more ingest copies.
- More people looking means more web copies.
- Both can be scaled by a HorizontalPodAutoscaler on CPU, or by hand.
- Nothing has to be resharded, since no copy owns any hosts.

## How it will be tested

- **Synthetic fleet:** fake agents and a fake cluster watcher make thousands
  of hosts, VMs and pods, at the default intervals. The interface rework
  for thousands of workloads uses the same fleet.
- **Rolling upgrade under load,** on kind and then OKD:
  - zero failed agent requests and zero failed browser requests;
  - no report lost, counted at both ends.
- **Kill the evaluator** while an incident is about to fire: it fires
  exactly once, at most about 20 s late.
- **CloudNativePG switchover and failover** under load: no lost reports;
  any failed browser requests during the switch are counted.
- **Grow a volume and add a tablespace** while running.
- **Single container:** upgrade under load, with no lost reports and the
  gap measured.

### Results so far

`scripts/fleet` is the synthetic fleet: `fleet config -n N` writes N
agents for obs-beacond.yaml and their tokens; `fleet run` reports from
each at the usual interval, with a person's worth of interface requests,
through a load balancer of its own that sends only to copies whose
`/readyz` passes. It times both and counts every failure. Like real
agents, each fake one keeps a report it couldn't send and sends it again,
marked as queued; at the end it counts reports made, taken, still queued
and lost.

2026-11-12, one workstation (two copies of obs-beacond and PostgreSQL 17
in a container on the same machine), 1,000 agents every 15 s with 10
containers each:

| Run | Reports | Report p95 / p99 | Pages | Page p95 | Failures |
|---|---|---|---|---|---|
| Steady, 3 min | 12,000 (66.7/s) | 2.3 ms / 2.7 ms | 90 | 27 ms | 0 |
| Both copies restarted one at a time, 2.5 min | 10,000 | 2.3 ms / 2.7 ms | 75 | 26 ms | 0 |

- Each copy used about 45 MB of memory and 3% of a CPU.
- In the rolling restart (`shutdown_delay: 3s`), the load balancer took
  each copy out within 0.5 s of SIGTERM. The copy exited 3.1 s later and
  was ready again 0.1 s after starting. The evaluator role moved each
  time with no gap.
- What it showed next: at 1,000 hosts the overview's API answer was
  1.1 MB and the containers list's 3 MB, every few seconds in every
  browser. Now (2026-11-13) the overview carries totals for the fleet and
  only the 24 hosts most in need, and the hosts and containers lists are
  filtered, sorted and paged by obs-beacond:

  | At 1,000 hosts, 10,000 containers | Before | After |
  |---|---|---|
  | `/api/v1/overview`, every 5 s | 1.1 MB | 29 KB |
  | `/api/v1/containers`, every 10 s | 3 MB | 74 KB (200 a page) |
  | `/api/v1/hosts` (new), every 10 s | — | 42 KB (100 a page) |

  Each answers in about 12 ms. Still whole: `/api/v1/agents`, the
  Configure hosts list (88 KB at 1,000), which refreshes only while open.

2026-11-13, kind (Kubernetes 1.32, one node) with CloudNativePG 1.30.1:
the `plain-ha` overlay as shipped, three copies of obs-beacond, three
PostgreSQL 17 instances, the namespace enforcing the "restricted" pod
security standard. A prober pod asked the Service for `/readyz`, the
page and one of the page's hashed scripts every 50 ms, each on a new
connection, so they spread across the copies:

| Event | What happened | Failed page or script requests |
|---|---|---|
| Fresh install | the copies waited 1 min 45 s for the database, then started; no restarts | — |
| Rolling upgrade, then a rollback | 18 s each; both versions served side by side, and every page found its scripts | 0 of about 2,000 |
| Evaluator stopped (pod deleted) | role handed over in 0.3 s | 0 |
| Evaluator killed (SIGKILL) | role taken over 15.0 s later, as the lease ran out; connections to the dead copy refused for about 1.5 s until Kubernetes noticed | 7 |
| Database switchover | new primary in 4 s; the lease kept its holder; the notice listeners reconnected | 0 |
| Database primary's pod deleted | new primary 16 s later (see below); the database unreachable for about 2 s; lease kept, listeners back | 0 |

- `/readyz` answered 503 twice during each rollout: copies that were
  stopping, as they should, still reached through the Service in the
  moment before it dropped them.
- The first try at the fresh install restarted every copy three times,
  because obs-beacond gave up at once on a database that wasn't there
  yet, and Kubernetes waits longer before each restart. It now waits up
  to 2 minutes for a database that isn't up (a wrong password still fails
  at once), and the Deployment has a startup probe that allows 3 minutes.
- The first failover took 3 minutes: a deleted primary waits for its
  clients to leave (`smartShutdownTimeout`, 180 s by default), and
  obs-beacond's pooled and listening connections don't leave. The
  `Cluster` now sets 15 s, and the failover took 16 s.
- Not covered yet: agents through the cluster (the local run above
  covered reports across a rolling restart), the autoscaler (kind has no
  metrics server), and more than one node.

2026-11-13, the evaluator killed mid-incident: two copies on PostgreSQL,
three fleet agents every 5 s, `stale_after: 20s`, `alerts.for: 30s`, an
ntfy channel to a stand-in server. One agent stopped; its "gone silent"
incident opened at 08:06:37 and was due to fire at 08:07:07. The
evaluator was SIGKILLed at 08:06:52. The other copy took the lease when
it ran out, brought the open incident back from the database and fired
it at 08:07:23: once, 16 s late.

The same, killed mid-send, with the outbox: the stand-in server held the
evaluator's first request without answering, and the evaluator was
SIGKILLed 2 s into it with the notice still in the outbox. The other copy
took the lease 14 s later and sent it at once; the outbox was then empty.
Before the outbox that notice was lost.

2026-11-13, the single container (container.md): Docker, SQLite in a
volume, 200 agents every 15 s that keep what they can't send, as real
agents do. Upgraded under load with `docker stop`, `rm` and `run` of the
next build: 0.6 s from stop to the new version answering, 4 reports
refused and sent again, 2,000 made and 2,000 taken. It also found that
`snapshot` failed when its folder didn't exist yet, as on a fresh volume;
obs-beacond now makes it.

2026-11-13, 10,000 workloads (step 1 of cluster-watcher.md): two copies
on PostgreSQL, the same 1,000 host agents every 15 s, and 3 fake cluster
agents (`fleet config -clusters 3`), each with 30 nodes, 300 controllers,
3,000 pods and 300 VMs with a volume: 11,800 records in all. Each cluster
changed 2 pods a second and listed itself in full every 3 minutes; the
interface asked for a page every 500 ms, the Workloads and Clusters pages
among them; both copies were restarted one at a time 4 minutes in. 8
minutes:

| | Count | p50 | p95 | p99 | Failures |
|---|---|---|---|---|---|
| Host reports | 32,000 (66.7/s) | 1.7 ms | 2.1 ms | 2.6 ms | 0 |
| Workload batches | 360 | 3.1 ms | 48 ms | 116 ms | 0 |
| Pages | 960 | 12.5 ms | 17.8 ms | 19 ms | 0 |
| A change in one cluster showing on both copies | 190 | 56 ms | 61 ms | 0.8 s | — |

- 32,000 reports made, 32,000 taken, none lost. Each copy used about
  75 MB of memory.
- The slow batches are the first full listings (up to 157 ms for 1,000
  records); after that p95 is about 12 ms. The slowest change to show
  (2.5 s) came while a copy was restarting and catching up.
- The restarts: each copy out of the load balancer within 0.5 s of
  SIGTERM and ready again 3.3 s later.
- The twelve controllers the fake clusters give a crash-looping pod
  opened twelve warnings, one each, as they should: pods are judged with
  their controller, and nothing but a node or the cluster pages until
  it's annotated.
- A node with about 100 pods and 10 VMs on it went down for 4 minutes,
  spanning the rolling restart (a second run, 9 minutes, 36,000 reports,
  none lost): its incident opened 14 s after, fired 2 minutes later on
  the copy that had just become evaluator, and resolved 2 s after the
  node was back. One incident, nothing for what was on it.
- The first run found a fault: two warnings already sent about
  controllers with a crashing pod on that node were "resolved" when the
  node went down, and sent again when it came back. An alert already sent
  now stays on its own, as with hosts; in the second run both stayed
  open through the outage, with nothing sent about them.
- Pods removed by the churn are kept as tombstones, so a copy catching up
  sees them go, and purged after an hour: 24,000 by the end of the
  second run.

Targets to hold to:

- 10,000 workloads and 1,000 agents;
- interface pages under 300 ms at p95;
- failover under 20 s.

## Plan

One PR each, in order:

1. **Storage seam** (done):
   - every query through the dialect layer;
   - the SQLite-only SQL made portable;
   - numbered migrations, with today's schema as migration 1.
   - SQLite only; nothing else changes.
2. **PostgreSQL back end** (done):
   - PostgreSQL DDL for each migration, and the CI check;
   - partitions and retention by partition;
   - `store copy`;
   - CI runs every test on both back ends (PostgreSQL 16, the oldest
     supported).
3. **Shared live state** (done):
   - sign-ins in progress and sign-in limits in the database;
   - `hosts_live`, `NOTIFY` and the caches.
4. **Evaluator lease** (done): fencing, notices only after their saves,
   acks from any copy, the evaluator-only jobs.
5. **Rolling upgrades** (done):
   - graceful drain (`shutdown_delay`, `shutdown_grace`), `/readyz` on
     having caught up, earlier versions' assets kept in the database;
   - PodDisruptionBudget, HPA and the CloudNativePG component in
     `deploy/kubernetes` (`components/postgres`, the `-ha` overlays);
   - agent retry jitter.
6. **Scale and failure tests** (done, above), with their results written
   into this note.

The interface rework for thousands of workloads and the cluster watcher
come after step 3, since both need the shared, paged queries.

## Open questions

1. **PostgreSQL operator:** CloudNativePG (recommended) or Crunchy PGO, or
   leave the choice to whoever installs it and only document both?
2. **History retention:** everything is kept 400 days now. At fleet size,
   should hourly numbers for pods and VMs, which come and go, be kept
   shorter than for hosts and services?
3. **Is three-instance PostgreSQL the stated minimum** for "zero downtime"?
   With one instance, the database is the single point of failure: its own
   restarts become the gap.
