# Build context must be the repo root (sentry/):
#   docker build -f api/Dockerfile -t sentry-api .

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY api ./api
WORKDIR /src/api
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/api /api
ENTRYPOINT ["/api"]
