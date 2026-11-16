# File formats

Cairn OBS is set up with YAML files, so the setup can be reviewed and
versioned like the rest of your configuration. All files are read strictly:
an unknown key is an error, not something silently ignored.

## obs-beacond.yaml and agent.yaml

See `config/obs-beacond.example.yaml` and `config/agent.example.yaml`. Quote
addresses (`"127.0.0.1:8150"`, `"[::1]:8150"`): YAML reads an unquoted
`[::1]` as a list.

### The agent listener

`agent_listen` (e.g. `":8461"`) is where agents report. It serves TLS with
obs-beacond's own agent CA (docs/protocol.md, Transport), which needs
`database` and `state_key_file`. On its first start obs-beacond makes the
CA and writes its certificate to `agent-ca.pem` in `data_dir` (State);
`obs-beacond ca` prints its fingerprint for the agents' `ca_fingerprint`.

`agent_tls` (optional):

| Key | |
|---|---|
| `names` | extra host names or addresses for the listener's certificate. Agents don't check names; this is for curl and other tools. The machine's host name and addresses are always in it. |
| `off` | `true` serves plain HTTP with tokens, as before. Only for a listener inside a VPN, while its agents move over. |

Without `agent_listen`, agents report to `listen` with their tokens (no TLS
of obs-beacond's own there: that listener usually sits behind a proxy).

With a proxy on the same machine, `listen` can be a unix socket,
`listen: unix:/run/obs-beacon/web.sock`: plain HTTP, no port to keep off
the network, and the socket's folder decides who can connect (obs-beacond
makes the socket itself writable by anyone). Agents can't reach a socket,
so set `agent_listen` too. `tls` doesn't go with it; the proxy does TLS.

### HTTPS for the web interface

`listen` serves plain HTTP unless `tls` says otherwise; passkeys and secure
cookies need HTTPS. Either put a proxy that ends TLS in front, or set one
of these (not both):

- `tls.cert` and `tls.key`: PEM files you renew yourself.
- `tls.acme`: obs-beacond gets and renews its own certificate.

`tls.acme`:

| Key | |
|---|---|
| `hostnames` | required: the names on the certificate (real DNS names, not addresses). The first is the public address: `alerts.public_url` defaults to `https://` it, with `listen`'s port unless that is 443. |
| `accept_terms` | required: `true` agrees to the CA's subscriber agreement (Let's Encrypt's is at https://letsencrypt.org/repository/). |
| `email` | optional: where the CA sends expiry and problem notices. |
| `directory` | the ACME directory; default Let's Encrypt. Point it at step-ca, a staging server or any other ACME CA. |
| `ca_file` | PEM roots for a private ACME server's own HTTPS. |
| `http_listen` | e.g. `":80"`: answers HTTP-01 challenges and redirects everything else to https. |
| `cache` | the account key and certificates; default `acme/` in `data_dir`. Keep it: a new one means new certificates, and CAs limit how often. |

The CA checks the name by connecting back: TLS-ALPN-01 on `listen`, so
port 443 of every name must reach `listen` directly (no proxy that ends
TLS, such as Cloudflare's orange cloud, in between), and HTTP-01 on
`http_listen` when port 80 reaches it. The packaged unit lets obs-beacond
bind ports below 1024. On start it gets the certificates within seconds
and logs `tls.acme: certificate for …` or why not; it renews them about
30 days before they expire.

```yaml
listen: ":443"
tls:
  acme:
    hostnames: [obs.example.org]
    email: ops@example.org
    accept_terms: true
    http_listen: ":80"
```

### Joining

Hosts added in the web interface (**Configure › Hosts**) join with a one-time command
(docs/install.md, Adding a host) and are kept in the database, not here.
They count everywhere an agent name does (checks, topology, backups,
maintenance windows). Clusters join the same way from **Configure ›
Clusters** (kubernetes.md, Watching clusters). `join` (optional) in
obs-beacond.yaml:

| Key | |
|---|---|
| `agent_url` | the agent listener as new hosts reach it, e.g. `https://monitor.example.org:8461`. Default: `alerts.public_url`'s host with `agent_listen`'s port |
| `packages_dir` | a directory of agent `.deb`/`.rpm` files and `SHA256SUMS` from a release, served at `/agent-packages/` for install.sh |
| `packages_url` | otherwise, a release download URL install.sh fetches them from |
| `image` | where clusters pull the obs-beacond image from, which carries the cluster agent: default `ghcr.io/coffey-labs/cairnobs`; set it, e.g. `registry.example.org/ops/obs-beacond`, for clusters that can't reach that. Without a tag, this obs-beacond's version (a development build takes `:latest` of the default) |

Joining needs `database` and the TLS agent listener. obs-beacond serves
`/install.sh` and `/cluster.yaml` on `listen`, unauthenticated; they hold
no secrets. `/cluster.yaml` adds a join token's Secret only when the
request carries a waiting cluster token (`Authorization: Bearer`).

In `agent.yaml`:

| Key | |
|---|---|
| `server` | `https://host:8461`, or `http://` for a plain listener |
| `ca_fingerprint` | required with https: `sha256:` and 64 hex characters (colons and case don't matter) |
| `token_file` | the agent's token: enrolls over https (and again after a revoked or expired certificate), signs every request over http. Joined agents have none |
| `state_dir` | the certificate and key; default `/var/lib/obs-beacon-agent` (the unit's StateDirectory) |
| `helper_socket` | the root helper's socket, `/run/obs-beacon/helper.sock`: containers (when Docker is there), the map sample, the machine's SMBIOS UUID, and libvirt's VMs (when libvirt is there). Without it, none of those |
| `map` | `false` stops the map sample on this host: no socket sample and no WireGuard read. Default on (docs/maps.md) |
| `cert_scan` | `false` stops the certificate scan: once a day the agent tries a TLS handshake on each TCP port the map sample saw listening (STARTTLS on 25, 587, 143, 110, 389 and 5432; never 22, 53, 111, 123, 2049, 5355) and sends the certificates it's served (cert-inventory.md). Default on wherever the map sample is |
| `hyperv` | Windows: `false` stops reading the host's Hyper-V VMs (windows.md, step 8: the agent's account needs Hyper-V Administrators to see them). Default true; nothing is read where Hyper-V isn't installed |
| `event_log` | Windows: `false` stops reading the errors and criticals in the System and Application logs (windows.md, step 7). Default true |
| `cert_files` | Globs of certificate files the root helper reads for the inventory (cert-inventory.md, step 3); only the certificates come back, never a key. `**` is any number of directories, `{pem,crt}` either. Absent: `/etc/letsencrypt/live/*/cert.pem`, `/etc/pki/tls/certs/*.crt`, `/etc/nginx/**/*.{pem,crt}`, `/etc/haproxy/**/*.pem`, `/etc/postfix/**/*.pem`, `/etc/dovecot/**/*.pem`; `[]` reads none. Needs the helper |
| `cert_stores` | Windows: the machine's certificate stores read for the inventory (`LocalMachine\<name>`, read-only; certificates only, never a key). Absent: `My`, `WebHosting`; `[]` reads none |
| `cert_reload` | Units obs-beacond may ask this agent to reload when one serves an older certificate than its renewed file (cert-rotation.md, step 2), each with the globs of the certificate files it serves: `[{unit: nginx.service, files: [/etc/letsencrypt/live/*/fullchain.pem]}]`. An admin presses a button each time; nothing reloads on its own. The root helper reads this list from agent.yaml itself, so the agent's word isn't taken; it reloads only for a renewed certificate with the same names that matches its key, and never restarts a unit that can't reload. Absent: nothing is ever reloaded. Needs the helper and the certificate scan |
| `proxmox` | on one node of a Proxmox VE cluster: read the whole cluster's VMs, containers and nodes (cluster-watcher.md, Proxmox). `token_file` holds an API token, `USER@REALM!TOKENID=SECRET`, whose user has the built-in PVEAuditor role on `/`. `url` defaults to `https://127.0.0.1:8006`. The API's certificate is checked against `ca_file` (default `/etc/pve/pve-root-ca.pem`), or pinned with `fingerprint` (its SHA-256, as `openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pve-ssl.pem` prints it) where the agent can't read the CA. `every` defaults to 30s. `name` is what the cluster is kept under, beside the agent's name (`pve1:proxmox`; default `proxmox`) |
| `vsphere` | a vCenter this host can reach: read its VMs, ESXi hosts, datastores and alarms (cluster-watcher.md, vSphere). `url` (`https://vcenter`), `username` (a user with the Read-only role on the vCenter, propagated) and `password_file` (the password alone, readable by the agent only). The certificate is checked against the system's CAs, or `ca_file` (the vCenter's VMCA root, PEM), or pinned with `fingerprint` (SHA-256, `AA:BB:…`). `every` defaults to 60s. `name` is what it's kept under (default its host name) |
| `nutanix` | a Prism Central this host can reach: read its clusters' AHV hosts, VMs, storage containers and open alerts (cluster-watcher.md, Nutanix). `url` (`https://prism-central`, port 9440 unless given), and either `api_key_file` (a service account's API key, Prism Central 2024.3 and later) or `username` and `password_file`; the account holds the built-in Prism Viewer role. The certificate is checked against the system's CAs, or `ca_file` (PEM), or pinned with `fingerprint` (SHA-256, hex, colons or not) for the self-signed one Prism Central is installed with. `every` defaults to 60s. `name` is what it's kept under (default its host name) |
| `pure` | a list of Pure FlashArrays and FlashBlades this host can reach on their management addresses (pure.md). Each: `type` (`flasharray`, the default, or `flashblade`), `url` (`https://fa-prod-01`), `token_file` (an API token, alone in the file, for a user with the built-in Read-Only role: in Purity's GUI, Settings → Users, make the user, then Create API Token on it). The certificate is checked against the system's CAs, or `ca_file` (PEM), or pinned with `fingerprint` (SHA-256, hex, colons or not) for the self-signed one the array ships with. `every` defaults to 60s; `name` (default the host name) is what it's kept under. Purity//FA 6.0 or Purity//FB 3.0 or later (REST 2) |
| `ibmi` | a list of IBM i partitions this host can reach, read through Mapepire (ibmi.md). Each: `address` (`prod1.example.com`, or with `:port`; Mapepire's is 8076), `user` (a profile with no special authorities: ibmi.md, Setting it up), `password_file` (its password, alone in the file). The certificate is checked against the system's CAs, or `ca_file` (PEM), or pinned with `fingerprint` (SHA-256, hex, colons or not) for the self-signed one Mapepire makes. `every` defaults to 60s; `name` (default the host name, lowercased) is what it's kept under. Mapepire must be running on the partition (ibmi.md, Setting it up) |
| `hmc` | a list of IBM Hardware Management Consoles this host can reach, read through the HMC's REST API (ibmi.md, The Power servers: the HMC). Each: `url` (`https://hmc1.example.com`; port 12443 is added), `username` (an HMC user with the `hmcviewer` task role), `password_file` (its password, alone in the file). The certificate is checked against the system's CAs, or `ca_file` (PEM), or pinned with `fingerprint` (SHA-256, hex, colons or not) for the HMC's self-signed one. `every` defaults to 60s; `name` (default the host name, lowercased) is what it's kept under. Read one HMC of a redundant pair: both would list the same servers |

An agent may set one of each of `proxmox`, `vsphere` and `nutanix`, and any number of arrays under `pure`, partitions under `ibmi` and HMCs under `hmc`. Each is a source of its own on Clusters, "read by" this host (protocol.md, Workloads). Two with the same `name` are refused, and then none is read.

### Workstations

An agent marked `workstation: true` in obs-beacond.yaml's `agents` list is a desk
machine, not a server. Cairn OBS watches it but it never pages:

- none of its conditions go above warning, its going silent included (it is
  switched off, or the office internet drops)
- it runs only the checks and data layer probes that name it in `from`, so it
  never votes on whether a service is down; a check with no `from` runs on
  the servers only
- its containers aren't judged, drawn on the maps or compared for image drift
- give it no section in expected.yaml. Its section in backups.yaml is
  usually empty (`desk: {}`): the scan then only asks smartctl about its
  disks, and a failing one is a warning like everything else on it. With no
  section, leave `backup_scan` out of its agent.yaml

```yaml
agents:
  - name: desk
    token_sha256: "<64 hex characters>"
    workstation: true
```

### Cluster agents

A cluster agent watches a Kubernetes, OpenShift or OKD cluster from
inside it and sends its nodes, pods, controllers, jobs, volumes and VMs
(cluster-watcher.md). It isn't a host: it sends no host report, isn't
counted or listed as one, and its cluster shows on the Clusters and
Workloads pages. A maintenance window may name it, which holds everything
about that cluster.

Clusters join from **Configure › Clusters** (kubernetes.md, Watching
clusters) and are kept in the database. An agent in obs-beacond.yaml
marked `cluster: true` sends workloads with a token instead, as
`scripts/fleet` does; the cluster agent itself joins.

```yaml
agents:
  - name: prod            # the cluster's name on the pages
    token_sha256: "<64 hex characters>"
    cluster: true
```

What's judged, and what pages:

| Condition | Severity |
|---|---|
| the cluster agent sends nothing for 2 minutes, or can't reach its API | Critical, one for the whole cluster; its workloads keep their last state |
| a node is down | Critical, one for the node, naming the pods and VMs on it, which aren't judged on their own while it's down |
| a node under pressure or not ready yet; one updating for over 30 minutes | Warning |
| a controller short of ready pods, or with pods crashing or failing to start for 5 minutes | Warning; Critical when it has none ready, or all its pods are crashing. Its pods are judged with it, never one by one |
| a pod with no controller doing the same | Warning |
| a failed job | Warning |
| a volume pending for 5 minutes | Warning; lost is Critical. Three or more in one storage class are one condition |
| a VM down or crashing | Critical; degraded, or starting for 5 minutes, is a warning |
| the cluster itself degraded | Critical |
| OpenShift and OKD: an operator degraded or not available | Critical; a warning while the cluster is updating |
| OpenShift and OKD: an update failing, or a machine config pool degraded | Critical |
| OpenShift and OKD: the agent silent while the cluster updates | Warning for up to 30 minutes (a single-node cluster reboots with its agent), then Critical |

On OpenShift and OKD the agent also sends the cluster's version, its
operators and its machine config pools, which the Clusters page shows
above the counts. A node the machine config operator is working on is
Updating, not down, even while it reboots. The platform's own namespaces
(`openshift-*`, `kube-*`) are judged through the operators, so their pods
raise nothing unless adopted or annotated.

As with hosts, an alert that had already been sent stays on its own when
its node goes down or the cluster agent goes silent: nothing reads as
"resolved" because a bigger problem came along.

Only a cluster's own trouble and its nodes page at their severity. Every
other workload stays a warning, on the pages and in channels that take
warnings, until it's adopted or annotated: `obs.cairnobs.org/alert:
page` on its namespace, controller or VM gives it its full severity,
`warn` keeps it a warning and `off` drops it.

Adopting is done in Cairn OBS on Configure › Clusters › a cluster, or in
expected.yaml under `clusters:` (below). It sets the same three words for
a whole namespace or for one controller or VM. When they disagree:

1. what's set in Cairn OBS for the workload itself,
2. then the annotation (the workload's own, its controller's, then its
   namespace's),
3. then what's set in Cairn OBS for its namespace.

An adopted controller or VM that isn't in the cluster's listing is
reported as missing (critical). An adopted VM that's stopped is critical,
unless it's adopted as stopped on purpose.

### Sign-in providers

Everyone who signs in has one of three roles:

| Role | Sees | Does |
|---|---|---|
| admin | everything | everything: setup under Configure, users, test notices, accept DNS changes |
| operator | everything, including container log lines, the audit log and Configure (read-only) | acknowledges alerts and writes their causes |
| viewer | what's watched, without log lines, Configure or the audit log | nothing |

`signin.providers` in obs-beacond.yaml lists the ways in. Each has an `id` (used
in `/auth/{id}/login` and `/auth/{id}/callback`), a `type` and a `label`.
`type: inbuxa`, `type: oidc` and `type: local` exist; `oauth` and `ldap`
are planned and rejected with "planned but not built yet" until then. See
the example config.

`type: oidc` is any OpenID Connect provider: Okta, Microsoft Entra ID,
Google Workspace, Keycloak, Authentik, Auth0 and the like. Register
Cairn OBS with it as a confidential web client whose only redirect address
is `redirect_url`, then:

| Key | |
|---|---|
| `issuer` | the provider's issuer, exactly as its `/.well-known/openid-configuration` names it (https; plain http only on loopback) |
| `client_id`, `client_secret_file` | the client; the secret is a file, never inline (leave it out for a public client) |
| `redirect_url` | `https://<Cairn OBS>/auth/<id>/callback` |
| `admin_groups`, `operator_groups`, `viewer_groups` | group names; a member of an admin group is an admin, else of an operator group an operator, else of a viewer group a viewer |
| `admin_emails`, `operator_emails`, `viewer_emails` | addresses (`ada@example.org`) or domains (`@example.org`); count only when the provider marks the address verified (`email_verified`) |
| `groups_claim` | where the groups are: default `groups`; `roles` for Entra ID app roles; a dotted path such as `realm_access.roles` reads nested claims |
| `scopes` | default `openid email profile`; `offline_access` is added when the provider offers it |
| `auth_params` | extra authorization parameters, e.g. `access_type: offline` and `prompt: consent` for Google, or `hd: example.org` |
| `recheck`, `grace` | as for inbuxa: 15m and 24h |

At least one of the six lists is required: there is no "anyone the
provider knows" setting. Anyone matching none of them is refused and
nothing about them is kept. The ID token's signature is checked against
the provider's published keys (RS, PS and ES algorithms and EdDSA; never
`none` or a shared secret), with its issuer, audience, expiry and nonce.
Groups or the address are read from the ID token, or from userinfo when
the token lacks them. Every `recheck` uses the refresh token and decides
the role again, so taking someone out of a group (or disabling them) at
the provider demotes or signs them out within that time. A provider that
gives no refresh token (Google without `access_type: offline`) logs so,
and its sessions end when the access token does, usually within an hour.
Entra ID: use app roles (`groups_claim: roles`) rather than groups, which
it leaves out of the token for people in many groups. Google Workspace
puts no groups in its tokens: use the `_emails` lists.

`type: local` is the local directory: accounts Cairn OBS keeps itself, signed
in with passkeys. It needs `database` and `origin`, Cairn OBS's own
address (`https://beacon.example.org`; plain http only on localhost),
which passkeys are bound to. With `passwords: true`, an account can
instead sign in with a password **and** a TOTP code from an authenticator
app (never the password alone): Argon2id hashes, the TOTP secret sealed
with the state key, each code good once, and five failures in 15 minutes
from one address or for one account block it for the rest of that time.
Admins manage accounts under Configure › Users: add
someone as admin, operator or viewer and send them the one-time setup
link (24 hours; the token is in the link's fragment, so it never reaches
a server log). Operators can look at Configure › Users but change
nothing there. Disabling or
removing an account ends its sessions on their next request.

**With no `signin` at all**, obs-beacond uses a built-in local directory
with `passwords` on, and `alerts.public_url` as its origin when that is
https (passkeys are offered only then). While no enabled admin can sign
in, every start logs a one-time link (24 hours) that makes the first
admin: whoever can read the server's log can open it. Once there's a
redirect provider (inbuxa or OIDC) configured, no such link is ever made.

`emergency_listen` (top level, loopback only, e.g. `"127.0.0.1:8152"`)
opens the emergency door: the whole interface again, on a listener that
can only be reached through an SSH tunnel to Cairn OBS's host
(`ssh -L 8152:127.0.0.1:8152 host`, then http://localhost:8152). An
emergency account (an admin, made under Configure › Users with "Emergency
account" ticked) gets a setup link at the door and its passkey is bound
to `localhost:8152`, so it works there and nowhere else; ordinary
accounts can't sign in at the door. Every emergency sign-in tells every
alert channel, whatever its minimum, and shows a red banner while the
session lasts. Nothing outside Cairn OBS's host is needed: it works when
the single sign-on provider is down.

### Alerts

`alerts` in obs-beacond.yaml: `for` (how long a problem lasts before anyone is
told, 2m), `repeat` (how often an unacknowledged critical alert repeats,
15m), `public_url` (for links in notices) and `channels`. Each channel has
an `id`, a `type` and a `min` severity (warning, serious or critical):

| type | needs | notes |
|---|---|---|
| `ntfy` | `topic_file`, optional `server` and `token_file` | priority urgent for critical, high for serious |
| `discord` | `webhook_file`, optional `mention_critical` | colour by severity; @here only if asked |
| `webhook` | `url_file`, optional `secret_file` | JSON body; `X-Beacon-Signature: sha256=<HMAC>` |
| `email` | `smtp` (host:port), `password_file`, `from`, `to` (list), optional `username` (default: from's address) | authenticated submission only: STARTTLS on 587, TLS from the first byte on 465; refuses a server without TLS. `stand_in: true` is for a stand-in relay on this machine (the fleet simulator's demo): loopback only, without TLS |
| `pagerduty` | `routing_key_file` (an Events API v2 integration key), optional `server` (EU accounts: `https://events.eu.pagerduty.com`) | follows each incident: Cairn OBS's incident id is the dedup key, so alerting triggers it, acknowledging it here acknowledges it there, and resolving resolves it; severities map to critical, error (serious) and warning. Repeats and digests aren't sent: PagerDuty's escalation policy does that. A test sends a change event, which pages nobody |
| `log` | nothing | prints to obs-beacond's log, for development |

Critical alerts must reach at least one channel that isn't email (if mail is what's down, the email never arrives); obs-beacond refuses to start otherwise. In obs-beacond.yaml secrets are always files, never inline, and they never
appear in obs-beacond's logs or errors: a failing channel is named by its id.

A failed send is tried again after 5 s and 30 s. With `database`, each
notice waits in the database until it's sent, saved together with the
alert it's about: a restart, or another copy taking over, sends what was
left, so none is lost. The price is that one can now and then arrive
twice, when obs-beacond stops between sending it and noting it sent.

Without `alerts.channels` in obs-beacond.yaml, channels are added under
Configure › Alerts instead (see "Settings edited in the UI"). Writing the key,
even as `channels: []`, makes the file the place for them.

### Certificates

`certificates.critical_within` (default `48h`) is how close to its end a
certificate's alert becomes critical, whichever source saw it: hosts'
ports and files, site checks, Cloudflare, Akamai CPS, Vault and the
clusters' cert-manager. Before that it's a warning, from when its source
starts warning (21 days for most; checks.yaml's `tls_warn_days` and
Cloudflare's `cert_warn_days` set theirs). A certificate that has ended is
critical. The Certificates page judges by the same window.

```yaml
certificates:
  critical_within: 72h
```

### State

`database` is obs-beacond's own database: sessions, incidents (open ones and
the history), the audit log, settings saved in the UI, accounts, agents and
history. With it, restarting obs-beacond signs nobody out, an alert that was
open is still open (and resolves normally when the problem clears), and
incident numbers carry on. Without it all of that is in memory.

It's one of two kinds:

- **A SQLite file**, e.g. `/var/lib/obs-beacon/beacon.db`: nothing else to
  run. This is the single-server setup.
- **A PostgreSQL server**, 16 or newer, by URL:
  `postgres://obs_beacon@db.example.internal:5432/obs_beacon?sslmode=verify-full`.
  This is for clusters (docs/scale-and-availability.md). Keep the password
  out of the URL: put it in `database_password_file`. The URL takes
  libpq's settings (`sslmode`, `sslrootcert`, `target_session_attrs`, …);
  `search_path` picks a schema other than the user's default.
  obs-beacond makes its tables itself, so the user needs to own the schema
  (or be able to create tables in it).

`data_dir` is where obs-beacond keeps its own files: the agent CA's
certificate (`agent-ca.pem`) and the ACME cache. By default it's the SQLite
file's folder, or `/var/lib/obs-beacon` with PostgreSQL.

The schema is versioned (`schema_migrations`) and obs-beacond brings it up
to date when it starts; copies starting together take turns. A release only
adds to the schema, so the previous release keeps working on a database the
new one has upgraded: that's what lets a rolling upgrade run old and new
side by side, and what makes going back a release safe.

On PostgreSQL, history (status changes, hourly host numbers, mail round
trips, the audit log) is split into monthly partitions, made a couple of
months ahead. Retention drops whole months, so it never runs a big
`DELETE`; a month goes once all of it is older than the retention, so up to
a month more is kept than on SQLite. Each thing's last known status is kept
whatever its age, as on SQLite.

**Moving from SQLite to PostgreSQL** is done once, with obs-beacond stopped:

1. Set `database` (and `database_password_file`) in obs-beacond.yaml to the
   new, empty PostgreSQL database; keep `state_key_file` as it is. Remove
   `snapshot`, which is for SQLite.
2. `obs-beacond store copy -from /var/lib/obs-beacon/beacon.db` copies
   every row in one transaction and checks the counts on both sides.
   Nothing is changed in the SQLite file beyond bringing its schema up to
   this release's, so it stays as the way back.
3. Start obs-beacond.

`state_key_file` is required with `database`: 32 random bytes, base64.
Session cookies are stored only as their SHA-256, and a sign-in
provider's tokens and the agent CA are sealed with this key (AES-256-GCM),
so the database alone signs nobody in and can't stand in for obs-beacond.
Back the key up with the database: without it obs-beacond refuses to start
rather than quietly make a new agent CA and strand every agent.

If the key really is lost: put a new one in place, remove the old CA with
`DELETE FROM meta WHERE k = 'agent_ca'` (with `sqlite3 <database>`, or
`psql`) while
obs-beacond is stopped, start it, and give every agent the new
`ca_fingerprint` (`obs-beacond ca`). Agents with a token enroll again by
themselves. Everyone is signed out.

`snapshot` (optional, SQLite) is where obs-beacond writes a consistent copy
of the database (`VACUUM INTO`) at start and every `snapshot_every` (6h),
for the backups to pick up. The live file is never copied directly. Back
PostgreSQL up with its own tools: CloudNativePG's backups to object
storage, `pg_dump`, or whatever already backs the server up.

The audit log is at `GET /api/v1/audit` and on the Audit log page.

### Health and stopping

- `GET /healthz` answers `ok <version>` while the process runs: the
  liveness probe.
- `GET /readyz` answers `ready <version>` once this copy has caught up
  with the others sharing its database (at once when it's alone), and 503
  while it's starting or stopping: the readiness probe, and what a load
  balancer should check. Both are on the web and the agent listener.
- On SIGTERM obs-beacond fails `/readyz`, gives up the evaluator role if it
  has it, keeps serving for `shutdown_delay` (default 0; 5 to 10 s behind
  a load balancer), then stops taking connections and gives requests in
  flight `shutdown_grace` (30s) to finish.

### Data layer

`datalayer` in obs-beacond.yaml feeds the Data layer page and its alerts.

- `patroni`: each member's API (`http://10.0.0.1:8008`), which obs-beacond
  asks over the mesh every 15 s. `sync_standby: true` expects one;
  `lag_warn_mb` (16) and `dcs_stale` (60s) set the warnings. No leader or
  no answer from any member is critical; a member that isn't running or
  streaming, a replica too far behind, a missing member, no sync standby,
  or Patroni not hearing from etcd for `dcs_stale` warns. With
  `preferred_leader` (a member's name), a leader elsewhere warns: a failover
  happened and the leader should be moved back. A new leader goes in the
  audit log.
- `probes`: status pages each agent reads on its own host (loopback or
  private addresses only), `id`, `kind`, `url`, optional `from`.
  `haproxy` (the `;csv` stats page) judges every backend: those in
  `exactly_one` need one server up (a database primary), those in
  `at_least_one` need one (its replicas), the rest need all. `nats`
  (`/varz`) wants `peers` other servers connected and warns when slow
  consumers grow. `garage` (`/health`) wants "fully operational". `etcd`
  (its metrics listener, `--listen-metrics-urls`, `/metrics`; the agent
  keeps only the few lines Cairn OBS reads) is critical without a leader,
  warns at `quota_warn_pct` (75) of its space quota, and warns when the
  WAL fsync p99 since the last reading is over `fsync_warn_ms` (25; spinning
  disks need more, e.g. 500). A page that doesn't answer is critical.

Alerts are keyed `dl/<service>/…`, so with a topology they group under the
service map's patroni, etcd, haproxy, nats and garage nodes.

### Maintenance windows

`maintenance` in obs-beacond.yaml lists daily windows: `name`, `hosts`,
`daily` (HH:MM), `timezone` (default UTC; use the host's own, so daylight
saving moves the window with the host's timer) and `duration`. While a
window is open, the hosts' problems don't open or fire incidents, and
incidents already open for them stay as they are (no repeats, no
"resolved" while the host reboots). After it closes everything is judged
normally, so a host still down then alerts. Only conditions that belong to
a host are held: site, mail and DNS checks aren't. The same window format
goes in an agent's `watch_server.quiet` for obs-beacond's own host.

Leave `maintenance` out of obs-beacond.yaml and admins add and edit the
windows under Configure › Alerts instead (see "Settings edited in the UI").
Writing the key, even as `maintenance: []`, makes the file the place for
them.

### Cloudflare

`cloudflare` in obs-beacond.yaml (optional): `token_file` (a read-only API
token, re-read on every poll), `every` (5m), `zones` (default: all the
token can read), `ssl_mode` (strict), `ssl_mode_zones` (per-zone exceptions, `zone: mode`), `cert_warn_days` (14) and
`cert_critical_days` (3), `error_warn_pct` (5), `error_critical_pct` (25),
`error_min` (20), `block_min` (100) and `block_factor` (10), and
`stand_in`: a stand-in for Cloudflare on this machine, such as the fleet
simulator's demo (`http://127.0.0.1:port` only; the token and every request
go there instead). Each poll
reads every zone's SSL mode, edge certificate packs, load balancers and
DNS records, the account's pools with each origin's health from every
region, each zone's last 15 minutes of requests (GraphQL analytics, ten
zones per query) and Cloudflare's status page.

Cloudflare, Patroni and the mail round trip are polled by one copy of
obs-beacond, the evaluator; with several copies the others show its
latest reading. After a restart or an upgrade the last reading is shown
and the next poll comes when it's due, so a restart sends no extra test
message; changed settings are polled at once.

| what | alert |
|---|---|
| a pool has no healthy origin | Critical |
| an enabled origin fails the pool's health check in any region | Warning (host = the origin's name) |
| a zone's SSL mode isn't `ssl_mode` (or its `ssl_mode_zones` entry) | Warning |
| an edge certificate isn't active | Warning |
| an edge certificate expires within `cert_warn_days` / `cert_critical_days` | Warning / Critical |
| 5xx answers are `error_warn_pct` / `error_critical_pct` of a zone's last 15 minutes, and at least `error_min` | Warning / Critical, naming the busiest host |
| Cloudflare's own security blocked or challenged at least `block_min` requests in 15 minutes, and `block_factor` times the day before's average | Warning |
| a zone's DNS records differ from the last accepted set | Warning until the change is undone or accepted on the Cloudflare page |
| the API can't be read for 3 polls in a row | Warning; the last good reading stays on screen |

Cloudflare's own status page is shown but never alerts.

The firewall events dataset needs a paid plan, so "blocked" counts
requests whose security action refused them at the edge: block,
challenge, jschallenge, managed_challenge, connection_close and
force_connection_close. Actions that let the request through are not
blocks: AI Labyrinth's link_maze_injected, a challenge already solved or
bypassed, rewrite, allow, skip and log. Neither is a site refusing a
request itself (git.example.org's 403s to crawlers). The first time obs-beacond sees a zone its records become the
baseline; with a database the baseline survives restarts, so a change
made while obs-beacond was down is still caught. Accepting is in the audit
log. DNS drift ignores record ids, comments and timestamps.

### Akamai

`akamai` in obs-beacond.yaml (optional; akamai.md): `edgerc_file` (an
Akamai API client's `.edgerc`, re-read on every poll), `section`
(`default`), `every` (5m), `domains` (GTM domains; default every one
the client can read), `error_warn_pct` (5), `error_critical_pct` (25),
`error_min` (20) and `contracts` (whose CPS enrollments are read;
default every contract Property Manager lists, else those the client
sees without one). Make the API client in Akamai Control Center
(Identity and Access, API clients) with read-only grants. It's polled by
the evaluator, as Cloudflare is, and only ever sent GETs, but for the
traffic report's query, which the Reporting API v2 takes only as a POST
(it changes nothing; akamai.md, step 5). Each poll reads
every GTM domain's datacenters and properties, its propagation status,
and each property's most recent IP availability report; and every CPS
enrollment, its certificates on the production network (RSA and ECDSA
when both) and what a pending change waits for; and every Property
Manager property's versions and newest activation on each network; and
every Edge DNS zone's record sets (again only when the zone's version
changed); and the latest 5-minute traffic report (edge hits and 5xx by
CP code) and Akamai's status page (shown, never alerting). CPS,
Property Manager and Edge DNS are read every 15 minutes.
A product the
client can't read (403 or 404) is skipped and said on the page. For a
stand-in on the same machine (`dev/fakeakamai`), `host` may be
`http://127.0.0.1:port`; any other host must be https.

| what | alert |
|---|---|
| a GTM property has no live datacenter (among those enabled with servers) | Critical |
| a server is failing GTM's liveness test | Warning (host = the fleet host with that address) |
| a GTM domain's change has been propagating for over 30 minutes, or was refused | Warning |
| a CPS certificate on production ends within 21 / 7 days (a third / a tenth of a short life) | Warning / Critical |
| a CPS change is waiting on you (a third-party CSR or certificate, a staging check, warnings to acknowledge) | Warning |
| a property's newest activation on staging or production failed, or has been pending over an hour | Warning |
| an Edge DNS zone's records changed since last accepted (Accept on the Akamai page, as Cloudflare's) | Warning |
| a CP code's share of edge hits answered 5xx is over `error_warn_pct` / `error_critical_pct`, with at least `error_min` (the latest 5-minute report) | Warning / Critical |
| the API can't be read for 3 polls in a row | Warning; the last good reading stays on screen |

CPS certificates also show on the Certificates page (kind `akamai`);
their alerts stay here.

### Mail

`mail` in obs-beacond.yaml (optional) watches mail the way a person uses it,
with a monitor account of its own: `account`, `password_file` (re-read
every round), `server` (the mail server's name), `smtp` (server:587,
STARTTLS), `imap` (server:993, TLS), `url` (https://server), `client_id`
and `redirect_url` (a registered OAuth client: JMAP signs in the way
webmail does, password for a code, code plus PKCE for a token; the
redirect URL is never visited), `host` (the agent host serving the mail
server, so its maintenance window holds these alerts), `every` (5m),
`wait` (4m, less than `every`), `round_trip_warn` (1m), `queue_warn` (50)
and `queue_critical` (500). `stand_in: true` is for a stand-in mail server
on this machine, such as the fleet simulator's demo: SMTP and IMAP without
TLS, and `smtp`, `imap` and `url` must all be on loopback.

Each round signs in over SMTP submission, IMAP and JMAP, reads the size of
the outgoing queue (`x:QueuedMessage/query`, server-wide), then sends the
account a message and looks for it over JMAP by received date, not
full-text search, which can lag behind delivery. A message that arrives is
deleted; anything in the mailbox over an hour old is deleted too. The
account needs only `sysQueuedMessageQuery` and `sysQueuedMessageGet` on
top of the default user role. Its password stays in `password_file` on the
obs-beacond host. In inbuxa, set the account's password from a hash (`openssl
passwd -6 -stdin < password_file`) so the password never leaves the host.

| what | alert |
|---|---|
| a sign-in (SMTP, IMAP or JMAP) refused two rounds in a row | Critical |
| a test message not arrived within `wait` | Warning; Critical two rounds in a row |
| a test message slower than `round_trip_warn` | Warning |
| `queue_warn` / `queue_critical` messages in the queue | Warning / Critical |
| the queue can't be read two rounds in a row | Warning |
| no round for three times `every` | Warning |

Without a JMAP sign-in there's no telling whether mail arrived or what's
queued, so then only the sign-in alerts. These group under the service
map's `mail` node.

### Vault

`vault` in obs-beacond.yaml (optional) is a list of HashiCorp Vault or
OpenBao clusters obs-beacond reads itself (vault.md): `url` (one address
for the cluster: its load balancer, or any server; `https://`), `token_file`
(the monitoring token, alone in the file, read again when it changes, so
rotating it needs no restart; every copy of obs-beacond needs the file),
`ca_file` (PEM) or `fingerprint` (SHA-256, hex, colons or not) when
Vault's certificate isn't signed by a CA obs-beacond trusts, `name`
(default the URL's host name; it's kept as `obs-beacond:<name>`), `every`
(60s, at least 10s) and `min_lifetime` (7d: a leaf certificate meant to
last less is counted, not judged). The evaluator reads them, as it polls Cloudflare; each is a source
of its own on Clusters and Workloads, its Raft servers as its nodes.

Make the token from this policy (vault.md lists what each path is for):

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

```sh
vault policy write obs-monitor obs-monitor.hcl
vault token create -policy=obs-monitor -period=24h -display-name=obs-beacond -field=token > vault-token
```

A periodic token renews itself, so it never ends while obs-beacond runs.

| what | alert |
|---|---|
| sealed, or not initialized | Critical |
| HA on and no leader | Critical |
| a Raft server failed or left | Critical |
| a Raft server unhealthy (autopilot) | Warning |
| Raft unhealthy, or three voters or more and no failure tolerance left | Warning |
| a path the token can't read (Raft's state) | Warning |
| the monitoring token refused | Critical |
| the token ends within 21 / 7 days and isn't renewing itself in full | Warning / Critical |
| a PKI issuer (CA) ends within 21 / 7 days, or has ended | Warning / Critical |
| a leaf certificate, the newest for its names, ends within 21 / 7 days (or a third / a tenth of its life, if less), or ended in the last 7 days | Warning / Critical |
| a PKI mount's certificates can't be listed or read | Warning |
| an audit device failed to log (the failure count went up) | Critical, for 15 minutes after the last failure |
| `sys/metrics` can't be read | Warning |
| Enterprise: a replication secondary not ready or silent over 2 minutes (a primary's), or catching up (a secondary's) | Warning |
| Enterprise: no secondary streaming, a secondary cut off from its primary, or a corrupt Merkle tree | Critical |
| Enterprise: the license expires within 21 / 7 days, or has expired | Warning / Critical |
| the API doesn't answer | Critical; what was seen stays |

## backups.yaml: what each host's backups should be

`backups` in obs-beacond.yaml points at it; the same file goes to every host,
where `obs-beacon-agent scan -config backups.yaml -host <name> -out <file>`
(as root, from a timer, every 15 minutes, no network) reads its own
section. The agent's `backup_scan` names that file, so its reports carry
the latest scan; obs-beacond judges every host's scan against the file and
re-reads it when it changes.

```yaml
max_age: 26h      # newest copy older than this (default 26h)
shrink: 0.5       # newest under half the one before warns (default 0.5)
disk_warn: 0.9    # a backup disk fuller than this warns (default 0.9)
hosts:
  node1:
    received:     # a subdirectory per backup under root, dated copies inside
      - {from: node2, root: /var/backups/node2, expect: [wiki-db, node2-system]}
    made:         # backups whichever host is in charge makes: newest over the globs
      - {name: inbuxa-pg, paths: ["/var/backups/node*/inbuxa-pg/*.dump"]}
    jobs: [gitea-backup.service, node1-backup.service]
```

The scan lists directories and reads file sizes and times; it never opens
a backup. It also reads each listed job's last result, /proc/mdstat, and
`smartctl -H` for every disk.

| what | alert |
|---|---|
| a backup missing, empty, or its newest copy older than `max_age` | Serious |
| the newest copy under `shrink` of the one before | Warning |
| a backup disk over `disk_warn` full | Warning |
| a job's last run didn't succeed | Serious; a job systemd doesn't know warns |
| an md array missing a member | Critical |
| a disk failing SMART's health check | Critical; one smartctl can't read warns |
| no scan, or one over an hour old | Warning |

A silent host's backups aren't judged (its silence alerts); its last scan
stays on the Backups page.

## Reports and the weekly digest

obs-beacond keeps the history reports need in its database, once a minute:
each service's, host's and check's status when it changes (so a year
stays small), an hourly sample per host (CPU and memory average and peak,
every disk's use) and every mail round. While obs-beacond is down nothing is
known: on restart each status is marked unknown from a minute after the
last record, and uptime counts only the time Cairn OBS was watching. History
older than 400 days is dropped.

The Reports page covers the last 7 or 30 days or any month: uptime per
service, host and check (down is time critical, degraded time warning or
serious), incidents people were told about with time to acknowledge and to
fix and a cause admins can write, certificates by expiry, disk use with a
straight-line forecast of when each disk fills, CPU and memory per day,
the mail monitor's rounds and block-list time, and backups. Every section
downloads as CSV (`/api/v1/reports/csv/<section>?period=`); printing the
page gives a clean PDF.

`reports` in obs-beacond.yaml (optional): `timezone` (where months and
days start, what the Reports page shows times in, and the "Since" time in
alert notices; default the
server's own: `TZ`, else what `/etc/localtime` links to, else UTC) and `digest`: `weekday` (monday), `at` (08:00 in
that timezone) and `channels` (alert channel ids; default every email
channel). The digest summarises the last seven days: what's wrong now,
uptime (services below 100% by name), incidents, backups, certificates
expiring within 30 days, disks filling within 90 and mail. Email gets the
whole text, chat a line and a link. It's sent once per week even across
restarts; admins can preview it and send it by hand on the Reports page.

## checks.yaml: active checks

`checks` in obs-beacond.yaml points at it. obs-beacond re-reads it when it changes
(a file that doesn't parse is logged and the last good list stays), and
hands each agent its share; agents run them and send the latest results
with every report. See `config/checks.example.yaml`.

Without a `checks` file, admins add and edit checks under Configure › Checks
and obs-beacond keeps them in its database, in this same format (see
"Settings edited in the UI"). Ids made there are letters, digits, spaces,
dots, dashes and underscores, up to 64.

Each check has an `id`, a `name` and exactly one of:

| kind | value | options |
|---|---|---|
| `http` | URL | `status` (200), `contains` (text the page must have). Redirects are not followed |
| `tcp` | host:port | `tls` (TLS from the first byte, e.g. 465, 993), `contains` (the greeting's first line) |
| `dns` | name | `type` (A, AAAA, MX, TXT, CNAME, NS; default A), `server` (ip:port; default the host's), `contains` |
| `ping` | host or address | three echoes; passes on one reply, reports loss and the average round trip |
| `rbl` | an IPv4 address | `lists` (DNS zones; default SpamCop, Barracuda, UCEPROTECT 1, PSBL, Mailspike), optional `server`; fails if listed anywhere; every 30m by default |

Common options: `every` (60s, at least 30s), `timeout` (10s), `from`
(hosts that run it; default every agent), `severity` when down
(critical or warning), `tls_warn_days` (21) and `tls_critical_days` (7)
for any certificate the check sees.

A check is **down only when two hosts agree** (or the only host running
it says so). One host failing alone shows on the Checks page as "failing
from hostN only" and doesn't alert. Results from a host that has gone
silent don't count. Certificates warn and go critical by the soonest
expiry any host saw.

Block lists: a list that answers 127.255.255.x is refusing our resolver,
not listing us (Spamhaus does this for public and hosting resolvers; it
needs its own resolver or a Spamhaus key), so it's noted and not counted.
The check fails only when the address is listed somewhere, or when no
list could be asked at all.

### Image drift

The same container name running different images (by name and tag) on
different hosts warns once it has lasted 15 minutes: a deploy that didn't
reach every host. Digests aren't compared, since hosts that build images
themselves get different digests for the same content.

## topology.yaml: corrections to the maps

`topology` in obs-beacond.yaml points at it; re-read when it changes. The
maps draw themselves from what the agents see (maps.md): sites from
shared private networks, links from WireGuard keys and traffic, services
from compose projects, containers and listening units, and what each
service needs from the connections between them. This file corrects and
names what's found; nothing in it is needed for the maps to draw.

`network`: `site` names the site holding most of the `hosts` listed, and
each host's `roles` (a few words, e.g. `"mail · Gitea"`) go on its card.
`remote` lists hosts outside the site (a desk machine at home on its own
tunnel), with `name` and `roles`. Every host named must be an agent.

`services`: `id`, `name`, `tier` (edge, public, apps, data), `needs`
(service ids or host names: declared edges, which group alerts), and what
makes up its state: `checks` (ids), `containers` and `units`
(`host/pattern` or `*/pattern`; a unit without a type is a .service) and
`cloudflare: true`. Each service claims the containers and units its
patterns match, which take their place under its own `id` and `name`;
one nothing matches is still drawn, as declared. A service's state is the
worst of its parts, and its history is kept for the Reports page.

Up to 0.1.0-rc.3 this file also drew two hand-drawn maps, with `mesh_ip`,
`mesh_check`, `network.mail`, a remote host's `via`, `link` and
`link_check`, and a service's `sub` and `note`. Those maps are gone, and a
file that still has those fields is refused: obs-beacond won't start with
it, and a running one keeps the last good file and logs why. Take them
out before upgrading.

### Root-cause grouping

obs-beacond groups alerts before they become incidents, from what
depends on what: the topology's `needs`, and the service map's edges
that are declared or learned and steady (docs/maps.md). A learned edge
only seen so far, or fading, never groups, and one rejected ("Not a
dependency") is gone for good. Each condition belongs to a node: a unit
or container to the service whose pattern matches it, or else to the
discovered service it's a part of; a check or certificate to the service
that lists it, Cloudflare trouble to the `cloudflare: true` service, and
a silent host to that host. A discovered service that runs on one host
depends on that host. Disk, memory and similar host warnings stand alone: they are
trouble on a host, not why its services fail.

When a failing node needs (directly or further down) another failing
node, only the deeper failure becomes an incident. Its detail says
"Also affected: …" with how many alerts were held back, and it takes the
group's worst severity, so a critical symptom still pages. An alert that
had already been sent on its own stays on its own, so nothing vanishes or
reads as "resolved" because its cause was found later. When a learned
edge did the grouping, the detail says so: "Grouped because webmail has
talked to postgresql :5432 since Nov 2." The Alerts page lists those
edges with **Not a dependency** for admins, which rejects the edge (a
map correction): the next evaluation splits the group. An incident also
links to where it is on the maps. What it holds back and why is kept
with the incident, so every copy shows it. The dashboard groups the same
way, and the service map names the likely cause on a failing service.

### Batches

After grouping by cause, problems that are alike and from one place are
one incident when there are three or more at once: "4 certificates from
pki_int in vault-prod end soon", "3 VMs in vcenter-phx: inaccessible".
Its detail names them, worst first (eight at most, then "and 5 more"),
and it takes the worst one's severity. What's alike:

| batch | what's in it |
|---|---|
| certificates ending, by issuer | the Certificates page's own alerts (hosts' ports and files), when ending is all that's wrong |
| certificates ending, by Vault mount or cluster namespace | Vault PKI leaves and issuers; cert-manager's certificates |
| Akamai certificates ending | CPS's deployed certificates |
| VMs, by cluster and state | a vCenter's, Nutanix's, Proxmox's or KubeVirt's VMs failing the same way |
| workloads, by cluster and how their pods fail | Deployments, StatefulSets and the like whose pods all fail the same way (CrashLoopBackOff, say) |

The safety rules are grouping's: one that has already paged on its own
stays on its own, and a batch that has paged stays one incident while
any of it is left, so it neither resolves early nor opens again as
something else. Certificates that have ended, or are weak or served
wrong as well, are never batched: each is its own job.

## expected.yaml: what should run where

Per host, `running` and `stopped` name containers; `units` names systemd
units that must be active (`inbuxa` means inbuxa.service). obs-beacond hands
each agent its units with its checks; an expected unit that isn't active
is critical, one that isn't installed too.

Without `expected` in obs-beacond.yaml, this is set per host under
Configure › Hosts instead: **Adopt what's running** lists the containers the agent sees and
the services running there (agents report them every five minutes), and
an admin picks what must stay running, what's stopped on purpose, and
which services must stay active. Configure › Hosts exports and imports
the whole file (see "Settings edited in the UI").

```yaml
hosts:
  node2:
    running:              # container names; * matches any run of characters
      - wiki-*
      - inbuxa-demo
    stopped:              # stopped on purpose: shown, never alerted on
      - match: cairnobs-demo-*
        note: Paused since 2026-09-18 while cairnobs development is on hold.
ci_jobs:                  # CI job containers: counted, never listed
  - GITEA-ACTIONS-TASK-*
clusters:                 # what's adopted in each cluster (Cluster agents, above)
  prod:
    namespaces:           # a whole namespace: page, warn or off
      shop: page
      batch: warn
    workloads:            # namespace/kind/name; a VM's kind is vm
      - name: shop/deployment/web          # alert defaults to page
      - name: shop/statefulset/db
        alert: warn
      - name: vms/vm/build-runner
        stopped: true                      # a VM stopped on purpose
  mgmt1:prod1:            # an IBM i partition, by its source (ibmi.md)
    subsystems: [QINTER, QBATCH, ORDLIB/ORDERS]   # must be active
```

An IBM i partition's `subsystems` must be active: one that isn't is
critical, and so is one that isn't there at all, or that its profile may
not see (it needs `*USE` on the subsystem description). Write a name, or
`library/name` when two libraries hold one of that name. The rest are
counted on Clusters and listed on Workloads, never alerted on. With
expectations kept in the UI, set them by exporting the file, editing it
and importing it.

How a container is judged:

| Situation | Shows as |
|---|---|
| matches `running`, running, healthy or no health check | OK · Running |
| matches `running`, health check failing | Critical · Unhealthy |
| matches `running`, restarting | Critical · Restarting |
| matches `running`, exited or never started | Critical · Not running |
| a `running` pattern matches nothing at all | Critical · Missing |
| matches `stopped`, not running | Off · Stopped on purpose |
| matches `stopped`, but running | Warning · Running (with the note) |
| on a host with expectations, running, matches nothing | Warning · Not expected |
| on a host with no expectations | judged on Docker's state alone |
| matches `ci_jobs` | counted only |

## Settings edited in the UI

Active checks, maintenance windows, alert channels and what should run
where can live in files or in the UI, one or the other for each:

| Setting | In a file when | Otherwise edited on |
|---|---|---|
| Active checks | `checks` in obs-beacond.yaml names a checks file | Configure › Checks |
| Maintenance windows | obs-beacond.yaml has a `maintenance` key (even `[]`) | Configure › Alerts |
| Alert channels | obs-beacond.yaml has an `alerts.channels` key (even `[]`) | Configure › Alerts |
| What should run where | `expected` in obs-beacond.yaml names an expected.yaml | Configure › Hosts, per host, and Configure › Clusters, per cluster; export and import on Configure › Hosts |

The web interface has two sides. The monitoring side shows only what's
set up: Mail, Data layer, Backups, Cloudflare and the maps appear once
their part of obs-beacond.yaml is there, Checks once there's a check,
Containers once a host reports Docker. Admins switch to **Configure** at
the top of the navigation for everything in the table above, the hosts,
users and the audit log; its Integrations page lists what isn't set up
yet and where to set it.

**A file wins.** While a setting is in a file the UI shows it read-only
and says where to change it, so a configuration tool that writes the
files keeps working. Editing in the UI needs `database`; without one
nothing set there could be kept.

Only admins edit. Each change goes in the audit log with who made it, and
reaches the agents with their next report; nothing restarts. A check or
window may name only hosts Cairn OBS knows (from obs-beacond.yaml or
joined). If a host is removed later, what names it stays as it is and
simply matches nothing until the next edit, which asks for the name to go.

**Export and import.** Each section has Export YAML and Import YAML
buttons. Checks export as a whole checks.yaml; maintenance windows as the
`maintenance:` block of obs-beacond.yaml. Defaults are left out, so an
export reads like a file written by hand. Import replaces the whole
section after asking, and only if every entry is valid; one bad entry and
nothing changes. To move a setting from the UI into files, export it,
put it where the table above says, and restart obs-beacond. To go the
other way, export, remove the file (or the key), restart, and import.

**Channel secrets.** A channel's topic, token, webhook URL, signing secret
or password is entered in the UI and kept in the database sealed with
the state key, like the other secrets there. It is never shown again: the
form says a secret is set, and leaving the field blank keeps it. An
optional one (ntfy's token, a webhook's signing secret) can be removed.
The audit log says when a secret was replaced, never what it is.

Exported channels leave their secrets out, so the file is safe to keep
or share. Importing keeps the stored secret of each channel with the same
id and type; a new channel in the file needs its secret written into it
as `topic`, `token`, `webhook`, `url`, `secret` or `password` (not
`*_file`, which is for obs-beacond.yaml only). Moving channels from the
UI into obs-beacond.yaml means putting each secret into a file and
adding the matching `*_file` setting.

Each channel has a send-a-test button, and the same critical rule holds:
a change that would leave critical alerts going out only by email is
refused.

## Coming next

Alert rules get their own file in the same style
when they're built: one entry per rule, patterns rather than repeated
entries where hosts share the same thing.
