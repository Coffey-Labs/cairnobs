# Phase 7 runbook

Extends `/docs/phase-0-runbook.md` through `/docs/phase-5-runbook.md`
(Phase 6 had no runbook of its own — a compliance audit, not a running
system). Read those first. Phase 7 adds one new component category (an
AI model provider) and touches `api`, `enterprise/`, `web`, and `cli` —
see `/docs/phase-7-ai-design.md` for the full design record; this
document is verification only.

## What's actually been verified

Every AI operation (`complete`, `explain`, `fix`, `optimize`,
`translate`, and the audit-logging endpoint behind it) has been run
end-to-end against a real `docker compose` stack — real HTTP requests
into the real `cairnobs-api` container, through the real
`api/ai/provider/ollama.Client`, over a real network call, into a real
process answering Ollama's actual `/api/chat` wire contract. **No real
model weights are used anywhere in this verification** — see
"Why a mock provider, not a real model" below for why that's a
deliberate, disclosed choice rather than a shortcut. Two real product
bugs were found and fixed via live browser verification of the frontend
half (`QueryEditor.svelte`'s ghost-text autocomplete) that neither
`svelte-check` nor `npm run build` caught — see
`/docs/phase-7-ai-design.md`'s Track A section for the full writeup;
this runbook doesn't repeat it.

Also verified in this pass, against the same live stack's real
Postgres: the Phase 4 `audit_log` table's `event_type` CHECK constraint
was extended with migration 0036, and both new
`enterprise/internal/audit` tests (`TestAIInteractionLoggerWritesAttributed
ToContextIdentity`, `TestAIInteractionLoggerRefusesWithoutIdentity`)
passed against it — a real row lands with `event_type='ai_interaction'`,
correctly attributed to the tenant/user identity in context, with
`detail` carrying the operation/confidence/accepted/edited fields as
JSON.

**Not verified, disclosed rather than silently skipped**: this
environment has no GPU and no downloaded model weights, so the actual
quality of `qwen2.5-coder:7b`'s output — whether it reliably produces
valid pipe syntax for realistic questions, how well-calibrated its
self-reported confidence is, whether Explain's prose actually reads as
useful — has never been checked here. See
`/docs/phase-7-ai-design.md`'s "Integration tests and CI testability"
section for why that's kept as a periodic human-run checklist item
rather than something this runbook or CI can cover.

## 1. Bring up the stack

```sh
docker compose up -d --build
cd web && npm run dev   # localhost:5183, talks to localhost:8080/8081 by default
```

No new required services — `docker compose ps` shows the same set as
Phase 5. AI routes are off by default: with no `OLLAMA_BASE_URL` set,
`api`/`enterprise-api` never register `/ai/*` at all (confirmed live in
this pass — `curl -X POST localhost:8080/ai/translate` returns a plain
`404`, not a 500 or a hang against an unreachable `localhost:11434`).

## 2. Enable AI routes against the committed mock provider

`hack/mock-ollama` (new this phase) answers Ollama's real `/api/chat`
wire contract with fixed, deterministic canned responses picked by
inspecting the system prompt's opening line — enough to exercise every
real code path (`ollama.Client`'s HTTP call, JSON parsing,
`planner.Compile`, `costguard.Assess`, the HTTP response shape) without
needing model weights, a GPU, or non-deterministic output. This is the
same technique `api/ai/aiapi/integration_test.go` uses in Go directly;
this tool is for manual/browser verification, where an in-process fake
isn't an option.

Run it as a container on the compose network with a network alias of
`ollama` (so `api`'s container can resolve the hostname), then point
`OLLAMA_BASE_URL` at it via a throwaway compose override:

```sh
docker run -d --rm --name cairnobs-mock-ollama --network sentry_default --network-alias ollama \
  -v "$(pwd)/hack/mock-ollama:/src" -w /src golang:1.25-alpine \
  sh -c "go build -o /tmp/mock-ollama . && /tmp/mock-ollama"

cat > /tmp/docker-compose.ai-verify.yml <<'EOF'
services:
  api:
    environment:
      OLLAMA_BASE_URL: "http://ollama:11434"
      OLLAMA_MODEL: "test-model"
EOF

docker compose -f docker-compose.yml -f /tmp/docker-compose.ai-verify.yml up -d api
```

For `enterprise-api` instead of core `api` (needed to also exercise
task 12's real audit-log write, since core has no `InteractionLogger`
wired in), override that service's environment instead, same shape.

Verify the routes are live:

```sh
curl -s -X POST localhost:8080/ai/translate -H 'Content-Type: application/json' \
  -d '{"nlQuery":"errors in the last hour"}'
# {"query":"earliest=-1h severity=ERROR","confidence":"high","compiles":true,"blocked":false}
```

**Clean up afterward** — don't leave the mock provider or the override
wired into a stack anyone else might reach:

```sh
docker compose up -d api   # drops back to the plain env, no -f override
docker rm -f cairnobs-mock-ollama
rm /tmp/docker-compose.ai-verify.yml
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/ai/translate -d '{}'
# 404 -- confirms AI routes are unregistered again
```

## 3. Track A — Explain / Fix / Optimize / ghost-text

With AI routes enabled (step 2) and the web dev server running against
`localhost:8080`, open the Search page's query bar:

- Type a partial query and pause — ghost text should appear inline
  after ~300ms; Tab accepts it. Stop `cairnobs-mock-ollama` and confirm
  ghost text just silently stops appearing (no error toast, no
  console noise) — this is the "graceful degradation" requirement,
  not incidental behavior.
- Run a query that produces a parse or execution error, click "Try AI
  fix" — the diff view should show the current vs. suggested query, and
  Accept should replace the query bar's content without running it.
- Run `severity=ERROR | stats count by host` (an unbounded aggregation
  against the real seeded ClickHouse data from
  `hack/benchmark-fixture`, per Phase 2/5's runbooks) — the inline cost
  warning should appear, and clicking "Optimize" should show the real
  mechanical rewrite (`earliest=-1h ` prepended).
- Click "Explain this query" on any query — confirm the modal shows
  prose, not raw JSON (a genuine model would return prose here too;
  the mock's canned Explain response is deliberately plain text for
  exactly this reason).

## 4. Track B — natural-language translation

Type a natural-language-shaped question into the query bar (4+ words,
no `|`/comparison operator/`:`) — e.g. "show me errors from the last
hour grouped by service". The "Interpret as natural language" affordance
should appear; clicking it opens the translate modal, auto-translates,
and shows both the generated query (editable) and an auto-fetched
explanation. Confirm "Use this query" replaces the query bar content
**without running anything** — no results table should appear until you
separately click "Run query".

CLI:

```sh
cd cli && go run ./cmd/cairnobsctl query --nl "errors in the last hour" --api http://localhost:8080
# prints the translated query and, in an interactive terminal, prompts y/N before running
```

## 5. Audit logging (task 12)

Requires `enterprise-api` (not core `api`) — core has no
`InteractionLogger` wired in by design (see the design doc's "off unless
configured" reasoning). With `enterprise-api` running against the
compose profile that includes it and AI routes enabled per step 2's
pattern applied to that service instead:

1. Accept or dismiss a Fix/Optimize/Translate suggestion in the web UI.
2. Confirm a row landed in `audit_log`:
   ```sh
   docker exec cairnobs-metadata-postgres psql -U cairnobs -d cairnobs_metadata \
     -c "SELECT event_type, query_text, detail FROM audit_log WHERE event_type='ai_interaction' ORDER BY id DESC LIMIT 5;"
   ```
   `detail` should show `operation`/`accepted`/`edited` matching what you
   just did in the UI.

This exact path (minus the browser click, using the adapter directly)
is what `enterprise/internal/audit/integration_test.go`'s
`TestAIInteractionLoggerWritesAttributedToContextIdentity` already
proves automatically — see "Running the automated suite" below to run
it yourself instead of clicking through the UI.

## 6. Running the automated suite

```sh
# api module -- includes the new mock-Ollama-backed integration tests
# (api/ai/aiapi/integration_test.go), no live infra needed
cd api && go build ./... && go vet ./... && go test ./...

# enterprise module -- same, plus the live-Postgres audit tests (skipped
# automatically unless AUDIT_TEST_POSTGRES_ADDR is set)
cd enterprise && go build ./... && go vet ./... && go test ./...

# live-Postgres audit tests specifically, against the real dev stack:
docker run --rm --network sentry_default -v "$(pwd):/src" -w /src/enterprise \
  -e AUDIT_TEST_POSTGRES_ADDR=metadata-postgres:5432 \
  -e AUDIT_TEST_POSTGRES_PASSWORD=audit-writer-dev-only \
  -e AUDIT_TEST_ADMIN_PASSWORD=cairnobs-dev-only \
  golang:1.25-alpine go test ./internal/audit/... -v

# cli module
cd cli && go build ./... && go vet ./... && go test ./...

# web
cd web && npm run check && npm run build
```

All of the above pass in this environment as of this runbook. The first
three don't need Docker or a live database at all except where noted —
that's deliberate, see the design doc's CI-testability section.

## Why a mock provider, not a real model

Testing against a real Ollama server running the actual pinned
`qwen2.5-coder:7b` needs a multi-gigabyte model download and either a
GPU or a slow CPU-bound wait per request — infeasible for both this
environment and, more importantly, for CI, and non-deterministic enough
even at temperature 0 that a failing test wouldn't reliably mean a real
regression. `hack/mock-ollama` and `api/ai/aiapi/integration_test.go`'s
in-process equivalent both trade away model-quality coverage for
plumbing coverage that's actually fast and deterministic enough to run
every time — the same tradeoff this project already made for
ClickHouse/Postgres-backed pieces of Phase 4 that only "compile and are
unit-tested" in environments without live infrastructure. Model-quality
verification (does the real model produce good translations for real
questions) is real, disclosed future work — a periodic, human-run
checklist item against a real local Ollama with the pinned model before
a release, not a CI gate.
