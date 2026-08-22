# Build context must be the repo root (sentry/), since this needs both
# search/ and proto/:
#   docker build -f search/Dockerfile -t cairnobs-search .
#
# Unlike /agent, this doesn't need musl/static linking -- it's a normal
# server-side container, not an edge-deployed binary, and both Tantivy and
# rskafka are pure Rust (no C deps to link against). distroless/cc rather
# than distroless/static: a normal dynamically-linked binary, not a fully
# static one.
FROM rust:1-slim AS builder
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY proto ./proto
COPY search ./search
WORKDIR /src/search
RUN cargo build --release

FROM gcr.io/distroless/cc-debian12
COPY --from=builder /src/search/target/release/cairnobs-search /cairnobs-search
ENTRYPOINT ["/cairnobs-search"]
