# Akamai: design note

Status: decided 2026-11-14 (Decisions, at the end): every question went
the recommended way. Built: all five steps under Plan (2026-11-14), each
tested against fakes only.

John asked for an Akamai monitor like the Cloudflare one: read-only, so
Cairn OBS can say when Akamai is why a site is down, and when something
in an Akamai account has drifted, failed or is about to end.

## What there is now

The Cloudflare monitor (formats.md, Cloudflare) is the model. obs-beacond's
evaluator polls the API every 5 minutes with a read-only token and judges:
load balancer pools and origin health, SSL mode, edge certificates, 5xx
share, security blocks, DNS drift against an accepted baseline, and
Cloudflare's status page, shown but never alerting. Its edge certificates
already appear on the Certificates page.

## What Akamai has to read

All with GET (EdgeGrid-signed; one exception, the traffic report's
query, under Traffic, step 5) against the account's API host
(`akab-….luna.akamaiapis.net`). The products a customer has decide which
answer; a 403 or 404 means "not in this account" and that part is
skipped quietly, as Vault Enterprise's are.

| Product | API | What we take | Why |
|---|---|---|---|
| Global Traffic Management | GTM Config v1 (`/config-gtm/v1/domains`, each domain's datacenters and properties, `…/status/current`) and GTM Reporting v1 (`/gtm-api/v1/reports/ip-availability/domains/{d}/properties/{p}?mostRecent=true`, liveness test failures) | each property's datacenters and servers, alive or not, handed out or not; a domain's change stuck propagating | The Cloudflare pools' counterpart: the site is down when no datacenter is alive |
| Certificate Provisioning System | CPS v2 (`/cps/v2/enrollments`, each enrollment's production deployment) | the deployed certificate (PEM: names, issuer, end, fingerprint), pending changes, a renewal waiting on someone | Joins the certificate inventory with its fingerprint; a third-party certificate's renewal is manual and easy to miss |
| Property Manager | PAPI v1 (properties, `/papi/v1/properties/{id}/activations`) | each property's production and staging versions; activations FAILED, or PENDING far too long | A failed activation is a change that didn't happen, or rolled back |
| Edge DNS | Config DNS v2 (`/config-dns/v2/zones`, each zone's `recordsets`) | every record set | Drift against an accepted baseline, as Cloudflare's DNS |
| Traffic | Reporting API v2 (`delivery/traffic/current`: edge hits by CP code and response class; v1's `hits-by-cpcode` was discontinued 2026-04-01) | the 5xx share per CP code | As Cloudflare's 5xx alert |
| Akamai's status | akamaistatus.com (its JSON summary, if it serves the usual one; else not read) | open incidents | Shown, never alerting, as Cloudflare's |

Field names come from Akamai's own published API references and its Go
SDK (`AkamaiOPEN-edgegrid-golang`, Apache 2.0), as Pure's came from
py-pure-client. We'd write our own small client and EdgeGrid signer
(HMAC-SHA256 over the request, about 80 lines) rather than take the SDK:
it covers hundreds of endpoints we'd never call, and every one is code to
audit.

## Credentials

An Akamai API client with read-only grants on exactly these APIs (GTM,
GTM Reporting, CPS, Property Manager, Edge DNS, Reporting), made by the
customer in Control Center (Identity and Access → API clients). Its four
values live in an `.edgerc` file, the format every Akamai tool uses, on
the obs-beacond host, re-read on every poll like Cloudflare's token file.
obs-beacond never writes to Akamai.

## Judging

Following Cloudflare's thresholds and wording:

- A GTM property with no alive datacenter: critical. A datacenter (or a
  server in it) GTM sees as down: warning, its host named when a fleet
  host has its address.
- A GTM domain whose change has been pending more than 30 minutes, or
  was denied: warning.
- A CPS certificate ending within 21 / 7 days: warning / critical, as
  everywhere else; a renewal waiting on the customer (a third-party
  certificate, a change awaiting acknowledgement): warning.
- A Property Manager activation FAILED, or PENDING over an hour:
  warning.
- Edge DNS drift: warning until undone or accepted, as Cloudflare's.
- 5xx share per CP code over `error_warn_pct` / `error_critical_pct`
  with at least `error_min`: warning / critical.
- The API unreadable for 3 polls: warning; the last reading stays.

## The page

**Akamai** under Monitor → Infrastructure, shown once `akamai` is
configured: GTM domains with each property's datacenters (alive, handed
out), certificates (also on Certificates), properties with their
production and staging versions and last activation, DNS zones with
drift, traffic per CP code, Akamai's status. The fleet simulator gets a
fake Akamai so the demo has one.

## Scale

An enterprise account: tens of GTM domains, hundreds of properties and
certificates, thousands of DNS records. Every 5 minutes for GTM status
and traffic; properties, CPS and DNS every 15 minutes, each zone's
records only when its serial changed. Akamai's rate limits (CPS's list
of active certificates is 10 a minute per account) are kept under, and a
429 waits as told.

## Testing

Akamai has no free tier, and we don't know of an account to test
against. So, as Nutanix was: fakes built from the published API
references, signature checking included, in the unit tests and the
fleet simulator; nothing is billed. Untested against a real account
until someone with one tries it, and the docs say so.

## Plan

One PR each, after the questions:

1. EdgeGrid client and `.edgerc`; GTM (config, status, IP availability);
   the page; alerts; fake.
2. CPS certificates, on the page and in the inventory.
3. Property Manager activations.
4. Edge DNS drift, with the same accept flow as Cloudflare's.
5. Traffic and Akamai's status; the fleet's fake Akamai.

## GTM (step 1, 2026-11-14)

- **`internal/akamai`:** our own EdgeGrid signer (checked against the
  signing key and "simple GET" signature in Akamai's published test
  data), `.edgerc` reading, and a reader for GTM: `/config-gtm/v1/domains`,
  each domain, its `status/current`, and each property's
  `ip-availability?mostRecent=true` report, four at a time.
- **obs-beacond:** the `akamai` section (formats.md, Akamai), polled by
  the evaluator and shared with the other copies as Cloudflare's reading
  is; the alerts under Judging for GTM; `GET /api/v1/akamai`; the Akamai
  page (Monitor → Infrastructure). A failing server's address is matched
  to a host's interface, so a maintenance window on it holds the alert
  and the incident groups under the host.
- **`dev/fakeakamai`:** a stand-in API that checks every signature, with
  two domains over three datacenters (a property failing over, one with
  no live datacenter, a change stuck propagating), writing the `.edgerc`
  to use.
- Not yet against a real account (decision 5).

## CPS certificates (step 2, 2026-11-14)

- **Read:** every enrollment (`/cps/v2/enrollments`, per contract: the
  `contracts` configured, else those `/papi/v1/contracts` lists, else
  none named), each one's production deployment
  (`…/deployments/production`) and, when a change is pending, that
  change (`…/changes/{id}`) for the input it needs. CPS versions its
  media types; each call names the newest we know first (enrollments
  v11, deployment v8) and falls back to the older ones (v10, v7) when
  the API answers 406 or 415. An enrollment not on production yet has no
  certificates and isn't an error. Only the public certificates are
  read; CPS never hands out keys.
- **Judged:** each deployed certificate (RSA and ECDSA are judged
  apart) at 21 and 7 days, or a third and a tenth of a short life, as
  on the Certificates page; a pending change that needs the customer's
  input (a third-party CSR or certificate, a staging check, warnings to
  acknowledge): warning. The detail says who renews it: a third-party
  certificate is the customer's; otherwise CPS renews well ahead, so one
  this close means a renewal stalled.
- **Shown:** a Certificates panel on the Akamai page (each enrollment,
  its deployed certificates, its change), and every deployed certificate
  on the Certificates page as kind `akamai` with its fingerprint, so a
  copy also served from a host's port joins the same row. In reports'
  certificate list as source `akamai`.
- **Fake:** enrollments that only speak the older media types, with
  generated certificates (`akamai.FakePEM`); `dev/fakeakamai` has five:
  RSA and ECDSA pairs, one whose renewal stalled (4 days left), a
  third-party one waiting on its CSR, one not deployed yet.
- **Pacing:** CPS is read every 15 minutes, not every poll (Scale);
  the reading between carries the last one. A CPS reading that fails
  keeps the last enrollments and says so on the page, without holding
  up GTM's.
- Not yet against a real account (decision 5).

## Property Manager (step 3, 2026-11-14)

- **Read:** `/papi/v1/groups`, each group's properties in each of its
  contracts (`/papi/v1/properties?contractId=…&groupId=…`: the latest
  version and the versions active on staging and production) and each
  property's activations (`…/properties/{id}/activations`), keeping the
  newest on each network. Activations' notify addresses aren't taken.
- **Scale:** every 15 minutes, with CPS. A property's activations are
  read again only when its versions moved, its newest activation hadn't
  finished or had failed, its last reading failed, or 6 hours passed;
  an enterprise's hundreds of settled properties cost one list call per
  group and contract.
- **Judged:** the newest activation on a network FAILED: warning,
  saying which version stays active and the activation's note; one
  still pending (NEW, PENDING, ZONE_1–3, …) over an hour: warning. Both
  networks alike; a failed one stays until a newer activation there.
- **Shown:** a Properties panel on the Akamai page, problems first, the
  first 50 unless asked: the latest version and each network's newest
  activation.
- **Fake:** groups, properties and activations (`SetPMProperty`);
  `dev/fakeakamai` has six: settled ones, a production activation that
  failed, one stuck on staging, one rolling out.
- Not yet against a real account (decision 5).

## Edge DNS (step 4, 2026-11-14)

- **Read:** every zone (`/config-dns/v2/zones`, 500 a page) and each
  zone's record sets (`…/zones/{zone}/recordsets`, 500 a page), SOA
  left out (its serial moves with every change). ALIAS zones are listed
  but not read: their records are another zone's.
- **Scale:** every 15 minutes, with CPS and Property Manager; a zone's
  record sets are read again only when its `versionId` changed, so
  thousands of records cost one list call while nothing moves.
- **Judged:** as Cloudflare's DNS: the first reading of a zone is its
  baseline (kept in the database, `ak_dns:<zone>`, so a change made
  while obs-beacond was down is still caught); any record set added,
  removed or changed (TTL included) since then is a warning, until it's
  undone or accepted. `POST /api/v1/akamai/dns/{zone}/accept` (admins;
  audited) takes today's records as the baseline. Cloudflare's baseline
  code is now shared.
- **Shown:** an Edge DNS changes panel on the Akamai page with Accept,
  as Cloudflare's; the zones and record-set count in a line. The record
  sets themselves aren't sent to the page.
- **Fake:** zones with paging and versions (`SetZone`); `dev/fakeakamai`
  has three, one of 1,500 records, and changes an MX 20 minutes after it
  starts (`-drift-after`).
- Not yet against a real account (decision 5).

## Traffic, Akamai's status and the fleet (step 5, 2026-11-14)

- **Traffic:** the Reporting API v2's `delivery/traffic/current`
  report, edge hits by CP code and response class (`edgeHitsSum`,
  dimensions `cpcode` and `responseClass`). This note planned v1, but
  v1's `hits-by-cpcode` was discontinued on 2026-04-01, and v2 takes a
  report's query only as a **POST** to
  `/reporting-api/v2/reports/delivery/traffic/current/data`. That is the
  one request that isn't a GET: a query body, changing nothing. The
  client refuses any other POST, the fake allows no other, and the API
  client still needs only read grants. With no time range the report is
  the latest complete 5-minute window, about ten minutes behind. Read
  every poll; the hit-based limit is 20 requests a minute per account.
  The EdgeGrid signer now hashes a POST body (`max-body`, default
  131072), checked against Akamai's published POST vectors.
- **Judged:** a CP code's 5xx share at `error_warn_pct` /
  `error_critical_pct` (5 / 25) with at least `error_min` (20) 5xx:
  warning / critical, as Cloudflare's. CP codes are shown by number.
- **Akamai's status:** akamaistatus.com serves the usual Statuspage
  summary (`/api/v2/status.json`); its indicator is shown at the top of
  the page, never alerting, as Cloudflare's. A stand-in's own
  `/status.json` is read when the `.edgerc` host is one on loopback.
- **Shown:** a Traffic panel, busiest first (the first 20 and every
  failing one unless asked).
- **Fleet:** `fleet -akamai DIR` serves an enterprise account on
  loopback (8 GTM domains over the first sites' load balancers, so a
  failing server is a fleet host; 180 CPS enrollments; 320 properties;
  40 zones, one of 5,000 records; 320 CP codes) and writes the
  `.edgerc` and `akamai.yaml` to DIR. Its problems are listed in
  `scripts/fleet/akamai.go`.
- Not yet against a real account (decision 5).

## Questions

My recommendation first in each.

1. **Who reads it?** (a) obs-beacond's evaluator, as Cloudflare: it's an
   Internet API, one reader, credentials in one place. (b) A host agent
   with an `akamai:` section, as Pure: only if the API host must be
   reached from inside a customer's network.
2. **What to read.** (a) All six above, built in the order under Plan.
   (b) GTM and CPS first, the rest when someone asks.
3. **Credentials.** (a) An `.edgerc` file and a section name (default
   `default`), the format Akamai's tools use. (b) Four values in
   obs-beacond.yaml with a secret file.
4. **Alerts.** (a) As under Judging, mirroring Cloudflare's. (b) Show
   everything, alert only on GTM and certificates at first.
5. **Testing.** (a) Fakes only, as Nutanix: no cost. (b) Look for a
   trial account through Akamai sales first (cost unknown; a sales
   conversation).

## Decisions

John, 2026-11-14: "go with your recommendations".

1. **obs-beacond's evaluator reads it**, as Cloudflare.
2. **All six**, built in the order under Plan.
3. **An `.edgerc` file** and a section name (default `default`).
4. **Alerts as under Judging**, mirroring Cloudflare's.
5. **Fakes only**, as Nutanix: nothing billed.
