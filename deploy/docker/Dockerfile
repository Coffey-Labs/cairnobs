# The obs-beacond image: the static binary and the host's CA bundle, nothing else.
# hosts-baseline's beacon-apply.sh assembles the build context (obs-beacond from
# `make`, ca-certificates.crt from the host) and runs it with the host's
# obs-beacon user, read-only, all capabilities dropped. The published image
# (at launch) also needs THIRD_PARTY_NOTICES.md and LICENSE in it.
FROM scratch
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY obs-beacond /obs-beacond
USER 65534:65534
ENTRYPOINT ["/obs-beacond", "-config", "/etc/obs-beacon/obs-beacond.yaml"]
