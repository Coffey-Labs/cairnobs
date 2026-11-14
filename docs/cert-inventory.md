# Certificate inventory: design note

Status: decided 2026-11-14 (Decisions, at the end): every question went
the recommended way. Being built in the order under Plan; all five steps are done.

John asked for a certificate inventory: every certificate in the fleet,
where it's used and when it ends, from sites, host files, Kubernetes and
Vault's PKI. Automated rotation is a separate note (it writes, so it
needs its own decision first); this one only reads.

## What there is now

Certificates are already seen in four places, each judged on its own:

| where | what's seen | judged |
|---|---|---|
| site checks (checks.yaml) | the certificate an `https` or `tcp` `tls` check is served | `tls_warn_days` 21, `tls_critical_days` 7 |
| Cloudflare | edge certificate packs | `cert_warn_days` 14, `cert_critical_days` 3 |
| Vault PKI | every issuer and leaf certificate (vault.md, step 2) | 21 and 7 days (or a third / a tenth of a short life); `cert` records |
| obs-beacond itself | its ACME certificate, the agent CA | renewed by itself |

What's missing is everything nobody wrote a check for: the certificate
in `/etc/postfix`, the one on an internal service's port 8443, the one in
a Kubernetes Secret behind an Ingress, the same wildcard copied to twenty
hosts. And there's no one place that answers "what ends this month, and
where is it used?"

## What it would be

- **One kind, `cert`,** for every certificate from every source (Vault's
  already are), keyed by its SHA-256 fingerprint, so the same certificate
  found in many places is one certificate with many places.
- **A Certificates page** (Monitor → Infrastructure): every certificate,
  soonest end first; its subject and names, issuer, key type and size,
  and where it's used ("web1:443, web2:443, web1 /etc/nginx/tls/shop.pem,
  shop Ingress in prod"). Filters: ending within, issuer, source, host.
- **One alert per certificate**, not per place: "shop.example.org ends in
  6 days, used on 4 hosts and 1 Ingress". Places it was seen within the
  last day count; a certificate replaced everywhere stops alerting.
- **Read-only.** Public certificates only. No private key is ever read,
  copied or sent (decision below on Kubernetes, where that's the catch).

## Where certificates are found

1. **Ports hosts listen on (agent).** The maps already know every
   listening TCP port (maps.md). The agent opens each one on loopback
   once a day, tries a TLS handshake (and STARTTLS for SMTP 25/587, IMAP
   143, POP3 110, LDAP 389, PostgreSQL 5432), and keeps the certificate
   it's served. No files, no configuration, and it finds what nobody
   listed. A port that doesn't speak TLS costs one failed handshake a
   day.
2. **Files (agent, root helper).** Globs in agent.yaml, by default
   `/etc/letsencrypt/live/*/cert.pem`, `/etc/pki/tls/certs/*.crt`,
   `/etc/nginx/**/*.{pem,crt}`, `/etc/haproxy/**/*.pem`,
   `/etc/postfix/**/*.pem` and `/etc/dovecot/**/*.pem`; the CA bundles in
   `/etc/ssl/certs` are skipped. Read through the root helper, which returns only certificates
   parsed out of each file: a file holding a key and a certificate
   (haproxy's) gives back the certificate alone; the key's bytes never
   leave the helper.
3. **Kubernetes and OpenShift (cluster agent).** cert-manager's
   `Certificate` objects (`status.notAfter`, `spec.dnsNames`,
   `spec.secretName`) need only `get/list/watch` on
   `certificates.cert-manager.io`: no Secrets. For what cert-manager
   doesn't manage, the cluster agent handshakes each Ingress and Route
   host on its service, as an outsider would. (An OpenShift Route with
   its certificate inline carries its key too; the cluster watcher
   already lists Routes and decodes only their host, backends and
   whether TLS is on, so the key is dropped as it's read. The inventory
   would decode `spec.tls.certificate` alone.) Reading `kubernetes.io/tls`
   Secrets directly is an opt-in (question 2): it's the only way to see
   unused ones, but it grants the keys.
4. **Vault PKI** (done, vault.md): its `cert` records join the page.
5. **Site checks and Cloudflare:** their certificates join the page.
   Their alerts stay where they are, so nothing alerts twice
   (question 5).
6. **Windows** (when its agent comes): the LocalMachine `My` store,
   public parts only.

## What's judged

- **Ends:** 21 days a warning, 7 critical, as site checks and Vault; a
  short-lived certificate (under 7 days' life, ACME's 6-day ones
  included) by a third and a tenth of its life.
- **Served but wrong** (from handshakes only): a name the certificate
  doesn't cover, an incomplete chain (missing intermediate), a
  self-signed certificate on a port that had a CA-signed one. Warnings.
- **Weak** (question 6): SHA-1 signatures, RSA under 2048 bits.
  Warnings, or only shown.
- **Not judged:** CA bundles, and certificates in files no service
  serves, unless they end within 7 days (a renewed file nobody reloaded
  is the classic outage: question 4).

## Scale

Thousands of hosts × a few certificates each, plus Vault's. Records are
per certificate, not per place, so a wildcard on 500 hosts is one record
with 500 places (kept as a count and the first 50, on the page; all in a
table the page reads on demand). Handshakes run once a day per port,
spread over the day by the host's name hash; files are read when their
modification time changes.

## The page (step 1, 2026-11-14)

- **Certificates** (Monitor → Infrastructure, shown once Vault, site
  checks, Cloudflare or Akamai are set up): `GET /api/v1/certificates`
  (`q`, `within` days, `kind` host|file|vault|check|cloudflare|akamai|cluster,
  `status=problems`, `offset`, `limit` up to 1,000), one row per
  SHA-256 fingerprint with its places (the first 50, and a count).
- **Sources so far:** Vault's `cert` records (their facts now carry the
  fingerprint, key and signature), site checks' handshakes (a check
  result carries the certificate it was served, `cert`, from agents
  built with this change; older ones send only its end), and
  Cloudflare's edge certificate packs (the API gives no fingerprint, so
  a row per pack).
- **Judged on the page** by 21 and 7 days (a third and a tenth of a
  short life), weak keys and SHA-1 as warnings (decision 6). No new
  alerts: each source's stay (decision 5).
- **`internal/certinfo`** describes a certificate the same way for every
  source; the agent's handshakes and files (steps 2 and 3) will use it.

## Ports (step 2, 2026-11-14)

- **`internal/tlsscan`**, in the agent: the TCP ports the map sample
  saw listening (so `map` off means no scan; `cert_scan: false` turns
  it off alone). Each new port is tried within a minute or two (five
  every ten seconds), then once a day, spread over an hour by the host's
  name and the port. A listener on every address is dialed on loopback;
  one on a single address, on that address. No SNI is sent: a server
  with several names shows its default certificate.
- **STARTTLS** by port: SMTP on 25 and 587, IMAP 143, POP3 110, LDAP
  389, PostgreSQL 5432. Other ports get a TLS ClientHello straight
  away; one that doesn't speak TLS costs one failed handshake a day.
  22, 53, 111, 123, 2049 and 5355 are never tried.
- **Sent** with the report when the list changes, and every half hour;
  obs-beacond keeps the last, as it does the map sample. A place counts
  while it was seen in the last two days.
- **Alerts** (decision 3): one per certificate found on hosts, keyed
  `certinv/<fingerprint>`, "Certificate admin.internal ends in 5 days
  (…)", detail "Served on host1:8443 · admin, host1:25 (smtp
  STARTTLS) · postfix." A certificate a Vault, a site check or
  Cloudflare also has alerts there, not here (decision 5). On one host
  only, a maintenance window on that host holds it.
- **Tested** against fake TLS, SMTP, IMAP, POP3, PostgreSQL and LDAP
  servers; the server test has a certificate found on a host, in Vault
  and by a check (one row, four places, no inventory alert) and one
  only on a host (one alert, both its ports).

## Files (step 3, 2026-11-14)

- **`internal/certfiles`**: the agent asks the root helper every ten
  minutes for the certificates in the files its globs match
  (`cert_files` in agent.yaml; absent, the defaults above; `[]` reads
  none). The helper parses each file again only when its time or size
  changed and gives back the first certificate in it that isn't a CA's
  (or, with no other, a self-signed one marked a CA, as openssl's and the
  snakeoil are, when it has names or its key is beside it), with the
  file's path, time and how many more certificates it holds.
  Keys are skipped as they're parsed, and a file with no certificate
  gives back nothing, so the helper says nothing about other files.
  `/etc/ssl/certs`, `/etc/pki/ca-trust` and `/usr/share/ca-certificates`
  are never read, and a file of CAs alone (a bundle) gives back nothing.
  `**` is any number of directories (eight at most) and `{a,b}` either;
  5,000 files and 1 MiB a file at most.
- **Places** of kind `file`, "host1:/etc/letsencrypt/live/shop/cert.pem".
  A certificate only in files is judged at 7 days, not 21: a file no
  port serves may be nobody's. Its alert says "no port serves it".
- **Renewed but not reloaded** (decision 4): a port serving a
  certificate when its host has a file with a newer one for the same
  names, an hour after the file changed and after the port was tried
  again since. A warning on the certificate's own alert ("…; renewed but
  not reloaded"), and an alert of its own for a certificate a check or
  Vault has (only the inventory knows the file); the detail names the
  file and the ports to reload. On the page: "Not reloaded".
- **Tested** with a leaf and its chain, a key next to its certificate,
  a bundle of CAs, a key alone and a DER file (no key's bytes come back;
  a changed file is read again); the server test has a renewed file a
  port hasn't caught up with, a check's certificate the same, a spare
  file at 15 days (no alert) and one at 3 (an alert).

## Kubernetes (step 4, 2026-11-14)

- **cert-manager:** the cluster agent watches `certificates.cert-manager.io`
  (added to the ClusterRole; harmless where cert-manager isn't
  installed) and makes a `cert` record of each: its names, issuer
  ("ClusterIssuer letsencrypt"), begin and end from its status, renewal
  time. Not issued yet is Pending ("Not issued yet: <why>"); Ready False
  once issued is a warning ("Not ready: <why>; ends in …"). No Secret is
  read, and its Secret's name stays in the agent.
- **Ingress and Route hosts:** every TLS host (wildcards aside) is tried
  on 443 with itself as SNI, as a visitor would, at the ports' pace
  (step 2): new ones within minutes, then once a day spread over an
  hour. A Route with its certificate inline has it read from the Route
  (`spec.tls.certificate` alone; the key isn't decoded) and isn't tried.
  `obs-beacon-agent cluster -cert-scan=false` stops the handshakes.
- **One record per certificate:** what an Ingress serves from a Secret a
  Certificate fills joins that Certificate's record (`served_on`, and its
  fingerprint when the ends match); every other served certificate is a
  record of its own, `served/<fingerprint>`, listing where it's served:
  the router's wildcard on 300 Routes is one record. Judged by the agent
  at 21 and 7 days (a third and a tenth of a short life), as Vault's
  leaves are, and alerting as cluster records do (decision 5): "Certificate
  shop.example.org from shop in cluster-a: ends in 5 days", detail
  "cert-manager Certificate shop/shop-cert. Served on shop/ingress/shop
  shop.example.org."
- **On the page:** places of kind `cluster`, "cluster-a · shop ·
  Certificate shop-cert · served on …". A row known without its
  fingerprint (a Certificate whose Secret nothing serves, a Cloudflare
  pack) joins the row of the certificate with the same names ending the
  same second, when there's exactly one.
- **Tested** against fake objects: a Certificate served by an Ingress
  and not ready, one never issued, two Routes on the router's wildcard,
  a Route with its certificate and key inline (no key, nor a Secret's
  name, in the records), and the scanner's SNI on a fake server. Not
  yet against a cluster with cert-manager installed.

## Served wrong (step 5, 2026-11-14)

Judged where the handshake happens, from the chain alone (no root store,
nothing fetched), and sent as `wrong` with each port's certificate:

- **A name it doesn't cover:** a cluster's Ingress and Route hosts (tried
  with SNI) and Routes' inline certificates. Hosts' ports are tried
  without a name, so there's nothing to compare there; site checks
  already fail on a name mismatch.
- **An incomplete chain:** a CA-signed leaf that names its issuer's URL
  (AIA, as every public CA's does) sent alone, since public CAs never
  sign a leaf from a root; or sent with an intermediate that didn't sign
  it. A private CA's leaf without AIA, sent alone, isn't judged: clients
  that trust that CA have it.
- **Self-signed where a CA-signed one was:** the scanner remembers, per
  port, that a CA-signed certificate was served (until the agent
  restarts).
- **Weak** keys and SHA-1 were already judged (step 1).

On a host, a warning on the certificate's own alert ("…; served wrong",
detail "Served wrong: host1:443 · nginx: sent without its intermediate
certificate."), or an alert of its own for a certificate a check or
Vault has. In a cluster, the record is a warning at least, its word
ending "; served wrong: <place>: <what>". On the page: "Served wrong".

Akamai's CPS certificates joined the page later (akamai.md, step 2):
kind `akamai`, one place per enrollment, alerts on the Akamai page.

## Plan

One PR each, after the questions:

1. The `cert` record grows the fields the inventory needs (fingerprint,
   names, issuer, key, places) and the Certificates page, fed by Vault's
   records and site checks.
2. Handshakes on listening ports (agent), with STARTTLS.
3. Files, through the root helper.
4. Kubernetes: cert-manager Certificates and Ingress/Route handshakes.
5. Served-but-wrong and weak-key checks.

## Questions

1. **Hosts.** (a) Handshakes on listening ports plus files by glob
   (recommended: handshakes find what's actually served, files find
   renewed-but-not-reloaded ones). (b) Handshakes only. (c) Files only.
2. **Kubernetes Secrets.** (a) No Secrets: cert-manager Certificates and
   handshakes to Ingresses and Routes (recommended: no key ever readable
   by the agent). (b) Also `kubernetes.io/tls` Secrets, opt-in per
   cluster, the certificate parsed out and the key dropped in the agent.
3. **Alert unit.** (a) One per certificate, listing its places
   (recommended). (b) One per place.
4. **A renewed file still being served old.** When a file on a host has
   a newer certificate than the one its port serves, (a) warn "renewed
   but not reloaded" (recommended), (b) only show it.
5. **Existing sources.** (a) Site checks' and Cloudflare's certificates
   show on the page, but their alerts stay where they are (recommended).
   (b) Move their alerts to the inventory.
6. **Weak keys and SHA-1.** (a) Warn (recommended). (b) Only show them.

## Decisions

John answered "1 yes 2 yes 3 yes 4 yes 5 yes 6 yes" (2026-11-14): each
question's recommended answer.

1. **Hosts:** handshakes on listening ports and files by glob.
2. **Kubernetes:** no Secrets. cert-manager Certificates and handshakes
   to Ingresses and Routes.
3. **One alert per certificate**, listing its places.
4. **Renewed but not reloaded** warns.
5. **Site checks' and Cloudflare's certificates** show on the page, and
   their alerts stay where they are.
6. **Weak keys and SHA-1** warn.
