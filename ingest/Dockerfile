# Build context must be the repo root (sentry/), not ingest/, since this
# needs both ingest/ and proto/:
#   docker build -f ingest/Dockerfile -t cairnobs-ingest .

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY proto ./proto
COPY ingest ./ingest
WORKDIR /src/ingest
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ingest ./cmd/ingest

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/ingest /ingest
ENTRYPOINT ["/ingest"]
