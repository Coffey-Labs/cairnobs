# One-shot migration runner: bash + psql client baked in, migrations/*.sql
# copied in at build time. Mirrors /storage/Dockerfile's shape.
#   docker build -f metadata/Dockerfile -t sentry-metadata-migrate metadata/
FROM alpine:3.20
RUN apk add --no-cache bash postgresql16-client
WORKDIR /metadata
COPY migrate.sh ./
COPY migrations ./migrations
ENTRYPOINT ["bash", "migrate.sh"]
