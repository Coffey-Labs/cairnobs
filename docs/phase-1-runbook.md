# Phase 1 runbook

Extends `/docs/phase-0-runbook.md` with Windows log collection and
full-text search. Read that one first — this assumes the Phase 0 stack
(dev certs, `docker compose up`, backend sanity check) already works;
Phase 1 layers on top of it, doesn't replace it.

## What's actually been verified vs. what needs real Windows

Unlike Phase 0's original draft, most of this runbook reflects steps
actually run in this session against a live stack, not just planned:

- **Verified for real:** the full Linux pipeline through search (agent →
  ingest's `record_id` assignment → Redpanda → both consumers →
  ClickHouse *and* Tantivy → both `/query` and `/search` → the same
  `record_id` back from both). The `windows-fixture` generator sending
  Windows-*shaped* data through the same pipeline and being correctly
  queryable both ways, including `winevt.*` attributes and severity
  mapping.
- **Not verified, and can't be from the environment this was built in:**
  the actual Windows agent binary — `EvtSubscribe`, ETW session creation,
  Windows service registration. No Windows toolchain was available
  anywhere (confirmed: only the Linux target's std library installed, no
  rustup, no way to even `cargo check --target x86_64-pc-windows-*`).
  Part C below is the logical sequence to run on a real or virtualized
  Windows host, not a report that it's been run.

## Prerequisites (beyond Phase 0's)

- A Windows host or VM (Windows 10/11 or Windows Server) for Part C.
- `mingw-w64` if cross-compiling the Windows build from Linux (optional —
  building natively on Windows with `rustup target add
  x86_64-pc-windows-msvc` works too and needs no extra setup on the Linux
  side).
- Administrator access on the Windows host, for service registration and
  (if you enable it) ETW.

## Part A: full-text search (Linux-only, no Windows needed)

Only needs what Phase 0's runbook already set up.

### A1. Bring the stack up (if not already)

```sh
docker compose up -d --build
```

Same as Phase 0, now also builds and starts `search` (Tantivy full-text
indexing). Confirm it's actually logging — same `RUST_LOG` gap the agent
has by default:

```sh
docker compose logs search
```

You should see "search gRPC server listening" and rskafka connecting to
all of `cairnobs.logs.raw`'s partitions. If you see nothing at all, check
`RUST_LOG=info` is set on the `search` service in `docker-compose.yml`.

### A2. Generate a log line and confirm both query paths agree

Follow Phase 0's runbook to get the agent running and generate a test
line (steps 4–6 there — mTLS certs, build, run, `logger`). Then, instead
of just checking `/query`, check both:

```sh
# The SQL path (ClickHouse).
curl -X POST http://localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query": "SELECT record_id, message FROM logs ORDER BY timestamp DESC LIMIT 1"}'

# The full-text path (Tantivy). A bare word is a free-text search -- see
# /docs/query-language-reference.md. Both go to /query: Phase 2 unified
# the two languages behind one endpoint, and the separate POST /search
# this step used to call no longer exists.
curl -X POST http://localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query": "<a distinctive word from your test log line>"}'
```

**The `record_id` in both responses should match.** That's the actual
Phase 1 exit criterion (`/PROJECT-SPEC.md`) for the Linux half: the same
record, reachable both ways. If `/search` returns nothing yet, give it a
few more seconds — Tantivy commits on a timer (`COMMIT_INTERVAL_MS`,
default 2s), so there's a small window where a record is in ClickHouse
but not yet searchable.

### A3. Confirm from the web UI

Open `http://localhost:3000` — there are now two pages, linked via the
top nav: **SQL Query** (unchanged from Phase 0) and **Full-Text Search**
(new). Run the same free-text term on the search page and confirm you
see the row.

## Part B: Windows-shaped data without a Windows host

Still no Windows needed — this tests the pipeline's handling of
Windows-*shaped* data, not the real Windows integration (see
`/hack/windows-fixture/README.md` for the exact distinction).

```sh
cd hack/windows-fixture
go run . --count 5
```

Then repeat A2's pattern: query for one of the synthetic events by
`attributes['winevt.event_id']` via `/query`, and by a distinctive word
from its message via `/search`. Both should return it, and `attributes`
should carry `winevt.event_id`/`winevt.provider`/`winevt.channel`/
`winevt.computer`.

## Part C: the real Windows agent (needs actual Windows)

### C1. Build

On the Windows host itself (simplest — avoids cross-compilation
entirely):

```powershell
rustup target add x86_64-pc-windows-msvc
cd agent
cargo build --release --target x86_64-pc-windows-msvc --no-default-features --features windows-eventlog,etw
```

Or cross-compile from Linux, then copy the binary over:

```sh
rustup target add x86_64-pc-windows-gnu
cargo build --release --target x86_64-pc-windows-gnu --no-default-features --features windows-eventlog,etw
```

`protoc` needs to be on `PATH` either way (used by `tonic-build` at
compile time), same requirement as the Linux build.

### C2. Get mTLS certs onto the Windows host

Copy `hack/dev-certs/out/{ca,client,client-key}.pem` from wherever you
ran `generate.sh` to `C:\ProgramData\CairnObsAgent\` on the Windows host
(create the directory first). Same dev-only certs Phase 0's Linux agent
uses — the CA doesn't care what platform the client is on, only that the
client cert was signed by it.

### C3. Config

Create `C:\ProgramData\CairnObsAgent\agent.toml`:

```toml
[source]
kind = "eventlog"
channels = ["Application", "System", "Security"]

[ingest]
endpoint = "https://<host-running-docker-compose>:4317"
```

If Docker Compose runs on a different machine than the Windows host,
`ingest`'s server cert SAN needs to cover that hostname/IP too — see
`hack/dev-certs/generate.sh` and regenerate with an updated SAN if
needed (same note as Phase 0's runbook's troubleshooting section).

### C4. Run it directly first, before installing as a service

```powershell
$env:RUST_LOG="info"
.\cairnobs-agent.exe --config C:\ProgramData\CairnObsAgent\agent.toml
```

Confirms the Event Log source and mTLS connection work before adding the
Windows service layer on top — if something's wrong, it's much easier to
diagnose here than after wrapping it in a service.

### C5. Generate a Windows Event Log entry and confirm it flows through

From another PowerShell window (or Event Viewer):

```powershell
eventcreate /T INFORMATION /ID 1 /L APPLICATION /SO "CairnObsTest" /D "phase1 windows verification line"
```

Then check both query paths, same pattern as A2.

### C6. Install as a service

```powershell
.\cairnobs-agent.exe install
sc.exe start CairnObsAgent
```

Verify it's running (`sc.exe query CairnObsAgent`) and generate another
test event to confirm it's still flowing through while running as a
service, not just in the foreground. **Known gap:** no console under the
SCM means `tracing`'s log output currently has nowhere to go — see
`/agent/README.md`'s "Running as a Windows service" section. If
something goes wrong here, you're debugging blind until that's
addressed; C4's foreground run is where to diagnose real problems.

```powershell
sc.exe stop CairnObsAgent
.\cairnobs-agent.exe uninstall
```

### C7 (optional). ETW

Only if you actually want it running — **read the privilege section in
`/agent/README.md` first.** ETW needs elevated privileges (an
administrator token or `SeSystemProfilePrivilege`), a real consideration
for a log-shipping agent, not a formality. Providers are configured by
GUID (`logman query providers "<Name>"` to look one up):

```toml
[source]
kind = "etw"
providers = ["{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}"]
```

### C8 (informational). WEF

No new steps — see `/agent/README.md`'s WEF section. The supported
pattern is running this same agent (Event Log source) on a Windows
Server already acting as a native Windows Event Collector, pointed at
the `ForwardedEvents` channel instead of the usual three. A true
agentless WS-Management receiver is explicitly not built in Phase 1.

## Troubleshooting (Phase 1-specific)

**`/search` returns nothing but `/query` finds the record.**
Check `COMMIT_INTERVAL_MS` hasn't elapsed yet (default 2s) — Tantivy
batches commits, same reasoning as ClickHouse batching inserts. If it's
been well over that and still nothing: `docker compose logs search` —
look for "skipping record with empty record_id" (would mean something
upstream isn't assigning IDs — shouldn't happen) or connection errors to
Redpanda.

**Windows agent connects but Event Log entries never show up.**
Check the channel name is exactly right (`Application`/`System`/
`Security`, case matters to the Windows API) and that the account
running the agent has read access to that log — `Security` specifically
often needs elevated rights beyond what `Application`/`System` need.

**Windows build fails to find `protoc`.**
Same requirement as the Linux build — install `protoc` and ensure it's
on `PATH` before `cargo build`. On Windows, the official protoc release
zip plus adding its `bin/` to `PATH` is the simplest route.

**Nothing in this section covers the problem.**
Genuinely possible — this is the least-tested part of the whole Phase 1
build (see the caveat at the top). Check `/agent/README.md`'s Windows
sections for the specific module involved (`source/windows_eventlog.rs`,
`source/etw.rs`, `service.rs`) and their own "UNVERIFIED" comments for
what's most likely to need a real fix.
