# Vault: design note

Status: decided 2026-11-13 (Decisions, at the end); being built in the
order under Plan. All four steps are done. Where the decisions changed the
proposal, the sections below say how.

John asked for HashiCorp Vault monitoring: health, seal, HA, Raft,
expiring leases and PKI certificates, with a read-only token. This note
is how Vault comes in: obs-beacond reads it (decision 1), and each Vault
cluster is a source of its own on Clusters, as an array is
(docs/pure.md). OpenBao, the
open-source fork, has the same API and is covered by the same reader.

## What we're aiming for

- **Each Vault cluster, its servers and what will run out**, on the same
  pages as everything else: sealed, no leader, a Raft server dropped, a
  CA a month from expiring, the monitoring token itself about to lapse.
- **Read-only.** A token with a policy that can only read the few paths
  below. Never a secret: no path under a secrets engine is read except
  PKI mounts' public certificates.
- **No SDK.** Vault's API is HTTP and JSON, as Purity's is: no new
  dependency.
- **We judge, and say how.** Vault raises no alerts of its own, unlike
  Purity or vCenter. The thresholds are ours, few, and the same ones the
  site checks already use for certificates (21 days a warning, 7
  critical).

## What's read

obs-beacond reads each Vault cluster every minute, through one address
(the load balancer or any server; standbys answer reads about the
cluster's state). Only the evaluator reads, as it polls Cloudflare and
Patroni: one copy asks Vault however many run, and the token is used
from one place at a time. What it reads goes into the same tables a
cluster agent's records do, under `obs-beacond:<name>`, so every copy
shows it.

| Read | Token? | Becomes |
|---|---|---|
| `sys/health`: initialized, sealed, standby, version, cluster name, replication modes | no | the cluster's own record: "vault-prod · read by mgmt1", "Vault 1.21.1" or "OpenBao 2.4.0"; sealed or not initialized is down |
| `sys/leader`: HA, the leader's address | no | no leader while HA is on: down |
| `sys/storage/raft/autopilot/state`: each server's health, voter or not, last contact; the cluster's failure tolerance | yes | a node per Raft server; unhealthy is degraded, gone is down; failure tolerance 0 makes the cluster degraded ("one more server lost and it stops") |
| `sys/replication/status` (Enterprise): DR and performance replication, each secondary's state | no | parts, as Pure's replication links are: `replication/dr`, connected or not |
| `sys/license/status` (Enterprise) | yes | the license's end, as a part of the cluster (step 4); 21 and 7 days |
| `auth/token/lookup-self`: the monitoring token's own TTL | yes (its own) | the token's end, on the cluster's record; 21 and 7 days, so it never lapses unnoticed |
| `<mount>/issuers` and each `issuer/<id>` on every PKI mount: the CA certificates | yes | a record per issuer CA: subject, its end; 21 and 7 days |
| `<mount>/certs` and each new `cert/<serial>`; `<mount>/certs/revoked` | yes | every leaf certificate the mount issued (decision 3), as below |
| `sys/metrics` (Prometheus format): lease count, request failures to audit devices, Raft commit time | yes | lease count on the cluster's record (and hourly, as arrays' numbers are); any audit write failing is critical: Vault stops answering when every audit device fails |

The policy the customer gives the token (the docs will carry it, with a
one-line `vault token create`):

```hcl
path "sys/storage/raft/autopilot/state" { capabilities = ["read"] }
path "sys/license/status"               { capabilities = ["read"] }
path "sys/metrics"                      { capabilities = ["read"] }
path "sys/mounts"                       { capabilities = ["read"] }
path "+/issuers"                        { capabilities = ["list"] }
path "+/issuer/+"                       { capabilities = ["read"] }
path "+/certs"                          { capabilities = ["list"] }
path "+/certs/revoked"                  { capabilities = ["list"] }
path "+/cert/+"                         { capabilities = ["read"] }
path "auth/token/renew-self"            { capabilities = ["update"] }
```

`auth/token/lookup-self` is in Vault's default policy already. `sys/mounts`
finds the PKI mounts. `renew-self` is the one write (decision 2): the
token renewing itself, when it's renewable, once half its TTL is gone.

## Every leaf certificate (decision 3)

A busy PKI mount issues thousands of certificates a day, most for hours,
and a renewed certificate leaves the old one in Vault until it ends. So:

- **Read once.** The serials are listed every minute; a certificate is
  fetched only the first time its serial is seen (its end never
  changes), at most 2,000 a minute, so a mount with 200,000 is read in
  under two hours and a new evaluator catches up the same way.
- **One record per subject.** Certificates for the same names (common
  name and alternative names) are one service's, renewed: only the
  newest counts. The record says how many it has had.
- **Revoked ones don't count**, nor ones whose subject has a newer one.
- **Judged when it matters.** A certificate meant to last under 7 days
  (`min_lifetime`) is counted, not judged: it's issued per job or per
  restart and nobody renews it. Others warn at 21 days left (or a third
  of their life, if that's less) and are critical at 7 (or a tenth). An
  ended one stays critical for 7 days, then goes, unless a newer one for
  its names came.
- **Counted** on the mount: issued, current, revoked, ending within 21
  days.

## What isn't read

- **Secrets.** Not one. The PKI issuer read returns the CA's public
  certificate and its settings, nothing private.
- **Every lease.** Listing leases one by one needs a `sudo` policy and
  walks every prefix: too much power and too much load on a big Vault.
  The count comes from `sys/metrics` instead (question 4).
- **Audit devices' settings** (`sys/audit` needs `sudo`). Their failures
  show in the metrics, which is what matters.

## How it's built

- **The reader:** `internal/vault`, a client and records, as
  `internal/flashblade` is. A `vault:` list in obs.yaml (`url`,
  `token_file`, `ca_file` or `fingerprint`, `name`, `every`,
  `min_lifetime`), each Vault cluster a source, `obs-beacond:<name>`. The
  token file is read again when it changes, so rotating it needs no
  restart. Every copy of obs-beacond needs the file; only the evaluator
  uses it.
- **The server:** nouns (a Vault cluster's nodes are "servers"), and a
  new record kind `cert`, a certificate with its subject and end, judged
  by days left. The certificate inventory will use the same kind.
- **Tested** against a real Vault and a real OpenBao, both run as
  services in the CI job in dev mode (free; nothing leaves the runner),
  plus a fake for what dev mode doesn't give: Raft (dev mode keeps
  everything in memory) and the Enterprise answers (replication,
  license). Raft is also checked once by hand against a real one-server
  Raft Vault.
- **The fleet simulator** gets two Vault clusters: one healthy with three
  servers, one with a server down, failure tolerance 0, and an
  intermediate CA 12 days from its end.

## The reader (step 1, 2026-11-13)

- **Set up** in obs-beacond.yaml's `vault:` list (formats.md, Vault),
  with the policy and a periodic token.
- **Read** every minute by the evaluator: `sys/health` (without the
  token; its standby, sealed and not-initialized answers are read, not
  errors), `sys/seal-status` when sealed (and nothing else: a sealed
  Vault answers nothing), `sys/leader`, Raft autopilot's state, and the
  token's own `lookup-self`. Storage that isn't Raft (dev mode's memory,
  Consul) has no servers to show; that isn't a problem.
- **Records:** the cluster ("vault-prod · read by obs-beacond", "Vault
  1.21.4", "Vault Enterprise" or "OpenBao" by its version), its Raft
  servers as nodes (leader, voter, non-voter; unhealthy is degraded,
  failed or left is down), and the monitoring token as a part of it.
- **The token** renews itself once half its time is gone or three weeks
  are left, at most once an hour; one that renews in full isn't judged
  by its days. One that can't, or comes back shorter each time (its max
  TTL near), warns at 21 days and is critical at 7.
- **Stored** as a cluster agent's records are, with the same batches and
  full listings, under `obs-beacond:<name>`; a Vault taken out of
  obs-beacond.yaml is forgotten when the evaluator starts. No agent may be
  called `obs-beacond`.
- **Tested** against Vault 1.21.4 and OpenBao 2.4.1 in dev mode (the CI
  job runs both, with the documented policy: renewing, a token that
  can't renew, an unknown token), a one-server Raft Vault by hand, and a
  fake for sealed, no leader, Raft unhealthy and no failure tolerance;
  the server test reads a fake over TLS on one copy and shows it on the
  other.

## PKI (step 2, 2026-11-13)

- **Mounts:** every mount `sys/mounts` lists as type `pki` (a mount
  nested deeper than one path segment isn't matched by the policy's `+`;
  give it its own lines). Its issuers (`<mount>/issuers`, each
  `issuer/<id>`), or `ca/pem` on a Vault before issuers (1.11).
- **Leaf certificates:** `<mount>/certs` and `certs/revoked` every read;
  each serial not seen before is fetched once (`cert/<serial>`), eight at
  a time, at most 2,000 a read and 30 seconds of it, and kept in memory
  for as long as obs-beacond runs (a new evaluator catches up the same
  way). One tidied away is dropped. CAs a mount signed (intermediates)
  show on their own mount, not as leaves.
- **Records** (the new kind `cert`, Namespace the mount): one per issuer
  (21 and 7 days) and one per set of names, the newest certificate for
  them, with how many there have been; revoked, short-lived (under
  `min_lifetime`) and long-ended (more than 7 days) ones aren't records.
  The mount itself is a part of the cluster: "212 names with a current
  certificate; 3 end within 21 days; 4,120 short-lived certificates, not
  judged; 5 revoked", "still reading 8,000 of 12,000" while it catches
  up, degraded when its certificates can't be listed or read.
- **Alerts:** a certificate under its warning line is a warning, under
  its critical line or ended is critical, keyed `cert/<source>/<id>`.
  Certificates page by default, as clusters and nodes do. Vault's own
  record and its parts (the token, a mount) warn when degraded, as the
  table in formats.md says, rather than paging as a cluster agent's do.
- **Tested** against Vault 1.21.4 and OpenBao 2.4.1 (a mount made per
  run: renewed, backdated, short-lived and revoked certificates), a fake
  for the fetch budget and tidying, the thresholds with fixed times, and
  the server test's critical certificate condition.

## Metrics (step 3, 2026-11-13)

- **Read** every minute: `sys/metrics?format=prometheus` (Prometheus'
  format is on by default; with `prometheus_retention_time` 0 it isn't,
  and the cluster says so). Counters there are totals since the server
  started, which is what lets one read a minute see every failure.
- **Leases:** `vault.expire.num_leases` on the cluster's record, and
  hourly (average and peak; migration 16 adds `leases` and `leases_max`
  to `workload_hours`), drawn on Clusters over a day, 30 days or a year.
  Only the active server keeps the count: point `url` at a load balancer
  that sends to the active one (`sys/health`'s 200), as Vault advises.
  Leases Vault couldn't revoke show as a fact.
- **Audit:** with an audit device on, a part "Its audit log". When the
  failures total goes up (`vault.audit.log_request_failure` and
  `log_response_failure`) it's critical for 15 minutes after the last
  rise: with every device failing Vault refuses requests. A lower total
  is a restarted server, not a failure; failures before obs-beacond
  started aren't counted. OpenBao 2.4 takes audit devices only from its
  config file, so its test checks the lease count alone.
- **Not read:** Raft commit time (step 4 may add it with replication).
- **Tested** against Vault 1.21.4 (with a `discard` audit device) and
  OpenBao 2.4.1, a fake for the failures (rising, held, a restart), the
  parser, and the hourly history through the server and both databases.

## Enterprise (step 4, 2026-11-13)

- **Asked only when there:** `sys/replication/status` when `sys/health`
  says DR or performance replication is on; `sys/license/status` when it
  says Enterprise. Open-source Vault and OpenBao are asked neither.
- **Replication:** a part per kind that's on, "DR replication" and
  "Performance replication". A primary is fine streaming to its
  secondaries, degraded when one isn't ready or its heartbeat is over 2
  minutes old, down when none is (or known ones aren't connected). A
  secondary is fine streaming WALs, degraded catching up (`merkle-diff`,
  `merkle-sync`), down cut off from its primary. A corrupt Merkle tree is
  down.
- **License:** a part, "Its license", rather than a fact on the
  cluster's record, so its end alerts as the token's does: 21 and 7 days;
  expired is critical, saying when Vault will seal (termination).
- **Tested** against a fake (no Enterprise image runs without a
  license): only asked when health says so, each replication state, the
  license's thresholds.

## Plan

One PR each, after the questions are answered:

1. **The reader** (done; The reader, above): health, leader, Raft,
   the token's end and renewal; the `vault:` section in obs.yaml; the
   evaluator polling it into the workload tables; the tests against real
   Vault and OpenBao.
2. **PKI** (done; PKI, above): issuers, every leaf certificate, and the
   `cert` kind.
3. **Metrics** (done; Metrics, above): lease count (and its hourly
   history), audit failures.
4. **Enterprise** (done; Enterprise, above): replication and license,
   against the fake.

## Questions

As put to John; my recommendation first in each.

1. **Who reads it?** (a) A host agent with a `vault:` section, as Pure
   is: the agent is already on the network, and one agent reads many
   clusters. (b) obs-beacond itself, as site checks are: one fewer piece,
   but the server then holds a Vault token.
2. **The token.** (a) A token file the agent reads again when it
   changes; we show its end and warn at 21 and 7 days; if the token is
   renewable the agent renews it, its only write, and only to its own
   token. (b) The same but never renewing: strictly read-only, and
   someone must rotate it by hand. (c) AppRole: the agent logs in itself
   with a role and secret ID.
3. **Certificates.** (a) PKI issuers (the CAs) here; leaf certificates
   in the certificate inventory, which will read Vault's PKI too, with
   the same `cert` kind. (b) Every leaf certificate here as well.
4. **Leases.** (a) The count, from `sys/metrics`, with its history: a
   count climbing for days is a leak. (b) Also list the leases nearest
   their end, which needs `sudo` on `sys/leases/lookup`.
5. **Enterprise.** (a) Read replication and license when Vault answers
   them, skip quietly on the free builds and OpenBao, test against a
   fake. (b) Leave Enterprise out until someone with a license asks.

## Decisions

2026-11-13, John: "1 b 2 a 3 b 4 a 5 a".

1. **obs-beacond reads Vault**, not an agent: the evaluator, into the
   workload tables, so every copy shows it.
2. **A token file, renewed by itself** when renewable; its end shown,
   warned at 21 and 7 days.
3. **Every leaf certificate too**, not only the CAs: read once each,
   one record per subject, short-lived ones counted, not judged.
4. **Leases as a count** from `sys/metrics`, with its history.
5. **Enterprise when present:** replication and license read when Vault
   answers, skipped on the free builds and OpenBao, tested against a
   fake.
