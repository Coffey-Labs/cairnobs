# The Pure arrays: design note

Status: decided 2026-11-13 (Decisions, at the end); built in the order
under Plan, all five steps.

The deployments Cairn OBS is aimed at keep their OpenShift volumes, their
VMs' disks and their databases on Pure FlashArrays (docs/kubernetes.md,
docs/scale-and-availability.md: CloudNativePG on `pure-block`). The
cluster watcher sees those volumes from the Kubernetes side, as PVCs that
are bound or not. It doesn't see the array: a controller failing over, a
drive gone, the array 92% full, an ActiveCluster pod out of sync, a
replication link down. This note is how the arrays get in.

## What we're aiming for

- **Every array, its controllers, its volumes and its replication**, on
  the same pages as the clusters and VMs that use them.
- **Read-only.** A Purity user with the built-in `readonly` role and an
  API token. Nothing is ever written to an array.
- **Pure judges the array.** Purity raises its own alerts, with its own
  thresholds and severities, and support already knows them. We pass them
  on, as we do vCenter's alarms and Prism's alerts, and read the few
  states that matter on their own (controllers, hardware, pods, links).
- **A PVC and its array volume are one thing.** A Pure volume in trouble
  shows on the PVC that uses it, and on the VM whose disk it is.
- **No new dependency.** Purity's REST API 2 is HTTP and JSON; no SDK,
  so the agent doesn't grow.

## What's read

A host agent (any one that can reach the arrays' management addresses,
on port 443) reads each array every minute, as the platform readers do
(cluster-watcher.md, steps 7 to 9). REST 2 logs in with the API token
(`POST /api/2.x/login`, `api-token` header) and gets a session token back;
everything after is GET:

| Read | Becomes |
|---|---|
| `/arrays`: name, Purity version, model, capacity, space used, data reduction | the array's own record: "fa-prod-01 · read by mgmt1", "Purity//FA 6.7.3" |
| `/controllers`: mode (primary, secondary), status | a node for each controller; not ready is down, a failover is noted |
| `/hardware`, `/drives`: anything not healthy | listed on the array's record; a failed part makes it degraded |
| `/volumes` (not destroyed): provisioned, used, data reduction | a volume record each (thousands is fine: the batches already carry 10,000) |
| `/pods`: ActiveCluster pods and each array's status in them | a part of the array's record, as OpenShift's operators are: `pod/prod`, "resyncing" or "offline" |
| `/array-connections`: replication links | parts: `connection/fa-dr-01`, connected or not |
| `/alerts?filter=state='open'`: Purity's open alerts | on the record they're about; a critical one makes it degraded |

Volumes destroyed but not yet eradicated (Purity keeps them 24 hours) are
left out.

## Linking a PVC to its volume

Pure's CSI driver names each array volume after the PersistentVolume:
`<prefix>-pvc-<uid>`. The cluster agent already sends each PVC; it also
sends the PV name it's bound to (`spec.volumeName`, from the PVC
itself, so no new permission; step 3). The server then links the two the
way it links a VM to its host agent (step 4b): the PVC's page shows the array
volume and its array, and a volume on an array that's down explains the
PVCs on it going wrong, as one incident.

## FlashArray (step 2, 2026-11-13)

- **Who reads it:** any host agent that can reach the arrays'
  management addresses, with a `pure:` list (formats.md). Each array is a
  source of its own: "fa-prod-01 · read by mgmt1", "Purity//FA 6.7.3".
- **How:** `/api/api_version` first, for the newest REST 2.x the array
  offers (Purity 6.0 or later; an older one says so); then the API token
  is traded for a session (`POST /api/2.x/login`), kept, and traded again
  when the array forgets it. Every minute: `/arrays`, `/controllers`,
  `/hardware`, `/drives`, `/volumes?destroyed=false`,
  `/pods?destroyed=false`, `/array-connections` and
  `/alerts?filter=state='open'`, 500 a page with continuation tokens.
  Only GET after the login. No SDK: the agent doesn't grow.
- **Records:** the array (capacity, how full, data reduction, model,
  version); its array controllers as nodes (offline, not present or not
  ready is down, updating updating; primary or secondary said; shelf
  controllers left out); volumes (not protocol endpoints; demoted and
  unconnected said, not judged); pods and connections as the array's
  parts, "Array pod prod-pod" and "Replication to fa-dr-01" (resyncing is
  updating, offline or the mediator out of reach degraded; a connection
  connecting is down, partly connected or unbalanced degraded).
- **Purity judges:** hardware or drives not healthy, and parity under
  1.0 (rebuilding), make the array degraded, saying which. Open alerts go
  on the record they name (a controller's ports on the controller, a
  volume, a pod, a connection), the rest on the array; a critical one
  makes a record that's otherwise fine degraded, "Alert from Purity: …".
  Hidden ones are left out. No space thresholds of ours.
- **On the pages and in alerts:** an array is "Array fa-prod-01
  (mgmt1)", its controllers "Controller CT1 in …"; vCenters and Prism
  Central now say what they are too, their nodes "Host …".
- **Tested** against a fake of the documented API only, as decided (field
  names from Pure's BSD-licensed Python client): 1,200 volumes over
  three pages, a lapsed session, a CA and a pinned certificate, a wrong
  pin, a wrong token, an array with no REST 2, a 403 on pods (a problem on
  the array's record) and on `/arrays` (the read fails); and in the fleet
  simulator: `-pure 4` makes fa-prod-01 and -02 (an ActiveCluster pod,
  sync and async replication; a failed drive and a volume over its space
  on -01), fa-dr-01 (an unbalanced connection) and fa-vdi-01 (a
  controller updating), 600 volumes each, all read by phx1-stor01. Not
  against a real array.
- **Performance:** step 4, below.

## PVC and volume (step 3, 2026-11-13)

- **The cluster agent** sends each PVC's PersistentVolume as its
  `volume` fact (`spec.volumeName`, from the PVC: no new permission).
- **The link:** an array volume whose name ends in a PersistentVolume's
  name (`k8s-pvc-<uid>`, or `<pod>::k8s-pvc-<uid>` in a pod) is that PVC's.
  A volume in an ActiveCluster pod is on both arrays, so its PVC links to
  both. Matched on every page load and every judging, from what's in
  memory: nothing new is stored.
- **On the pages:** Workloads shows a PVC's array volume ("On an array:
  k8s-pvc-… on fa-prod-01 (mgmt1), and its state") and an array volume's
  PVC ("Used by shop/data-db-0 in prod"), each a click away.
- **In alerts:** a PVC's alert says its array side; an array volume's says
  which PVC it is; an array in trouble, or silent, says how many PVCs its
  volumes hold and in which clusters ("Its volumes hold 312 PVCs, in
  prod, cluster-01"). That's the "one incident" this step promised, as far
  as it goes: the pods failing on a lost array still open their own, as
  they do for a lost node's storage. Folding those in is grouping's work
  (maps step 6), later.
- **Tested:** a PVC and an array volume in a pod linked both ways, an
  unlinked PVC left alone, the three alert details; in the fleet
  simulator, cluster-00's 300 VM disks are fa-prod-01's volumes and
  cluster-01's are in the pod fa-prod-01 and -02 share.

## Performance and space (step 4, 2026-11-13)

- **Read:** `/arrays/performance` every minute, with the rest: reads and
  writes a second, microseconds a read and a write take, bytes read and
  written a second, all protocols together, as Purity averages its last
  few seconds. Space is `/arrays`' physical total of its capacity. An
  array that won't give its performance is still judged as before; its
  record says "performance: can't read it: …".
- **Sent** as the array's usage reading (`usage[].io`, on its own
  record), as a cluster agent sends CPU and memory: once a minute, not in
  the records.
- **Kept:** each copy's recorder adds each reading to the hour and, at
  the hour's turn, writes to `workload_hours` the averages, the worst
  reading's read and write latency, and the space used at the end of the
  hour. Ten columns, 0 for everything that isn't an array. Kept 400 days,
  as host and workload hours are. Schema 15.
- **Shown:** an array's card on Clusters has its numbers now (reads /
  writes a second, latency, bandwidth, space used of capacity, marked
  from 90%) and five lines, hourly averages over a day, 30 days or a year
  (`GET /api/v1/workloads/hours?days=`, now up to 400; a year is averaged
  into 200 points a line). The cards also say "controllers" and
  "volumes" for an array, not "nodes" and "workloads".
- **Not judged:** no latency or space thresholds of our own (decision
  3); Purity's alerts say when space runs short.
- **Upgrade obs-beacond first.** An older one ignores `io`; nothing
  breaks, the numbers just aren't kept.
- **Tested:** the fake REST 2 array's performance read, and one that
  refuses it; the readings rolled into an hour (averages, worst latency,
  the latest space) and stored on SQLite and PostgreSQL; the Clusters
  view. The fleet simulator's arrays are busy in the afternoon, quiet at
  night; fa-prod-01's writes are slow while its failed drive is rebuilt,
  and fa-dr-01 is written in bursts. Not against a real array.

## FlashBlade (step 5, 2026-11-13)

- **Set up** as a FlashArray is, in the same `pure:` list, with `type:
  flashblade`. A FlashBlade set up without it says so when the login
  isn't found ("is it a flashblade? then say type: flashblade").
- **How:** the same REST 2 client (`internal/purerest`, shared with
  FlashArray now): the newest 2.x from `/api/api_version` (Purity//FB
  3.0 or later), the token traded at `/api/login`, which on a FlashBlade
  isn't under the version. Every minute: `/arrays`, `/arrays/space`,
  `/blades`, `/hardware` (what isn't healthy, blades aside), `/file-systems`
  and `/buckets` (not destroyed), `/file-system-replica-links`,
  `/bucket-replica-links`, `/array-connections`, open `/alerts`, and
  `/arrays/performance`.
- **Records:** the array (Purity//FB, its version, model such as
  FlashBlade//S, capacity, used %, data reduction; degraded on hardware
  that isn't healthy, parity below 1, a critical alert, or a list it
  can't read); blades as its nodes ("Blade CH1.FB7 … is down"; an
  evacuating one is updating, an evacuated one stopped, empty slots left
  out); file systems and buckets as volumes are, with their size, use,
  protocols (NFSv3, NFSv4.1, SMB, HTTP) or account, objects and quota;
  replication to other arrays as parts, as a FlashArray's are.
- **Replica links** go on the file system or bucket they replicate ("to
  fb-dr-01, replicating, 4 min behind"); an unhealthy one makes it
  degraded, with Purity//FB's reason. Paused or quiesced is noted, not
  trouble.
- **Linked to PVCs** as FlashArray volumes are (step 3): Portworx names a
  FlashBlade file system after its PersistentVolume
  (`px_<cluster>-pvc-<uid>`), and the link matches the name's end.
- **Performance and space** as step 4, from the same endpoints.
- **Tested:** a fake of the REST 2 API (login outside the version, 700
  file systems over two pages, blades in each state, an unhealthy link, a
  paused bucket link, an incompatible connection, alerts on a blade, a
  bucket and a fan); one that refuses buckets and performance; a
  FlashBlade set up as a FlashArray. In the fleet simulator, fb-prod-01:
  ten blades, one evacuating, a failed fan, 400 file systems (cluster-02's
  first 100 VM disks among them) and 40 buckets. Not against a real
  FlashBlade.

## Plan

One PR each, in order, after the questions are answered:

1. **Sources by name** (done): an agent's batches carry the name of what
   they're about (`source`), and the server keeps one source per agent
   and name, `mgmt1:vcsa-01`, each its own card. The agent's reports list
   what it reads (`sources`), so one taken out of its config is
   forgotten, and so is one an older agent sent under its bare name.
   Proxmox, vSphere and Nutanix are named sources now (`name:`, default
   `proxmox` or the host name), one of each per agent.
2. **FlashArray** (done; FlashArray, below): the reader, the records, a
   `pure:` section in agent.yaml (a list of arrays, each with `url`,
   `token_file`, `ca_file` or `fingerprint`), a fake of the documented
   API for the tests, and fake arrays in the fleet simulator.
3. **PVC ↔ volume** (done; PVC and volume, below): `volume` on PVC
   records, the link both ways, each side's alerts naming the other.
4. **Performance and space history** (done; Performance and space,
   below): read latency, IOPS and bandwidth (`/arrays/performance`) and
   space, hourly, kept 400 days as hosts' numbers are; a graph on the
   array's page.
5. **FlashBlade** (done; FlashBlade, below): the same, for file systems
   and buckets.

## Questions

As put to John; my recommendation first in each.

1. **One agent, many arrays?** (a) Yes: the server keeps a source per
   agent and name, so one management host reads all twenty arrays and
   each is its own card on Clusters. That also lifts the "one platform
   per agent" rule from steps 7 to 9. (b) No: one agent per array, no
   server change.
2. **Which Pure products, in what order?** (a) FlashArray, then
   FlashBlade; Portworx, which runs inside the cluster, is read by the
   cluster agent from its StorageCluster status, later. (b) FlashArray
   only.
3. **Who judges the array?** (a) Purity's own alerts, plus controllers,
   hardware, pods and links read directly. (b) Also our own space
   thresholds (85% warning, 95% critical, as datastores have), which
   would disagree with Purity's where an array's were changed.
4. **Testing.** Pure has no free simulator; Cloud Block Store needs a
   Pure subscription. (a) A fake built from Pure's published REST 2
   reference and its open-source Python client's models (as Nutanix
   was), and a read-only run on a real array only if you have one to
   point it at. (b) Wait for a real array.
5. **Performance numbers.** (a) Hourly, as in plan step 4, kept 400
   days. (b) Not at all: Pure1 already graphs them.

## Decisions

2026-11-13, John went with the recommendation on each ("next", after the
questions were put):

1. **One agent reads many arrays**, each a source of its own, by name.
2. **FlashArray, then FlashBlade**; Portworx later, from the cluster agent.
3. **Purity judges**: its open alerts, plus controllers, hardware, pods
   and links read directly. No space thresholds of our own.
4. **Tested against a fake** of the published REST 2 API; a real array
   only if one is offered.
5. **Performance and space hourly**, kept 400 days.
