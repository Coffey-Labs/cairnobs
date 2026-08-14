# Same shape as every other Go service's Dockerfile in this repo
# (alerting/Dockerfile, enterprise/Dockerfile) -- context is
# deploy/operator/ itself, no /proto dependency.
#   docker build -f deploy/operator/Dockerfile -t sentry-tenant-operator deploy/operator/
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/tenant-operator ./cmd/tenant-operator

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/tenant-operator /tenant-operator
ENTRYPOINT ["/tenant-operator"]
