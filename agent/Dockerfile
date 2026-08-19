# Build context must be the repo root (sentry/), not agent/, since this
# needs both agent/ and proto/:
#   docker build -f agent/Dockerfile -t sentry-agent .

FROM rust:1-alpine AS builder
RUN apk add --no-cache musl-dev protobuf-dev protobuf
WORKDIR /src
COPY proto ./proto
COPY agent ./agent
WORKDIR /src/agent
# file-tail is additive (no extra deps -- see Cargo.toml's [features]
# doc comment) and needed on any host whose logs aren't in journald
# (e.g. nginx's plain access/error log files), so it's built in
# alongside the journald default rather than left as an opt-in most
# deployments would otherwise have to remember to ask for.
RUN rustup target add x86_64-unknown-linux-musl \
    && cargo build --release --target x86_64-unknown-linux-musl -p sentry-agent --features journald,file-tail

FROM scratch
COPY --from=builder /src/agent/target/x86_64-unknown-linux-musl/release/sentry-agent /sentry-agent
ENTRYPOINT ["/sentry-agent"]
