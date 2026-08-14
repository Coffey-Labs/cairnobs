# Same shape as every other Go service's Dockerfile in this repo --
# context must be the repo root (needs both enterprise/ and proto/, like
# api/Dockerfile does for api/ + proto/), not enterprise/ alone.
#   docker build -f enterprise/cmd/enterprise-api/Dockerfile -t sentry-enterprise-api .
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
WORKDIR /src/enterprise
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/enterprise-api ./cmd/enterprise-api

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/enterprise-api /enterprise-api
ENTRYPOINT ["/enterprise-api"]
