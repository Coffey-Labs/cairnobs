# sentry-agent

Distro-agnostic Linux/Windows log collector. On Linux, statically linked
against musl, no glibc runtime dependency. Tails journald (Linux default),
a file, Windows Event Log, or ETW, batches lines, and ships them over mTLS
gRPC to the ingest service.

**Windows support status:** the Windows-specific code
(`source/windows_eventlog.rs`, `source/etw.rs`, `service.rs`) was written
against documented Win32/ETW API shapes but has **not been compiled or run
on Windows** — no Windows toolchain was available in the environment this
was built in (confirmed: only the Linux target's std library was
installed, no way to even `cargo check --target x86_64-pc-windows-*`).
Linux builds/tests/clippy are verified clean across every feature
combination; Windows code is a first draft to compile-check and test for
real before trusting it. See `/docs/phase-1-runbook.md`.

## Workspace layout

- `sentry-parser` — pure-`std` RFC 5424 syslog parser with raw-passthrough
  fallback. No I/O, easy to unit test in isolation.
- `sentry-agent` — the binary: config loading, sourcing (journald/file/
  Windows Event Log/ETW), batching, mTLS gRPC client, Windows service
  wrapper.

## Why one crate for both platforms, not a platform split

`config.rs`, `batch.rs`, `grpc.rs`, and `main.rs`'s event loop are already
100% cross-platform Rust — nothing in them is Linux- or Windows-specific.
Only the `source/` modules differ per platform, and that boundary already
existed before Windows support was added (it's exactly what made adding
Windows sources a matter of adding two files, not restructuring anything).
Windows-only dependencies (`windows`, `windows-service`, `quick-xml`) live
in a `[target.'cfg(windows)'.dependencies]` section in `Cargo.toml`, so
they're not in the Linux build's dependency graph at all — no crate split
needed to keep the two platforms from stepping on each other.

## Why journalctl, not libsystemd

The journald source shells out to `journalctl -f -o json` rather than
linking `libsystemd` via FFI. Statically linking libsystemd into a musl
binary is fragile — it pulls in dbus/libcap transitively and isn't designed
for static linking — and would undermine the no-glibc-runtime-deps goal
even where technically possible. `journalctl` ships on every systemd distro
this agent targets, so shelling out sidesteps the problem entirely. See
`/docs/architecture.md`.

## Building

Native build (whatever target your machine is):

```sh
cargo build --release
```

musl targets (what actually ships):

```sh
rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl

# x86_64: works with musl-gcc installed locally (musl-tools on Debian,
# musl on Arch, etc.) — the musl target is fully static by default.
cargo build --release --target x86_64-unknown-linux-musl

# aarch64 cross-compilation needs a cross toolchain; the boring, reliable
# option is `cross` (https://github.com/cross-rs/cross), which builds
# inside a Docker container with the right linker preinstalled:
cross build --release --target aarch64-unknown-linux-musl
```

Building requires `protoc` on PATH (used by `tonic-build`/`prost-build` at
compile time to generate the gRPC client from `/proto/sentry/logs/v1/logs.proto`).

Container build (see caveat below):

```sh
# from the repo root, not agent/
docker build -f agent/Dockerfile -t sentry-agent .
```

**Caveat:** the container image is provided for CI/completeness, but
journald sourcing needs `journalctl` and access to the host journal —
neither of which exist in the `scratch` image or are available to a
container without deliberately bind-mounting `/var/log/journal` (or
`/run/log/journal`) and the `journalctl` binary in. The intended Phase 0
deployment for journald sourcing is as a native binary managed by systemd
on the host, not containerized.

### Building for Windows

```sh
# Cross-compiling FROM Linux, for the build step only:
rustup target add x86_64-pc-windows-gnu
cargo build --release --target x86_64-pc-windows-gnu \
    --no-default-features --features windows-eventlog,etw

# Natively on Windows (MSVC toolchain):
cargo build --release --target x86_64-pc-windows-msvc \
    --no-default-features --features windows-eventlog,etw
```

`--no-default-features` matters: the default feature set is `journald`,
which is Linux-only (the module is `target_os = "linux"`-gated and simply
won't compile in on Windows, but there's no reason to carry the dead
feature flag). Drop `,etw` from `--features` if you only want Event Log —
see the privilege note below for why most environments will want to.

**Cross-compilation only covers the *build* step.** Running/testing the
Windows sources — actually calling `EvtSubscribe`, starting an ETW
session, registering a Windows service — needs a real or virtualized
Windows host. There is no way around that, and nothing in this repo
pretends otherwise; see `/docs/phase-1-runbook.md` for exactly what's
automatable vs. manual-only.

## Running

No CLI flags are required for the common case:

```sh
./sentry-agent
```

This uses the platform's conventional config path if present
(`/etc/sentry-agent/agent.toml` on Linux, `C:\ProgramData\SentryAgent\agent.toml`
on Windows), otherwise built-in defaults: journald source on Linux (whole
journal, no unit filter), service name `default`, and mTLS material
expected under the same conventional directory
(`{ca,client,client-key}.pem`). mTLS is mandatory per the project's
transport requirements, so a from-scratch run with no certs in place will
fail fast with a clear error rather than connecting insecurely.

See `config/agent.example.toml` for all fields.

```sh
./sentry-agent --config /path/to/agent.toml
```

## Running as a Windows service

"A native Windows service, not a WSL wrapper" means implementing the Win32
Service Control Manager protocol, not just running the binary in a
console — that's what `service.rs` (via the `windows-service` crate)
does. From an administrator shell:

```powershell
sentry-agent.exe install     # registers the service, Automatic start, LocalSystem account
sc.exe start SentryAgent
sc.exe stop SentryAgent
sentry-agent.exe uninstall
```

`install`/`uninstall`/`run-service` are subcommands only present in
Windows builds (`sentry-agent` with no subcommand is still the normal
foreground/console run, same as on Linux) — `run-service` specifically is
what the SCM itself invokes at service start; don't run it directly.

**Known limitation:** when running as a service, there's no console
attached, so `tracing_subscriber::fmt()`'s stdout writer has nowhere to
go — logs won't be visible anywhere useful until this is redirected to a
file or a proper Windows Event Log tracing sink is written. Not addressed
in Phase 1; flagging it here rather than shipping it silently broken.

## ETW: read this before enabling it

ETW needs elevated privileges to subscribe to most providers — running
the agent under an administrator token or a service account with
`SeSystemProfilePrivilege`/ETW-specific rights. This is a real privilege
escalation, not a footnote: think about whether your environment wants
the log-shipping agent running with that level of access before turning
on the `etw` feature and an `[source] kind = "etw"` config. Event Log
alone (no elevated privileges needed) covers the common case and is what
Phase 1's exit criteria in `/CLAUDE.md` actually requires to be running.

Providers are configured by **GUID**, not friendly name — ETW's own API
requires it. Look one up with `logman query providers "<Friendly Name>"`.

## Windows Event Forwarding (WEF)

Two different things people mean by "WEF support," worth being explicit
about since they're very different amounts of work:

1. **What this repo supports today, with zero extra code:** WEF is a
   native Windows-to-Windows mechanism (`wecsvc`, the built-in Windows
   Event Collector role) — endpoints forward to a Windows Server acting
   as collector using Windows' own mechanism, no Sentry code involved in
   the forwarding itself. Run this agent *on the collector box*,
   subscribed to the `ForwardedEvents` channel instead of the usual three:
   ```toml
   [source]
   kind = "eventlog"
   channels = ["ForwardedEvents"]
   ```
2. **What this repo does *not* implement:** a true agentless receiver —
   Sentry itself speaking the WS-Management/WinRM event-subscription
   protocol so endpoints can forward directly to `ingest` without any
   Windows Event Collector role or Sentry agent anywhere. That's a
   standalone protocol implementation (SOAP-ish subscription/heartbeat/
   delivery over WinRM), not an agent or ingest-side tweak, and it's out
   of scope for Phase 1. If you need this, it's a real project of its
   own — say so before assuming it's a small addition.

## Testing

```sh
cargo test --workspace
```

## Feature flags

- `journald` (default) — journalctl-based journald source. `target_os =
  "linux"`-gated: enabling this on a Windows build is a no-op, not a
  build failure.
- `file-tail` — polling-based file tailer (no inotify dependency; doesn't
  follow rename-based log rotation yet). Cross-platform, works on Windows
  too.
- `windows-eventlog` — Windows Event Log via `EvtSubscribe`.
  `target_os = "windows"`-gated the same way; a no-op on Linux.
- `etw` — ETW real-time session. Same gating. See the privilege section
  above before enabling.

Any combination can be enabled together; `[source].kind` in config picks
which one actually runs. Building without a feature and configuring that
source at runtime fails at startup with a clear error rather than
silently doing nothing.

Dependencies added for Windows support, worth knowing about:
`windows` (Microsoft's official Win32/ETW bindings), `windows-service`
(Windows Service Control Manager wrapper), `quick-xml` (parses
EvtSubscribe's rendered event XML). All three are `[target.'cfg(windows)'.dependencies]`
— not in the Linux build's dependency graph at all.
