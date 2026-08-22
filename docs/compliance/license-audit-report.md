# License compliance audit report — Phase 6

**This report is a strong first pass, not a legal opinion.** It should
be reviewed by actual legal counsel before the project is publicly
released, pitched to customers, or used as the basis for any compliance
claim. Nothing in this document should be represented to a third party
as legal advice or a certified compliance determination.

## Scope and goal

Every component in the monorepo — `/agent`, `/transport`, `/ingest`,
`/storage`, `/api`, `/web`, `/cli`, `/deploy`, and `enterprise/` — is
licensed AGPLv3, with no separate commercial-license carve-out anywhere
in the project. This audit inventories every third-party dependency
across every language ecosystem, classifies each for AGPLv3
compatibility, resolves or explicitly flags anything that doesn't
resolve cleanly, and stands up CI enforcement so this doesn't need to be
manually re-audited from scratch every time a dependency changes.

This is a compatibility audit, not a "replace every permissively
licensed library" exercise. MIT, Apache-2.0, BSD, and ISC dependencies
are all fine to depend on from AGPLv3 code. The concern is dependencies
with licenses that are genuinely incompatible, impose unaccounted-for
obligations, or aren't open source at all (source-available licenses
like BSL/SSPL, anything with a field-of-use or non-compete restriction).

## Methodology

1. **Inventory** every dependency, direct and transitive, in every
   language ecosystem present in the repo:
   - **Rust** (`agent` workspace: `cairnobs-agent`, `cairnobs-parser`;
     `search`): `cargo-deny` (`cargo deny list --format tsv`), installed
     fresh for this audit (`cargo install cargo-deny --locked`).
   - **Go** (`api`, `ingest`, `alerting`, `enterprise`,
     `deploy/operator`, `terraform`, `proto`,
     `hack/benchmark-fixture`, `hack/windows-fixture` — every module with
     real third-party dependencies; `cli`, `hack/webhook-sink`, and
     `hack/alert-load-test` are stdlib-only, confirmed by inspecting
     their `go.mod` files, not scanned): `go-licenses`
     (`google/go-licenses`, installed via `go install ...@latest`),
     `go-licenses csv ./...` per module.
   - **npm** (`web`): `license-checker` (`npx license-checker --json`).
   - **Vendored/non-manifest content**: a repo-wide grep for
     copy/attribution markers ("adapted from", "copied from",
     "stackoverflow", stray copyright headers, "vendored") — zero hits.
     A separate pass for binary/asset files (fonts, icons, images)
     outside `node_modules`/build output — two font files and one SVG
     found, see below. Docker base images referenced in
     `docker-compose.yml` were pulled out and reviewed manually, since
     they're not a language-ecosystem dependency any of the three
     scanners above would see.
2. **Classify** every distinct license found into: (a) clearly
   compatible as a dependency, (b) requires a closer look, (c) actually
   incompatible or non-open-source. Every (b)/(c) result is cited with
   real reasoning below, not asserted.
3. **Named risk areas** (Redpanda's licensing history, ClickHouse client
   libraries, Phase 5 font/asset files, Kubernetes Operator tooling)
   checked explicitly, against primary sources (actual license files at
   the actual pinned versions/tags), not general recollection.
4. **Remediation** options recorded for every (c)/unresolved-(b) item —
   fix, isolate, or flag for a business decision. Nothing was silently
   swapped.
5. **Own license declarations audited**: root `LICENSE` file, per-ecosystem
   manifest `license` fields, and a repo-wide check for any file still
   claiming a license other than AGPLv3.
6. **`enterprise/` relicensed** to AGPLv3, closing the former
   commercial-license carve-out project-wide.
7. **CI enforcement** stood up so this audit's findings don't silently
   go stale.

## Inventory summary

Full machine-readable inventory: `/docs/compliance/license-inventory.csv`
and `.json` (columns: `component`, `dependency`, `version`, `license`,
`direct_or_transitive`, `ecosystem`, `flagged`, `flag_reason`,
`classification`).

| Ecosystem | Component-scoped rows | Unique dependencies | Flagged (unique) |
|---|---|---|---|
| Rust (cargo) | 440 | — | 4 |
| Go | 255 | — | 9 (`segmentio/asm` + 8 HashiCorp MPL-2.0 packages) |
| npm | 75 | — | 3 |
| Docker base images | 3 | 3 | 1 (Redpanda) |
| Font/vendored assets | 3 | 3 | 1 (favicon) |
| **Total** | **776** | **502** | **23** (~4.6%) |

Rows double-count dependencies shared across multiple components by
design (e.g. `pgx` appears once per Go module that imports it) — that's
what makes the `component` column meaningful. All 502 unique
dependencies resolved to classification (a) except one: Redpanda,
classification (c), recorded below with remediation options rather than
resolved unilaterally.

## Classification results

**774 of 776 rows (all but Redpanda and the favicon asset) classify as
(a): clearly compatible.** No dependency in this project's tree required
a genuine (b)-category deep-dive that didn't resolve cleanly — every
item that looked ambiguous at first pass (dual-licensed with a copyleft
option, a non-standard license file, an unfamiliar SPDX identifier)
turned out, on actual inspection, to resolve to (a) once read carefully.
That's a real result of doing the reading, not an assumption going in —
recorded below per item so the reasoning is checkable.

### Dual/multi-licensed crates electing a permissive branch (Rust)

Four Rust crates carry an SPDX OR expression that includes a
copyleft/less-common option alongside a permissive one. Standard
practice for a disjunctive license grant is that the downstream user
elects whichever listed option they prefer — we elect the permissive
branch in every case, incurring zero copyleft obligation:

| Crate | License expression | Elected | Note |
|---|---|---|---|
| `r-efi` | `MIT OR Apache-2.0 OR LGPL-2.1-or-later` | MIT/Apache-2.0 | Build-dependency only (via `tonic-build`→`prost-build`→`tempfile`→`getrandom`), for a UEFI target this project doesn't build for — doesn't even ship in a release artifact. |
| `fastdivide` | `MIT OR zlib-acknowledgement` | MIT | |
| `htmlescape` | `Apache-2.0 / MIT / MPL-2.0` | Apache-2.0/MIT | MPL-2.0 would also have been fine on its own merits (see below). |
| `ryu` | `Apache-2.0 OR BSL-1.0` | Apache-2.0 | `BSL-1.0` here is the Boost Software License — permissive, unrelated to Redpanda's Business Source License below despite the shared abbreviation. |

### MPL-2.0 (Go, npm, and one Rust option above)

MPL-2.0 is file-level (weak) copyleft: modifications to MPL-covered
*files* must stay available under MPL if distributed, but combining
MPL-covered code into a larger differently-licensed work — including an
AGPLv3 work — does not require the larger work to relicense (MPL 2.0
§3.3, "Distribution of a Larger Work"). Pre-cleared as category (a) per
this audit's own scope definition. Found in:

- **Go** (`/terraform` only, HashiCorp's Terraform provider SDK and its
  own dependencies): `go-plugin`, `go-uuid`, `terraform-plugin-framework`,
  `terraform-plugin-go`, `terraform-plugin-log`, `terraform-registry-address`,
  `terraform-svchost`, `yamux`. Lower risk still than the general case:
  `/terraform` is its own standalone Go module producing a Terraform
  provider plugin binary, not linked into any core service.
- **npm**: `axe-core` (dev-only, used for the Phase 5 accessibility
  sweep, never shipped), `lightningcss` + its platform-specific native
  binary (a transitive dependency of Vite's CSS pipeline — build-time
  only, never bundled into `web`'s shipped static output).

### `segmentio/asm` — tool detection gap, not a real license question (Go)

`go-licenses` reported `Unknown` for every sub-package of
`github.com/segmentio/asm` (a transitive dependency via the ClickHouse
Go driver). Its actual `LICENSE` file (read directly from the module
cache) is headed "MIT No Attribution" — SPDX `MIT-0`, a permissive MIT
variant that drops the attribution requirement. The auto-detector's
regex didn't recognize that non-standard heading text. Confirmed by
reading the file, not assumed; carried as an explicit CI ignore with
this citation (see the policy doc) rather than silently added to the
general allow-list.

### Named risk areas (task 3)

- **ClickHouse client libraries** (`github.com/ClickHouse/clickhouse-go/v2`,
  `github.com/ClickHouse/ch-go`): Apache-2.0, matching the server itself.
  No divergence.
- **Kubernetes Operator tooling** (`sigs.k8s.io/controller-runtime`,
  `k8s.io/client-go`, `k8s.io/apimachinery`): Apache-2.0 (one forked
  sub-package, `apimachinery/third_party/forked/golang`, is BSD-3-Clause
  — also fine). The concern about "generated boilerplate's license
  headers" turned out not to apply: `deploy/operator/api/v1alpha1/
  zz_generated.deepcopy.go` is, despite its name, **hand-written**, not
  actually produced by `controller-gen` (no kubebuilder/controller-gen
  binary was available when it was built — disclosed in the file's own
  doc comment and in `/deploy/README.md`). There's no real
  upstream-generated boilerplate to check for header drift against.
- **Phase 5 font/asset files**: see below.

## Redpanda — classification (c), recorded for a business decision

**The pinned Redpanda image (`docker.redpanda.com/redpandadata/redpanda:v24.2.7`,
`docker-compose.yml` and `transport/`) ships under BSL 1.1 (Business
Source License), confirmed against the actual `licenses/bsl.md` file at
that tag** (`github.com/redpanda-data/redpanda`, tag `v24.2.7`) — not
assumed from general familiarity with Redpanda's licensing history,
which the audit brief specifically warned has shifted over time.

Key facts, verified against primary sources:
- **Not OSI-approved open source.** BSL is explicitly source-available,
  matching this audit's own category-(c) definition.
- **Change Date**: 4 years from each version's release date, after which
  that version's `Licensed Work` converts to Apache-2.0. v24.2.7 was
  released 2024-10-11 (confirmed via the GitHub Releases API) — its
  Change Date is ~2028-10-11. As of this audit, it has **not** yet
  converted.
- **The restriction is narrow**: BSL's Additional Use Grant permits any
  use except offering the Licensed Work as a "Streaming or Queuing
  Service" to third parties (defined as a commercial offering letting
  third parties create topics in the Licensed Work, e.g. a hosted Kafka
  broker product). This project's `docker-compose.yml` uses only
  plaintext core Kafka-protocol functionality — no RCL-gated enterprise
  features, no tiered storage, no SASL/RBAC — squarely within the
  permitted grant as an internal transport layer.
- **No AGPL linking/compatibility issue.** Cairn OBS never links against
  Redpanda's code; it's consumed purely over the Kafka wire protocol, the
  same relationship as ClickHouse and Postgres. AGPLv3's copyleft
  doesn't reach across a network-protocol boundary to unrelated,
  separately-licensed software you merely talk to.
- **The genuinely open question**: Phase 6 relicenses `enterprise/` to
  AGPLv3 specifically so that anyone, including competitors, can legally
  self-host or fork Cairn OBS — including offering it as a network service,
  per AGPLv3's own terms. If a third party does that using the bundled
  `docker-compose.yml` (which pulls this BSL-licensed Redpanda image),
  does *their* deployment trip BSL's Streaming-or-Queuing-Service
  restriction? Cairn OBS's ingest pipeline creates fixed internal topics,
  not per-end-user topics exposed for direct third-party production or
  consumption — so this is very likely **not** a Streaming-or-Queuing-Service
  under BSL's own definition. But this is a business/redistribution
  judgment call about a hypothetical third party's use, not a pure
  technical compatibility question this audit can close unilaterally.

### Remediation options (recorded per task 4's requirement)

1. **Accept as-is.** Document the reasoning above; Cairn OBS's own use is
   clearly within BSL's permitted grant, and the third-party-SaaS
   scenario is a reasonable-but-unverified reading, not a known
   violation. Lowest effort, zero functional change.
2. **Swap to Apache Kafka** (Apache-2.0, genuinely OSI open source).
   `apache/kafka` (KRaft mode, no ZooKeeper needed as of Kafka 3.x) is
   wire-protocol-compatible with everything `transport`/`ingest`/`search`
   already speak. Real tradeoff: Redpanda was originally chosen partly
   for its lightweight single-binary footprint (`docker-compose.yml`
   runs it with `--smp=1 --memory=1G --overprovisioned`, tuned for a
   resource-constrained local/homelab deployment per `CLAUDE.md`'s
   stated deployment targets); Kafka's JVM-based broker has a materially
   larger minimum memory/startup footprint. This is a real regression
   for the project's stated "docker-compose for local/homelab" use case,
   not a drop-in swap with no cost.
3. **Stop bundling a pinned broker image at all.** `transport/` already
   has no application code of its own — it's a thin `docker-compose`
   wrapper and topic-provisioning script. Document Kafka-API
   compatibility as the requirement and let the operator supply their
   own broker (self-installed Apache Kafka, their own separately-licensed
   Redpanda, or anything else wire-compatible). This moves the
   redistribution question out of this project's own `docker-compose.yml`
   entirely, at the cost of a rougher out-of-the-box local dev experience
   (an extra manual setup step instead of `docker compose up`).

**Decision recorded 2026-08-16: option 1, accept as-is.** No code or
deployment change was made as a result — Redpanda stays pinned at
v24.2.7 in `docker-compose.yml`/`transport/`, under BSL 1.1, as a
disclosed and accepted risk rather than an unresolved one. This
decision should be revisited if the project's redistribution posture
changes materially (e.g. an official hosted/managed offering of Cairn
OBS itself, which would make the third-party-SaaS reading in this section
Cairn OBS's *own* situation rather than a hypothetical third party's).

## Non-license finding: `favicon.svg`

`web/src/lib/assets/favicon.svg` was SvelteKit's own default project
scaffold logo (`<title>svelte-logo</title>` — the `sv create`/`create-svelte`
starter icon), never replaced with an original mark during Phase 5's
redesign. Not a license-compatibility blocker — Svelte's own project
assets are MIT-licensed — but it was unauthored, third-party-branded
content shipping as this product's own favicon, caught by the same
"grep for anything that looks copied" pass this audit's task 1 asked
for. Recorded as an action item (replace with an original mark), not a
compliance blocker at the time; **resolved as part of the Sentry → Cairn
OBS rebrand**, which replaced it with the real Cairn OBS mark from the
project's own logo package.

## Own license declarations (task 5)

**Before this audit**: no root `LICENSE` file existed anywhere in the
repo — not at the root, not in `enterprise/`. The only license
declarations were prose statements in `CLAUDE.md`/`docs/architecture.md`
and correct `license = "AGPL-3.0-only"` fields in the two Rust
workspaces' `Cargo.toml`s. `web/package.json` had no `license` field at
all (npm's tooling reported the package itself as `UNLICENSED` as a
result). `web/static/fonts/LICENSE.txt` was a paraphrase describing the
Overpass font's license, not the actual OFL-1.1 license text.

**Fixed**:
- Added `/LICENSE` — the verbatim, unmodified AGPLv3 text from
  `gnu.org/licenses/agpl-3.0.txt`, byte-for-byte, not paraphrased.
- **Chosen convention, applied consistently**: one root `LICENSE` file
  governs the whole monorepo, plus a `license` field in every ecosystem
  manifest that supports one (`Cargo.toml`'s `license`/`license.workspace`,
  now confirmed correct; `package.json`'s `license`, added:
  `"AGPL-3.0-only"`). Go has no manifest-level license field — the
  standard convention (and what `go-licenses` itself looks for) is the
  root `LICENSE` file, which now exists. **Deliberately not** adopting
  per-file SPDX header comments across the monorepo's several thousand
  source files: headers are an FSF best-practice recommendation, not a
  legal requirement once a correct root `LICENSE` plus copyright
  ownership is established, and retrofitting them here would be a huge
  mechanical change for very little incremental legal value over what's
  now in place. Recorded as a deliberate choice, not left half-done.
- Corrected `web/static/fonts/LICENSE.txt` from a paraphrase to the
  actual, complete, unmodified OFL-1.1 text (fetched from
  `github.com/googlefonts/overpass`, the actual repository these font
  files were fetched from per the file's own prior note) plus the
  correct copyright statement, with the original context note (self-hosted
  vs. CDN) preserved as a clearly separated project note, not mixed into
  the license text itself.
- No accidental license mismatch from copied code was found — the
  repo-wide attribution-marker grep in the methodology section came back
  empty, and no vendored directories exist.

## `enterprise/` relicensing to AGPLv3 (task 6)

**This is a deliberate business-model choice, recorded plainly so it
isn't rediscovered as a surprise later**: the project is no longer
pursuing commercial-license revenue from the former `enterprise/`
features (SSO, multi-tenancy/RBAC, audit logging). As of Phase 6,
**anyone, including competitors, can legally self-host or fork those
features under AGPLv3's terms.** AGPLv3's source-sharing obligation
applies to network use (anyone interacting with a modified version over
a network is entitled to its source) — it does not impose any payment
obligation, and nothing in this project gates functionality behind a
license key or entitlement check.

**Confirmed no license-gating logic exists**: a repo-wide grep for
license-key/entitlement-check patterns (`license.?key`, `entitlement`,
`commercial.?key`, `paywall`, `IsLicensed`, and similar) across
`enterprise/`'s Go source returned zero hits. There was never a
functional paywall to remove — Phase 4's `enterprise/` split was always
an architectural/import-boundary separation, not a runtime license
check, so this task's "if any such gating exists, flag it explicitly, it
needs to come out" condition doesn't apply here — confirmed, not
assumed.

**Changes applied**:
- Every prose reference to `enterprise/` as "commercial license" or
  "commercial-licensed" across the repo was updated. Present-tense
  claims (code comments, `README.md` files describing current state,
  `CLAUDE.md`'s non-negotiable constraints) were corrected outright.
  Historical, phase-specific documents (`docs/phase-4-isolation-design.md`,
  `docs/phase-4-rbac-design.md`, `docs/phase-4-runbook.md`, and the
  relevant parts of `CLAUDE.md`'s and `docs/architecture.md`'s Phase 4
  sections) were given forward-pointing corrections — "commercial
  license at the time this was written; AGPLv3 as of Phase 6" — rather
  than rewritten as if the commercial-license period never happened,
  matching this project's existing convention for superseded claims
  (e.g. Phase 3's `tenant_id` gap, corrected inline by Phase 4's section
  rather than edited out of Phase 3's).
- `enterprise/README.md`, `enterprise/Dockerfile`,
  `enterprise/cmd/enterprise-auth/main.go`: relicensing statement
  corrected.
- `hack/check-tenant-boundary.sh` and every doc describing it
  (`docs/architecture.md`, `docs/security/threat-model.md`, the two
  `phase-4-*-design.md` docs): the import-boundary check **itself is
  kept** — it still enforces a real, valuable architectural property
  (core builds and deploys standalone with zero multi-tenant mechanism
  present, tenant identity resolution stays server-side) — but every
  description of *why* it exists was reframed from a licensing reason to
  an architectural one, since both sides now carry the same license.
- Final repo-wide grep for `commercial` confirms every remaining
  occurrence is one of the above corrections (explicitly framed as
  historical/superseded), not a live claim. **No file in the repo claims
  a license other than AGPLv3** for Cairn OBS's own code, as of this audit.

## Ongoing enforcement (task 7)

`.github/workflows/license-compliance.yml` — this repo's **first** CI
workflow file (several docs already said "enforced in CI" about
`hack/check-tenant-boundary.sh`, but no CI system had actually been wired
up yet; fixed as part of this task rather than left as a second gap next
to the one this task asked about). Four jobs, one per ecosystem plus the
architectural boundary check, all real commands verified locally against
this repo before being written into the workflow (not guessed):

- **Rust**: `cargo-deny-action` running `cargo deny check licenses`
  against `agent/deny.toml` and `search/deny.toml` — both verified
  passing locally with the policy's real allow-list.
- **Go**: `go-licenses check ./... --allowed_licenses=...` per module
  with real dependencies — verified passing locally for every listed
  module, including the `segmentio/asm` and HashiCorp-MPL-2.0 cases.
- **npm**: `license-checker --onlyAllow "..."` — verified passing
  locally.
- **Architectural boundary**: `hack/check-tenant-boundary.sh`, now
  actually wired into CI instead of only documented as if it were.

Full policy, including exactly what's auto-allowed, what needs manual
review, and what's rejected outright: `/docs/compliance/license-policy.md`.

**Not verified**: the workflow YAML itself has not been run through a
real GitHub Actions execution in this environment (no way to trigger
that here) — every individual command it invokes was verified locally
with real exit codes, but the workflow file's syntax and job wiring
should be confirmed on the first real PR that triggers it, the same
"written but not run against the live thing" caveat this project applies
to its other CI-adjacent and Docker-gated claims.

## What's resolved vs. what's still open

Per the audit brief's explicit gate: this phase is not "done" while a
(c) or unresolved-(b) item has no recorded resolution. As of the
Redpanda decision below, every item has one.

| Item | Status |
|---|---|
| All 774 permissive/MPL-2.0/OR-resolved dependencies | **Resolved** — classification (a), no action needed. |
| `segmentio/asm` (Go) | **Resolved** — confirmed MIT-0, carried as a cited CI ignore. |
| `favicon.svg` | **Flagged, not a compliance blocker** — action item recorded (replace with an original mark), not license-gating. |
| Redpanda (BSL 1.1) | **Resolved** — decision recorded 2026-08-16: accept as-is (option 1). No code change; the BSL exposure is a disclosed, accepted risk, not an unresolved one. |
| `enterprise/` relicensing | **Resolved** — applied throughout the repo, confirmed via repo-wide grep. |
| Root `LICENSE` / manifest declarations | **Resolved** — added and corrected. |
| CI enforcement | **Resolved** — workflow written and every command verified locally; the workflow file itself untested end-to-end (disclosed above). |

**Every task-2 (c)/unresolved-(b) item now has an explicit resolution**
— fixed, isolated, or, for Redpanda, flagged and decided. Phase 6's exit
criteria in `CLAUDE.md` are updated accordingly.

## Legal disclaimer (repeated, deliberately)

This audit and the policy derived from it are a strong first pass — real
primary sources were checked for every named risk area and every
flagged item, not guessed from memory. They are not a substitute for
review by actual legal counsel, which is recommended before the project
is publicly released, pitched to customers, or used as the basis for any
compliance claim — particularly the open Redpanda question above, which
is exactly the kind of judgment call outside counsel exists to make.
