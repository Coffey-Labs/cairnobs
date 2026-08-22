# alerting never needs /proto (it talks to /api over plain HTTP, no gRPC),
# so unlike api/ingest/search this build context is just alerting/ itself,
# same shape as cli/Dockerfile:
#   docker build -f alerting/Dockerfile -t cairnobs-alerting alerting/
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/alerting ./cmd/alerting

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/alerting /alerting
ENTRYPOINT ["/alerting"]
