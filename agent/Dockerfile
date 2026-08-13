# Build context must be the repo root (sentry/), not agent/, since this
# needs both agent/ and proto/:
#   docker build -f agent/Dockerfile -t sentry-agent .

FROM rust:1-alpine AS builder
RUN apk add --no-cache musl-dev protobuf-dev protobuf
WORKDIR /src
COPY proto ./proto
COPY agent ./agent
WORKDIR /src/agent
RUN rustup target add x86_64-unknown-linux-musl \
    && cargo build --release --target x86_64-unknown-linux-musl -p sentry-agent

FROM scratch
COPY --from=builder /src/agent/target/x86_64-unknown-linux-musl/release/sentry-agent /sentry-agent
ENTRYPOINT ["/sentry-agent"]
