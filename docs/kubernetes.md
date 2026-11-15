# The server on OpenShift, OKD or Kubernetes

The server also comes as a container image, `obs-beacond`, for linux/amd64
and linux/arm64. It holds the static binary, a CA bundle, the license and
the third-party notices, and nothing else. It runs as whatever user ID the
cluster assigns, in group 0, with a read-only root file system and no
capabilities. That fits OpenShift's and OKD's default `restricted-v2`
security context constraint, and the Kubernetes "restricted" pod security
standard, unchanged.

Without a cluster, the same image runs as one container with Docker or
Podman (container.md). Agents stay on the hosts they watch (install.md). In the cluster runs one
server pod. Its database can be PostgreSQL instead of the SQLite file on
its volume (formats.md, State). On PostgreSQL several copies can share
the one database, and only one of them sends alerts: the `-ha` overlays
(More than one copy, below).

## The image

Each release's image is on GitHub's registry, for linux/amd64 and arm64:
`ghcr.io/coffey-labs/cairnobs:0.1.0` (and `:latest`, the newest final
release). To copy it into your own registry, OpenShift's internal one
included:

```sh
skopeo copy --all docker://ghcr.io/coffey-labs/cairnobs:0.1.0 docker://registry.example.org/ops/obs-beacond:0.1.0
```

Or build it yourself from a release's binaries:

```sh
scripts/build-release.sh v0.1.0 dist                        # or download a release into dist/
scripts/build-image.sh v0.1.0 dist                          # writes dist/obs-beacond-image-v0.1.0.tar
scripts/build-image.sh v0.1.0 dist registry.example.org/ops/obs-beacond   # or pushes it
```

The archive is an OCI image index: `skopeo copy
oci-archive:dist/obs-beacond-image-v0.1.0.tar docker://<registry>/<path>:0.1.0`
puts it in any registry, including OpenShift's internal one.

## Deploy

The manifests are in `deploy/kubernetes`, as a kustomize base with two
overlays:

| Overlay | For | Outside access |
|---|---|---|
| `openshift/` | OpenShift and OKD (same APIs, same steps) | two Routes: the web interface with edge TLS, the agent port passed through |
| `plain/` | any other Kubernetes | an Ingress for the web interface, a LoadBalancer Service for the agent port |
| `openshift-ha/`, `plain-ha/` | the same, with several copies on PostgreSQL (More than one copy, below) | as above |

Before applying, set in your copy:

- `base/kustomization.yaml`: the image's registry path and tag.
- `base/obs-beacond.yaml`: `alerts.public_url` (the web address) and
  `join.agent_url` (the agent address as hosts reach it, port 443 behind
  an OpenShift Route). Everything else from formats.md works here too.
- `openshift/routes.yaml` (or `plain/ingress.yaml`): the host names.
- `base/pvc.yaml`: the storage class and size.

Then, in a namespace of its own:

```sh
oc new-project cairnobs
# The state key seals secrets in the database. Make it once and keep a copy
# somewhere safe: without it the database's secrets can't be read.
oc create secret generic obs-beacon-state-key \
  --from-literal=state.key="$(head -c 32 /dev/urandom | base64)"
oc apply -k deploy/kubernetes/openshift
oc logs deploy/obs-beacond        # the one-time setup link for the first admin
```

With `kubectl`, the same with `kubectl create namespace` and
`deploy/kubernetes/plain`.

## Storage

The volume holds the database (with SQLite), the agent CA and the ACME
cache. SQLite
needs a disk of its own, so the claim is **ReadWriteOnce block storage**
with a file system on it:

| Storage | Class (usual name) |
|---|---|
| Pure FlashArray through Pure Service Orchestrator / Pure CSI | `pure-block` |
| Portworx (Pure) | `px-csi-db`, or a class with `repl: "2"` or more |
| OpenShift Data Foundation | `ocs-storagecluster-ceph-rbd` |

Never put it on a shared file system (NFS, `pure-file`, CephFS,
ReadWriteMany): SQLite's locking isn't safe there. 10 GiB is plenty for
dozens of hosts.

The deployment uses the `Recreate` strategy, so during an upgrade the old
pod lets go of the volume before the new one mounts it.

## More than one copy

`openshift-ha/` and `plain-ha/` add `components/postgres` to the overlays
above. They run three or more copies of obs-beacond on one PostgreSQL
database, so a pod, a node or an upgrade takes nothing down
(scale-and-availability.md):

- A **CloudNativePG** `Cluster`, `obs-beacon-db`: three PostgreSQL
  instances that fail over by themselves. Install the operator first
  (OperatorHub on OpenShift and OKD, or its release manifest elsewhere).
  It makes the `obs-beacon-db-app` Secret with the password and the
  `obs-beacon-db-ca` Secret, which obs-beacond checks the server's
  certificate against.
- The Deployment at **three replicas, rolling**: `maxUnavailable: 0`, so a
  new copy is ready before an old one stops. `/readyz` passes only once a
  copy has caught up with the others, and fails as soon as it's told to
  stop. It keeps serving for `shutdown_delay` (10 s here) while traffic
  moves off, then closes. Pods are spread across nodes where possible.
- A **PodDisruptionBudget** (one copy down at a time) and a
  **HorizontalPodAutoscaler** (3 to 10 copies at 70% CPU).
- No volume claim: with PostgreSQL nothing on the pod needs keeping.
- On a fresh install the copies wait (up to 2 minutes, with a startup
  probe allowing 3) while the operator brings the database up, so they
  start as soon as it's there instead of restarting.

Set `components/postgres/obs-beacond.yaml` instead of
`base/obs-beacond.yaml` (it replaces it), and the storage class and size
in `components/postgres/database.yaml`. The same state key Secret serves
every copy.

During an upgrade, old and new copies serve side by side. Each copy
stores its web interface's hashed files in the database when it starts,
so a page from either version finds its scripts on any copy. One copy at
a time is the evaluator, which judges alerts and sends them. Which one it
is shows in the logs, and a stopping copy hands the role over at once.

To move an existing single-copy install onto this, see formats.md (State,
`obs-beacond store copy`).

## Agents

Agents talk to the server over TLS with client certificates from Cairn
OBS's own CA, so their connection must reach the pod untouched:

- OpenShift and OKD: the `obs-beacond-agents` Route uses `passthrough`.
  Agents connect to `https://<its host>:443`, which `join.agent_url` names.
- Elsewhere: the LoadBalancer Service on port 8461, or an ingress
  controller with TLS passthrough.

Join tokens from the Hosts page then work as on any install. The image
doesn't carry agent packages, so set `join.packages_url` to a release's
download location (install.md, Adding a host).

## Watching clusters

Each cluster Cairn OBS watches, this one included, runs one cluster agent
(cluster-watcher.md): `obs-beacon-agent cluster`, from the same image as
obs-beacond. It reads the cluster with `get`, `list` and `watch` only and
sends its nodes, pods, controllers, jobs and volumes over the agent port,
as a host's agent does.

1. Clusters pull this image from `ghcr.io/coffey-labs/cairnobs`; if yours
   can't reach it, set `join.image` in obs-beacond.yaml to your copy
   (formats.md, Joining). For the cluster obs-beacond runs in,
   `join.agent_url` may be the Service: `https://obs-beacond.<namespace>.svc:8461`.
   Other clusters need the agent Route or load balancer.
2. **Configure › Clusters › Add a cluster** gives one command, good once
   for an hour:

   ```sh
   curl -fsSL -H 'Authorization: Bearer cobsj1_…' https://obs.example.org/cluster.yaml | kubectl apply -f -
   ```

   Run it as a cluster admin (`oc apply -f -` works the same). It makes
   the `cairn-obs-agent` namespace (Pod Security `restricted`), a
   read-only ClusterRole, and the agent with the token in its Secret. The
   agent joins and keeps its key and certificate in that Secret, which
   its Role lets it read and patch, and no other.

The manifest is deploy/kubernetes/cluster-agent/cluster-agent.yaml. The
token travels in a header, not the address, so it stays out of proxies'
and servers' logs.

- **Upgrade the agent:** after upgrading obs-beacond, apply the same
  address without the token:
  `curl -fsSL https://obs.example.org/cluster.yaml | kubectl apply -f -`.
  The agent restarts on the new image and carries on with its
  certificate.
- **Remove a cluster:** Remove on Configure › Clusters revokes its
  certificate and forgets what it sent; then
  `curl -fsSL https://obs.example.org/cluster.yaml | kubectl delete -f -`
  takes the agent out.
- **OpenShift and OKD:** the same manifest; the ClusterRole also reads
  the cluster version, the cluster operators and the machine config pools
  (formats.md, Cluster agents). An update shows as Updating, not as
  outages.
- **Alerting:** only the cluster's own trouble and its nodes page at
  first. Adopt namespaces, controllers and VMs on Configure › Clusters ›
  the cluster (What pages), or annotate a namespace, controller, job or
  volume with `obs.cairnobs.org/alert: page` (or `warn`, or `off`)
  (formats.md, Cluster agents).

## Day to day

- **Upgrade:** change the tag and apply again. With one copy the pod is
  replaced (a short gap); with `-ha` the copies roll one at a time.
- **Back up** the volume (a CSI snapshot is fine) and, separately, the
  state key secret.
- **Emergency door:** `emergency_listen` is loopback-only; reach it with
  `oc port-forward deploy/obs-beacond <port>`.
- **Moving to another namespace:** OpenShift gives each namespace its own
  user ID range. Files the old pod wrote are owned by the old ID and
  readable only by it, so restore the volume with a pod that runs
  `chown -R <new uid>` on it first, or set the new namespace's
  `openshift.io/sa.scc.uid-range` annotation to the old range.

## Tested

- The image runs as an arbitrary user ID in group 0 with a read-only root,
  no capabilities and group-0 volumes.
- On a Kubernetes 1.32 cluster (kind) with the namespace enforcing the
  "restricted" pod security standard: the base with an OpenShift-style
  assigned user ID, fsGroup and group 0, and the `plain/` overlay, both
  admitted and running; the state survived the pod being replaced (same
  agent CA); a real agent enrolled over mTLS through the Service and
  reported.
- Not yet on a live OpenShift or OKD cluster, so the Routes themselves are
  untested.
- `plain-ha/` as shipped, with CloudNativePG 1.30.1 on kind: installed
  fresh, upgraded and rolled back under load, the evaluator killed, and
  the database switched over and failed over, with no failed page
  requests except for about 1.5 s after a copy was killed outright
  (scale-and-availability.md, Results so far).
