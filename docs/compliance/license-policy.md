# License policy

**This is a policy document derived from a first-pass compliance audit,
not a legal opinion.** See `/docs/compliance/license-audit-report.md`
for the audit itself and its findings; this document is the resulting
ongoing policy, enforced in CI (`.github/workflows/license-compliance.yml`)
on every PR across all three dependency ecosystems (Rust, Go, npm) plus
the deployment-image surface reviewed manually at audit time.

The entire project, including `enterprise/`, is licensed AGPLv3 as of
Phase 6 — there is no separate commercial-license carve-out anywhere in
this repo. Every third-party dependency must be compatible with AGPLv3
as the single project-wide license.

## Auto-allowed (CI passes without review)

These license families are pre-cleared as dependencies of AGPLv3/GPLv3
code — permissive licenses impose no copyleft obligation at all, and
MPL-2.0's file-level copyleft doesn't extend to a larger work that
merely links/imports MPL-covered code (MPL 2.0 §3.3, "Distribution of a
Larger Work"):

- MIT, MIT-0 ("MIT No Attribution")
- Apache-2.0 (including `Apache-2.0 WITH LLVM-exception`)
- BSD-2-Clause, BSD-3-Clause, 0BSD
- ISC
- MPL-2.0
- Unlicense, Zlib, Unicode-3.0, BSL-1.0 (Boost — not to be confused with
  the *Business Source License*, also abbreviated BSL elsewhere in this
  document; Boost's BSL-1.0 is a permissive OSI-approved license with no
  relation to Redpanda's BSL 1.1)

CI enforcement:
- **Rust**: `cargo deny check licenses` against `agent/deny.toml` and
  `search/deny.toml`'s `[licenses.allow]` list.
- **Go**: `go-licenses check ./... --allowed_licenses=...` per module
  with real dependencies (see the workflow's matrix for the full list).
- **npm**: `license-checker --onlyAllow "..."` against `web`'s
  dependency tree.

A dependency whose *only* license is outside this list fails CI. A
dependency offering one of these licenses as *one option* in an SPDX OR
expression (e.g. `MIT OR Apache-2.0 OR LGPL-2.1-or-later`) passes,
because we elect the permissive branch — this is a normal, standard
reading of a disjunctive license grant, not a loophole.

## Requires manual review (category b)

Anything not on the auto-allowed list and not obviously incompatible
needs a human to actually read the license and record reasoning here or
in the audit report before merging — not a guess, and not a silent
`--ignore`/allow-list addition. This includes:

- Other copyleft licenses not listed above: LGPL (any version), EPL,
  CDDL, and similar. The specific question that matters is usually
  *how* the code is consumed — a dynamically-linked/networked LGPL
  dependency is generally fine; statically linking LGPL code into an
  AGPL binary is murkier for some LGPL versions and needs a real
  per-case read, not a blanket rule.
- Dual/multi-licensed packages where *none* of the offered licenses is
  on the auto-allowed list.
- Anything with a custom license file rather than a standard SPDX
  identifier, unless it's been manually confirmed (as
  `github.com/segmentio/asm`'s "MIT No Attribution" text was at audit
  time — SPDX `MIT-0`, added to the CI ignore list with that
  citation, not silently allowed) — a new custom-licensed dependency
  should not get the same free pass without its own confirmation.
- Docker/container base images pulled into `docker-compose.yml` or
  `/deploy` — not covered by any of the three CI dependency scans above,
  since they're not a language-ecosystem dependency. Reviewed manually
  at audit time (Redpanda, ClickHouse, Postgres); a new base image needs
  the same manual check, not an assumption that "it's just
  infrastructure."

## Rejected (category c)

Not usable as a dependency of this project without an explicit,
recorded exception:

- Source-available licenses that aren't OSI-approved open source: BSL
  (Business Source License), SSPL (Server Side Public License), Commons
  Clause, and similar "free to use except..." terms.
- Any license with a field-of-use restriction or a "non-compete" clause
  (e.g. "may not be used to offer a competing hosted service").
- "Free for non-commercial use" or similarly non-open terms.

**Known, accepted exception**: Redpanda (the
`docker.redpanda.com/redpandadata/redpanda` image pinned in
`docker-compose.yml`/`transport/`) ships under BSL 1.1 as of the pinned
version (v24.2.7), confirmed against the actual license file at that
tag, not assumed. This is consumed only as an external networked
Kafka-protocol broker — never linked into any AGPLv3 binary — so it
doesn't create an AGPL compatibility problem in the traditional linking
sense, and BSL's specific restriction (no reselling direct broker access
as a hosted streaming/queuing service) doesn't obviously apply to how
this project uses it. **Decision recorded 2026-08-16: accept as-is** —
see the audit report's Redpanda section for the full reasoning, the
other two remediation options that were considered and not chosen, and
the condition under which this decision should be revisited (an
official hosted/managed Cairn OBS offering). A future change to Redpanda's
license, or to this project's own redistribution posture, should trigger
re-review, not silently ride on this entry.

## What CI does not cover

The automated checks above only see what a package manager sees. They
do not catch:
- Vendored/copied code not declared as a dependency (checked manually
  at audit time via a repo-wide grep for copy/attribution markers — see
  the audit report's methodology section; not re-run automatically).
- Font files, icon packs, or other design assets (also checked manually
  at audit time).
- Docker base images (see above).

A new instance of any of these needs the same manual treatment the
original audit gave — this policy doesn't claim CI makes the project
audit-proof going forward, only that *dependency-manifest* drift is
caught automatically.

## Legal disclaimer

This policy, and the audit it's derived from, is a strong first pass —
not a legal opinion. It should be reviewed by actual legal counsel
before the project is publicly released, pitched to customers, or used
as the basis for any compliance claim.
