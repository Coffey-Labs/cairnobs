# sentry-agent

Distro-agnostic Linux log collector. Statically linked against musl, no
glibc runtime dependency. Tails journald (default) or a file, batches
lines, and ships them over mTLS gRPC to the ingest service.

## Workspace layout

- `sentry-parser` — pure-`std` RFC 5424 syslog parser with raw-passthrough
  fallback. No I/O, easy to unit test in isolation.
- `sentry-agent` — the binary: config loading, sourcing (journald/file),
  batching, mTLS gRPC client.

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

## Running

No CLI flags are required for the common case:

```sh
./sentry-agent
```

This uses `/etc/sentry-agent/agent.toml` if present, otherwise built-in
defaults: journald source (whole journal, no unit filter), service name
`default`, and mTLS material expected at
`/etc/sentry-agent/{ca,client,client-key}.pem`. mTLS is mandatory per the
project's transport requirements, so a from-scratch run with no certs in
place will fail fast with a clear error rather than connecting insecurely.

See `config/agent.example.toml` for all fields.

```sh
./sentry-agent --config /path/to/agent.toml
```

## Testing

```sh
cargo test --workspace
```

## Feature flags

- `journald` (default) — journalctl-based journald source.
- `file-tail` — polling-based file tailer (no inotify dependency; doesn't
  follow rename-based log rotation yet).

Both can be enabled together; `[source].kind` in config picks which one
runs. Building without a feature and configuring that source at runtime
fails at startup with a clear error rather than silently doing nothing.
