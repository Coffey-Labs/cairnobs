# AGPLv3 module (same as core as of Phase 6), built like every other Go service here --
# context must be the repo root, not enterprise/ alone. enterprise/go.mod
# has replace directives for api/, ingest/, and proto/ (all resolved as
# sibling directories, e.g. ../api), and enterprise-auth needs api/
# specifically for two real reasons: cmd/enterprise-auth/main.go imports
# api/httpserver directly (WithCredentialedCORS, for the tenant-picker's
# credentialed cross-origin requests), and internal/rbacstore's
# DashboardPermissions adapter imports api/dashboards transitively.
# Go's module resolution needs the whole module's go.mod satisfied to
# build any one package in it, so this was never actually optional the
# way the old enterprise/-only context assumed -- confirmed broken the
# first time this was built with real Docker access after those two
# imports existed; the repo-root context below is the same shape
# enterprise-api's and enterprise-ingest's Dockerfiles already use for
# the identical reason.
#   docker build -f enterprise/Dockerfile -t cairnobs-enterprise-auth .
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
WORKDIR /src/enterprise
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/enterprise-auth ./cmd/enterprise-auth

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/enterprise-auth /enterprise-auth
ENTRYPOINT ["/enterprise-auth"]
