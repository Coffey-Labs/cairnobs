# Contributing to Cairn OBS

Thanks for your interest in contributing to **Cairn OBS** — open-core, Kubernetes-native log aggregation and observability. Contributions of all kinds are welcome: bug reports, feature requests, code, documentation, and testing.

## Code of Conduct

By participating in this project you agree to abide by the [Code of Conduct](CODE_OF_CONDUCT.md). Be constructive, be patient with newcomers, and keep discussion focused on the project.

## Before You Start

- **Read [`docs/architecture.md`](docs/architecture.md) before changing any component.** The storage/query split in particular is deliberate: ClickHouse serves analytics, Tantivy serves full-text, and one query language compiles to a single plan across both. Changes that blur that line need discussion first.
- **PostgreSQL is control-plane only** — dashboards, panels, alert rules and state, notification targets, delivery log. Log data never touches it. That boundary is about transactional read-modify-write, not preference.
- **`enterprise/` is a separate Go module that core never imports from.** Since the Phase 6 relicensing that boundary is architectural rather than legal — it keeps core buildable and deployable standalone, and keeps tenant resolution server-side. Please don't reach across it.
- **The whole repository is AGPLv3, `enterprise/` included.** There is no feature gate and no paid tier holding code back. Anything you contribute is distributed under AGPL-3.0, including for hosted/SaaS deployments.

## How to Contribute

### Reporting Bugs

Search [existing issues](https://github.com/Coffey-Labs/cairnobs/issues) first. When filing a bug, include:

- A clear, descriptive title
- Steps to reproduce, and expected vs. actual behaviour
- **Which component** — `agent`, `ingest`, `api`, `search`, `alerting`, `enterprise`, `web`, `cli`, `deploy`
- How you're running it: `docker compose`, the Helm chart, or something else, and which `COMPOSE_PROFILES` if compose
- Relevant logs from the component, and the query if the bug is in query behaviour
- Whether it reproduces from a clean `docker compose down -v && docker compose up`

Two areas are known to be unverified rather than broken — Phase 4 (RBAC and tenant isolation) beyond its audit-logging guarantees, and the Windows agent, which has never run on real Windows. Both are called out in the README. Reports against them are welcome; please say that's what you were testing.

### Suggesting Features

Open an issue describing:

- The problem you're trying to solve, not just the solution
- Where it sits relative to the architecture — which component owns it, and whether it crosses the storage/query split or the `enterprise/` boundary
- Roughly what it costs at volume, if it touches the ingest or query path. Cost-per-GB is a design goal, not an afterthought.

For larger changes, open an issue to discuss the approach **before** submitting a pull request — this saves everyone time if the direction needs adjusting.

### Submitting Pull Requests

1. **Fork** the repository and create your branch from `main`.
2. **Name your branch** descriptively, e.g. `fix/tantivy-merge-stall` or `feat/alert-absence-rules`.
3. **Keep PRs focused** — one logical change per PR.
4. **Write clear commit messages** describing what changed and why. The "why" matters more than the "what"; the diff already says what.
5. **Add or update tests** in the component you touched. Each top-level directory carries its own unit tests.
6. **Update documentation** if your change affects setup, configuration, the query language, or user-facing behaviour.
7. **Open the pull request** against `main`, with a summary, any related issue numbers, screenshots for UI changes, and what you actually ran to check it.

Please sign off your commits: `git commit -s` adds the `Signed-off-by` trailer, which is a statement that you have the right to submit the work under this project's licence. GitHub enforces it for commits made through the web interface; for everything else it is asked for rather than blocked.

### CI

Three workflows run on a pull request:

- **License compliance** — the dependency licence inventory under `docs/compliance/` must stay accurate. A new dependency with an incompatible licence fails the build.
- **Security scan**
- **Web route check** — the route lists are kept honest against the actual SvelteKit routes, so an unrouted path answers 404 rather than rendering something.

### Code Style

- Match the existing formatting and naming conventions in the component you're editing. It's a polyglot tree: Rust in `agent` and `search`, Go in `ingest`, `api`, `alerting`, `enterprise` and `cli`, TypeScript and Svelte in `web`.
- `cargo fmt` and `gofmt` output is the standard; don't hand-format around them.
- Prefer clarity over cleverness. This is infrastructure people page on.
- Comment the non-obvious, especially anything about query planning, tenant scoping, or the exactly-once/at-least-once properties of the ingest path — those are easy to get subtly wrong and expensive to debug later.

### Development Setup

1. Clone your fork:
   ```bash
   git clone https://github.com/YOUR-USERNAME/cairnobs.git
   cd cairnobs
   ```
2. Bring the stack up:
   ```bash
   docker compose up
   ```
   The web UI is on <http://localhost:3000> and the API on `:8080`; alerting is on `:8081` and enterprise auth on `:8082`. The agent connects to ingest over mTLS gRPC on `:4317`. `search` publishes no host port — it's reachable only on the compose network.
3. `COMPOSE_PROFILES` in `.env` selects the query-serving binary: `single-tenant` (default) or `enterprise` for the multi-tenant path. They're mutually exclusive, the same choice Helm's `enterprise.enabled` flag makes for a real cluster. Override per invocation with `COMPOSE_PROFILES=enterprise docker compose up`.
4. Exercise the path you changed end to end — for ingest or query work that means getting a real log line in and querying it back, not just a passing unit test.

## Review Process

- A maintainer will review your PR and may request changes.
- Please respond to review feedback in a timely manner; PRs with no activity for an extended period may be closed, and can be reopened once updated.
- Once approved, a maintainer will merge the PR.

## Reporting Security Issues

Please **do not** open a public issue for security vulnerabilities. See [SECURITY.md](SECURITY.md) for how to report privately, and for what is in and out of scope — cross-tenant data exposure is the class we most want to hear about.

## Questions?

If you're unsure whether something is a good fit, open an issue and ask, or start a [discussion](https://github.com/Coffey-Labs/cairnobs/discussions). Discussion before you invest time in a PR is welcome.

Thanks again for helping improve Cairn OBS.
