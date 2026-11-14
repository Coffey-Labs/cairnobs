# Certificate rotation: design note

Status: decided 2026-11-14 (Decisions, at the end): every question went
the recommended way. All three steps built. Everything Cairn OBS does today only reads, so this note
is the first that would let it change something on a customer's
machines. That's why it's a separate decision.

John asked for certificate monitoring "and possibly cert rotation". The
inventory (cert-inventory.md) now finds every certificate: on hosts'
ports and in their files, in Kubernetes (cert-manager and what Ingresses
and Routes serve), in Vault's PKI, on Cloudflare and on Akamai. This note
is about what Cairn OBS could do once it has found one that's about to
end, or was renewed but isn't served yet.

## What already renews certificates

Most certificates already have something renewing them. Cairn OBS
shouldn't take that job over:

| Where | Who renews | What goes wrong |
|---|---|---|
| Let's Encrypt on a host | certbot, lego, acme.sh on a timer | the timer stopped, the challenge fails, or the renewal worked and nobody reloaded the service ("renewed but not reloaded", decision 4 of the inventory) |
| cert-manager | cert-manager, at two thirds of a certificate's life | the issuer is failing; a Certificate stuck not Ready |
| Vault PKI | whoever asks Vault (Vault Agent, consul-template, a script) | the asking stopped |
| Cloudflare, Akamai CPS | the provider | a third-party certificate's renewal waits on the customer |
| Hand-made certificates | a person, once a year | the person forgot |

So the useful, safe part of "rotation" is finishing what the renewer
started, not becoming a certificate authority's client.

## What it would do

Two actions, both on certificates the inventory already sees, both off
until the customer turns them on:

1. **Reload a service that's serving an old certificate** after its file
   was renewed (the inventory's "renewed but not reloaded"). The host's
   agent runs `systemctl reload <unit>` through the root helper. That's
   the most common failure in practice and the safest fix: the new
   certificate is already in place, and a reload doesn't drop
   connections for nginx, haproxy, Apache, Postfix or Dovecot.
2. **Ask cert-manager to renew a Certificate now** (what `cmctl renew`
   does: it sets the Certificate's `Issuing` condition), for one that is
   close to its end and isn't renewing. The cluster agent does it, only
   with the extra permission described below.

What it would not do: issue certificates (no ACME client, no Vault
writes), copy keys anywhere, restart services that can't reload, or
touch Vault, Cloudflare or Akamai. Those stay read-only.

## How an action reaches an agent

Agents only ever call obs-beacond; nothing calls an agent. Checks
already use that: a report's 204 carries `X-Beacon-Checks` and the agent
fetches its list. Actions would do the same: `X-Beacon-Actions:
<version>`, then `GET /api/v1/agent/actions`, and the result in the next
report. An action is at most a few minutes old when it runs, and is
dropped after 10 minutes.

## Who decides

The agent has the last word, not obs-beacond, so that a stolen
obs-beacond (or an admin's session) can't make every host in a fleet do
something:

- **The agent's own config lists what it may do**, off by default:

  ```yaml
  cert_reload:
    - unit: nginx.service
      files: [/etc/letsencrypt/live/*/fullchain.pem]
    - unit: haproxy.service
      files: [/etc/haproxy/certs/*.pem]
  ```

  A reload is refused unless the unit is listed, the certificate it
  serves came from one of that entry's files, and the agent checks for
  itself that the file is newer than what's served, parses, isn't
  expired and its key matches (the helper does that; keys still never
  leave it). Units without `ExecReload` are refused.
- **The cluster agent needs a flag and a role.** `cluster
  -cert-renew` and a ClusterRole rule allowing `patch` on
  `certificates/status` (cert-manager.io), shipped as a separate,
  optional manifest. Without both, it can't.
- **obs-beacond needs a person.** An admin presses "Reload nginx on
  web1" or "Renew now" on the Certificates page; every press is in the
  audit log with who and what came back. No automatic mode at first
  (Questions, 2).

## What changes in the agent

- The root helper gets its first endpoint that changes something:
  `POST /obs/reload?unit=…` (systemd's D-Bus `ReloadUnit`), and only for
  units in `cert_reload`. The helper reads the agent's config itself
  rather than trusting the agent's request, so a compromised agent
  process can't reload other units either.
- The packages stay the same; `cert_reload` absent means nothing
  changes.

## Testing

Reloads: in the systemd containers the package tests already use
(Debian and RHEL), with nginx and haproxy, a renewed file, and a check
that the port serves the new certificate after the reload and that an
unlisted unit is refused. cert-manager: on the kind cluster, with
cert-manager and a self-signed issuer. No cost.

## Plan

If the answers are yes:

1. The actions channel (protocol, agent, server), with nothing that
   uses it yet.
2. Reloads: `cert_reload`, the helper's endpoint, the button and the
   audit entry.
3. cert-manager renew: the flag, the optional manifest, the button.

## The channel (step 1, 2026-11-14)

- **obs-beacond:** a table, `agent_actions` (migration 17), holds each
  action: agent, kind, arguments, who asked, when it was taken and what
  came of it. A report's 204 carries `X-Beacon-Actions: 1` while one
  waits; `GET /api/v1/agent/actions` hands them over and marks them
  taken in one statement, so two copies never hand one over twice. An
  action not taken within 10 minutes never is. Results come back in the
  next report and are audited (asked, done, not done and why).
  `GET /api/v1/actions` (operators and admins) lists the newest 200.
  Without a database there are no actions.
- **The agent:** fetches its actions after a report's answer says some
  wait, runs each kind it knows, refuses the rest with the reason, and
  sends every result once in its next report. protocol.md, Actions.
- Nothing asks for an action yet: reloads are step 2.

## Reloads (step 2, 2026-11-14)

- **agent.yaml:** `cert_reload` (formats.md), absent by default. The
  agent checks it, and that the port is one the host listens on, then
  handshakes the port and asks the root helper.
- **The helper:** `POST /obs/reload`, its first endpoint that changes
  anything. It reads `cert_reload` from agent.yaml itself (`helper
  -config`, default `/etc/obs-beacon/agent.yaml`) on every request,
  one at a time, and reloads only when one of the unit's files holds a
  certificate for the same names as the served one that ends later, has
  started, hasn't ended and matches its key (in the same file, or
  `privkey.pem` or `<name>.key` beside it), and `systemctl show
  CanReload` says yes. Then `systemctl reload`; never a restart. Keys
  are read only to compare. The design said D-Bus `ReloadUnit`;
  `systemctl` does the same over systemd's private socket, as root,
  without a D-Bus library.
- **The agent** then handshakes again, for up to 15 seconds, and says
  done only when the port serves the renewed certificate: "reloaded
  nginx.service; 127.0.0.1:443 now serves /etc/letsencrypt/live/…".
- **obs-beacond** offers a reload on a "renewed but not reloaded"
  certificate only where the host's reported `cert_reload` has a unit
  whose globs match the renewed file. `POST /api/v1/certificates/reload`
  (admins) takes only an offered one, refuses a second while one waits,
  and audits who asked and for what. The Certificates page shows the
  button in the open row, with the last reload's state; without
  `cert_reload` it says how to allow it.
- **Tested** on real systemd in Debian 13 and AlmaLinux 9 containers,
  through the packaged, hardened helper unit: nginx (both) and haproxy
  (Debian, certificate and key in one file) reloaded and served the
  renewed certificate with the same main process; an unlisted unit, a
  unit without `ExecReload`, a file not renewed and a key that doesn't
  match were each refused, and the unit without `ExecReload` kept
  running.

## Renew now (step 3, 2026-11-14)

- **The cluster agent:** `-cert-renew`, off by default. With it, the
  `cluster` record says so, and a `renew` action for a Certificate sets
  its `Issuing` condition to True (reason `ManuallyTriggered`), keeping
  the other conditions, with the Certificate's `resourceVersion` so a
  change made meanwhile fails rather than being overwritten: what
  `cmctl renew` does. One cert-manager is already renewing is refused.
  Actions come with the agent's batches (protocol.md, Workloads).
- **The role:** `deploy/kubernetes/cluster-agent/cert-renew.yaml`, a
  ClusterRole with `patch` on `certificates/status` (cert-manager.io)
  and its binding; nothing else. Without it the agent says what to
  apply. `/cluster.yaml?cert_renew=1` is the manifest with the flag and
  the role, for adding, upgrading and taking out the agent alike.
- **obs-beacond** offers "Renew now" on a certificate that needs a look
  (21 days, or a third of a short life) behind a cert-manager
  Certificate, when its cluster agent has `-cert-renew` and the
  Certificate isn't being renewed. `POST /api/v1/certificates/renew`
  (admins) takes only an offered one, one at a time, audited.
- **Tested** against a stand-in Kubernetes API (the patch, its content
  type and every refusal), through obs-beacond's batches end to end, and
  against cert-manager v1.18.2 on a kind cluster with a self-signed
  issuer, as the cluster agent's own service account: without the role
  it said to apply cert-renew.yaml; with it, cert-manager issued
  revision 2 into the Secret, and asking again while it did was refused.
  The role allows patching a Certificate's status and nothing else (not
  its spec, not Secrets).

## Questions

My recommendation first in each.

1. **Should Cairn OBS change anything at all?** (a) Yes, these two
   narrow actions, each off until a customer turns it on. (b) No: keep
   Cairn OBS strictly read-only and stop at telling people. Saying "it
   only reads" is easy to explain to a security team, and that's worth
   something too.
2. **Who starts an action?** (a) An admin presses a button each time.
   (b) Also an automatic mode, per host, after the inventory's one-hour
   grace. Automatic could come later, once people trust the button.
3. **Where is it allowed?** (a) In each agent's own config (and the
   cluster agent's flag and role). (b) Only in obs-beacond's config.
   That's easier to manage, but then a stolen obs-beacond could act
   everywhere.
4. **cert-manager renew?** (a) Yes, behind its flag and optional role.
   (b) Reloads only for now.
5. **Issuing certificates** (an ACME client in the agent, or writing to
   Vault's PKI)? (a) No: certbot, cert-manager and Vault Agent already
   do that well, and owning keys changes what Cairn OBS is. (b) Yes,
   later, as its own note.

## Decisions

John, 2026-11-14: "go with recommendations".

1. **Yes, the two narrow actions**, each off until a customer turns it
   on: reloading a service that serves an old certificate after its
   file was renewed, and asking cert-manager to renew now.
2. **An admin presses a button each time**, audited. No automatic mode
   for now.
3. **Each agent's own config allows it** (`cert_reload`), and the
   cluster agent's flag and role.
4. **cert-manager renew: yes**, behind its flag and optional role.
5. **No issuing certificates.**
