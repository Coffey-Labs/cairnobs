# Commercial-license module, built the same way as every other Go
# service here -- no /proto dependency, context is enterprise/ itself,
# same shape as cli/Dockerfile and alerting/Dockerfile.
#   docker build -f enterprise/Dockerfile -t sentry-enterprise-auth enterprise/
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/enterprise-auth ./cmd/enterprise-auth

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/enterprise-auth /enterprise-auth
ENTRYPOINT ["/enterprise-auth"]
