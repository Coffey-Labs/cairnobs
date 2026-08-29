# Security Policy

## Supported Versions

Cairn OBS is under active development. Security fixes are applied to the latest state of the `main` branch. Older tags/releases are not guaranteed to receive backported fixes.

| Version         | Supported          |
| --------------- | ------------------ |
| `main` (latest) | :white_check_mark: |
| Older releases  | :x:                |

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.** Public issues are visible to everyone, including potential attackers, before a fix is available.

Instead, report security issues privately by emailing:

**johnellisATlinuxDOTcom**

Please include as much of the following as you can:

- A description of the vulnerability and its potential impact
- Steps to reproduce, or a proof-of-concept
- The commit of Cairn OBS affected
- Which component it lives in — `agent`, `ingest`, `api`, `search`, `alerting`, `enterprise`, `web`, `cli` or the deployment charts — since they are separate binaries with separate trust boundaries
- Whether it needs an authenticated session, and if so at what role

### What to Expect

- **Acknowledgment:** You should receive a response within a few days confirming the report was received.
- **Assessment:** The issue will be triaged and its severity assessed.
- **Fix & disclosure:** Once a fix is ready it will be published on `main`. We'll coordinate with you on public disclosure timing and credit, if you'd like to be credited.

### Scope

In scope:

- Authentication, session handling and token issuance in the control plane
- **Cross-tenant data exposure** — anything that lets a query, dashboard, alert rule or API call read data belonging to another tenant. This is the highest-severity class for this project.
- RBAC bypass: performing an action the signed-in role does not carry
- Query-language injection reaching ClickHouse or Tantivy, including through the SQL escape hatch, and anything that escapes the compiler's intended plan
- Log injection or parser flaws in the agent or ingest pipeline that lead to code execution, resource exhaustion, or forged records
- Credential or token exposure in logs, API responses, or the web bundle
- Dependency vulnerabilities that are actually exploitable in Cairn OBS's usage

Out of scope:

- The public demo's `demo` account credential, which is deliberately published — it is prefilled on the login page and baked into the web bundle, against a database that is wiped and reseeded on a schedule
- Findings against a deployment the reporter does not operate or have permission to test
- Vulnerabilities in ClickHouse, Redpanda, PostgreSQL or other third-party components with no demonstrated impact on Cairn OBS — please report those upstream
- Missing hardening with no demonstrated impact, and issues requiring physical access or an already-compromised host

### A note on unverified components

Two parts of the tree are known not to have been exercised, and are documented as such in the README:

- **Phase 4** (RBAC, tenant isolation, per-tenant ClickHouse) compiles and is tested, but only its audit-logging guarantees were confirmed against a live database.
- **The Windows agent** (`EvtSubscribe`, ETW, service registration) has never run on real Windows.

Reports against these are welcome and useful. Please say which one you were testing, so the finding is not mistaken for a regression in a verified path.

## Disclosure Policy

We follow coordinated disclosure: please give us a reasonable window to investigate and release a fix before any public disclosure. In turn, we'll keep you updated on progress and won't leave you waiting indefinitely.

Thank you for helping keep Cairn OBS and the people running it safe.
