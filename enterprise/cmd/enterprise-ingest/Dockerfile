# Same shape as every other Go service's Dockerfile in this repo --
# context must be the repo root (needs ingest/, proto/, and enterprise/,
# like enterprise-api/Dockerfile does for api/ + proto/ + enterprise/),
# not enterprise/ alone.
#   docker build -f enterprise/cmd/enterprise-ingest/Dockerfile -t cairnobs-enterprise-ingest .
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
WORKDIR /src/enterprise
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/enterprise-ingest ./cmd/enterprise-ingest

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/enterprise-ingest /enterprise-ingest
ENTRYPOINT ["/enterprise-ingest"]
