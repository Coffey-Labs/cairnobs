# Cairn OBS (obs-beacon)

The new Cairn OBS: a self-hosted monitor for networks, hosts and
services, with maps, metrics, alerts and reports. `obs-beacon-agent` runs
on each host and reports to `obs-beacond`, which judges the reports,
runs the alerts and serves the web interface. Licensed AGPLv3; the
third-party code inside the binaries and its licenses are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). **Private until launch.**

The documentation for people installing it is at
<https://docs.cairnobs.org/>. To report a security problem, see
[SECURITY.md](SECURITY.md). The notes below are for working on the code.

- [docs/install.md](docs/install.md): install the packages, make a release
- [docs/kubernetes.md](docs/kubernetes.md): the server on OpenShift, OKD or Kubernetes
- [docs/container.md](docs/container.md): the server in one container, with Docker or Podman
- [docs/development.md](docs/development.md): build, test, run it locally
- [docs/protocol.md](docs/protocol.md): how the agent reports
- [docs/formats.md](docs/formats.md): config and expected-containers files

## Layout

```
cmd/obs-beacond      the server
cmd/obs-beacon-agent the agent, and its read-only root helper
internal/collect     host readings from /proc and statfs
internal/docker      Docker client and the helper's read-only gate
internal/report      the wire format between them
internal/server      API, judgement, sessions, embedded UI
web/                 the Svelte 5 interface
config/              example configs
deploy/systemd/      unit files for the hosts
```
