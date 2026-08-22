# Built FROM the Redpanda image so `rpk` is already present -- no need for
# a separate client install, and no Docker socket access needed since
# provisioning happens over the network, not via `docker exec`.
#   docker build -f transport/Dockerfile -t cairnobs-transport-provision transport/
FROM docker.redpanda.com/redpandadata/redpanda:v24.2.7
COPY provision-topics.sh /provision-topics.sh
ENTRYPOINT ["/provision-topics.sh"]
