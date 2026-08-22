# One-shot migration runner: bash + curl baked in, migrations/*.sql copied
# in at build time. No runtime package install and no host volume mount
# needed — works offline once built.
#   docker build -f storage/Dockerfile -t cairnobs-clickhouse-migrate storage/
FROM alpine:3.20
RUN apk add --no-cache bash curl
WORKDIR /storage
COPY migrate.sh ./
COPY migrations ./migrations
ENTRYPOINT ["bash", "migrate.sh"]
