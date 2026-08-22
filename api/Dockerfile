# Build context must be the repo root (sentry/), since this needs both
# api/ and proto/ (api now speaks gRPC to /search, using proto's checked-in
# Go bindings via the `replace` directive in api/go.mod):
#   docker build -f api/Dockerfile -t cairnobs-api .

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY proto ./proto
COPY api ./api
WORKDIR /src/api
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/api /api
ENTRYPOINT ["/api"]
