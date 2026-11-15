# The cluster watcher: design note

Status: decided 2026-11-13 (Decisions, at the end); built in the order
under Plan. All nine steps are done: workloads in the server, the cluster
agent for Kubernetes, OpenShift and OKD, KubeVirt, metrics, libvirt,
Proxmox, vSphere and Nutanix. The Pure arrays are next, in a note of
their own (docs/pure.md).

Cairn OBS watches hosts through an agent on each one. The deployments it's
meant for also run thousands of pods and VMs on Kubernetes, OpenShift and
OKD (VMs through OpenShift Virtualization, which is KubeVirt), on libvirt
and on Proxmox. Nobody installs an agent in a pod, and many VMs won't have
one either. This note covers how those workloads get in: what's read, how
it reaches obs-beacond, how it's stored and alerted on, and what the pages
show at that size.

It also finishes step 6 of scale-and-availability.md: the 10,000-workload
test needs workloads to exist.

## What we're aiming for

- **See every node, pod, VM and volume** in each cluster, with nothing
  installed in the workloads themselves.
- **Read-only, as the agent is.** Get, list and watch, nothing else: no
  exec, no Secrets, no writes to the cluster.
- **Several thousand workloads per cluster and several clusters**, at the
  targets already set: 10,000 workloads and 1,000 agents, pages under
  300 ms at p95.
- **One problem, one incident.** A node that goes down is one incident
  that names the 40 pods and 6 VMs on it, not 46 alerts. An OpenShift
  upgrade rolling through the nodes is maintenance, not an outage.
- **One model for every kind of VM.** A VM from KubeVirt, libvirt or
  Proxmox is the same thing on the pages and in alerts, and a VM that also
  runs our agent shows up once, with both views.

## Not in this note

- **The Pure arrays.** They get their own add-on (Purity's REST API with a
  read-only token), after this. Pure volumes used by the cluster do show
  here, as PVCs with their storage class.
- **Certificates kept in Secrets.** The certificate inventory is a separate
  note. Reading Secrets is a much bigger grant, and this design doesn't
  ask for it.
- **Prometheus.** OpenShift has one already. Reading it is an option for
  later (volume usage, VM guest metrics), not a dependency.
- **Writing anything to a cluster.** No restarts, no scaling, no cordons.

## Where the watching happens: a cluster agent

The watcher is `obs-beacon-agent cluster`: the same agent binary, run as
a Deployment (one replica) in each cluster it watches. It watches the
Kubernetes API with its own service account and sends what it sees to
obs-beacond over the agent port, with a client certificate, as a host's
agent does.

Why an agent in the cluster, rather than obs-beacond reading the API itself:

- **Credentials stay in the cluster.** The service account token never
  leaves. obs-beacond holds no cluster credentials at all.
- **Only outbound connections.** It needs to reach obs-beacond's agent port,
  as any agent does. A cluster in another network, behind NAT or in
  another site needs no way in.
- **Several clusters are the same as one**, including the one obs-beacond
  itself runs on.
- **obs-beacond stays one kind of server.** Ingest is stateless and runs
  on every copy (scale-and-availability.md). A watch held open by obs-beacond
  would have to live on one copy, and move when the evaluator moves.
- **What's sent is already trimmed.** The agent turns each object into a
  short record (below). Pod specs, environment variables and annotations
  never leave the cluster.

The cost: one more Deployment per cluster, to keep up to date. There's no
second image: the obs-beacond image carries the agent too
(`obs-beacon-agent cluster`), about 4 MB more.

**Joining** works as for a host. An admin chooses **Add a cluster** on
Configure → Clusters and gets one command:

```sh
curl -fsSL -H 'Authorization: Bearer cobsj1_…' https://obs.example.org/cluster.yaml | kubectl apply -f -
```

The token goes in a header, not in the address as first planned
(`?join=`): addresses end up in proxies' and servers' logs. The manifest
holds a namespace, the service account, the read-only ClusterRole and its
binding, the agent's Secret with the join token, and the Deployment. The
token is single use and good for an hour, as for hosts. On first start the
agent joins, makes its key and stores its certificate in that Secret,
which it alone may read and patch (a Role limited to that Secret's name).
After that it renews with its certificate, as any agent does. The same
address without the token gives the manifest alone, to upgrade the agent
or to delete it.

If the cluster agent stops, the cluster shows as **not current** and one
incident says so. Its workloads keep their last known state, greyed out;
they don't each go silent. A new copy lists everything again, so a gap
loses nothing that still matters: the cluster is the source of truth, and
restart counts, for one, only go up.

## What it reads

All through the API, with `get`, `list` and `watch` only:

| Kind | What we take | Why |
|---|---|---|
| Nodes | Ready and the pressure conditions, unschedulable, kubelet version, capacity and allocatable, roles | A node down or under pressure explains everything on it |
| Pods | phase, node, owner, each container's ready, restarts, waiting reason (CrashLoopBackOff, ImagePullBackOff), last termination (OOMKilled, exit code), start time | The workloads themselves |
| Deployments, StatefulSets, DaemonSets, ReplicaSets | desired, ready, available, updated; whether a rollout is stuck (`ProgressDeadlineExceeded`) | Pods are judged as their controller: "web: 2 of 3 ready" |
| Jobs and CronJobs | failed, last successful run | Backups and batch work that runs in the cluster |
| PVCs and PVs | phase (Pending, Lost), capacity, storage class, the volume's provisioner | Storage that won't bind, or is gone |
| Events (type Warning only) | reason, message, object, count | The detail for an incident: FailedScheduling, FailedMount, Evicted |
| Namespaces | name, our annotations | Where alert settings can live (below) |
| Services and EndpointSlices | type, cluster and external addresses, ports, node ports; which controllers or VMs the ready endpoints belong to | The service map's edges (maps.md): what's behind a Service, and what reaches it |
| Ingresses | hosts, the Services they send to, TLS or not (and which Secret, kept in the agent), class | The way in from the Internet, on the service map; each TLS host's certificate (below) |

When the API serves them (found by discovery, so nothing is configured):

| Kind | What we take |
|---|---|
| KubeVirt VirtualMachines | printable status (Running, Stopped, ErrorUnschedulable, CrashLoopBackOff, Migrating…), run strategy |
| KubeVirt VirtualMachineInstances | phase, node, guest agent connected, guest OS, IP addresses, firmware UUID, migration state |
| KubeVirt migrations | failed migrations |
| KubeVirt guest file systems (`filesystemlist`, read through the guest agent) | each file system's size and use, so a VM's full disk is caught with no agent in it |
| CDI DataVolumes | import or clone failures |
| OpenShift ClusterVersion | version, update in progress, failing |
| OpenShift ClusterOperators | Available, Degraded, Progressing |
| OpenShift MachineConfigPools | updating, degraded, how many nodes are done |
| OpenShift Routes | host, the Services it sends to, TLS or not; a certificate given inline (never its key) |
| cert-manager Certificates | names, issuer, when it began and ends, renewal time, Ready (never the Secret it fills, nor its name) |
| metrics.k8s.io (metrics-server; OpenShift has it) | CPU and memory per node and pod, every 60 s |

Not read, and not in the ClusterRole: Secrets, ConfigMaps, `pods/exec`,
`pods/attach`, `pods/portforward`, `nodes/proxy` (a `get` there reaches
the kubelet's API, which is more than it looks), and the guest agent's
list of logged-in users.

**Pod logs** are the one optional grant. With `logs: true` on a cluster,
the agent may read `pods/log`. It reads only when the server asks for the
last lines of one failing container, for an incident, as the host agent
does for Docker. Only the operator tier and admins see those lines. It's
off by default, since a log can hold anything.

## The record: one shape for every workload

The agent sends records, not Kubernetes objects:

```json
{
  "kind": "pod",
  "id": "uid of the object",
  "name": "web-7d9f8-x2k4q",
  "namespace": "shop",
  "owner": "deployment/web",
  "node": "worker-3",
  "state": "crashloop",
  "word": "Restarting (CrashLoopBackOff)",
  "since": "2026-11-13T09:12:40Z",
  "labels": {"app.kubernetes.io/name": "web"},
  "facts": {"restarts": 14, "last_exit": "OOMKilled", "ready": "1/2"}
}
```

- **Kinds:** `node`, `pod`, `controller`, `job`, `pvc`, `vm`, and
  `cluster` for the cluster's own state (its version, its operators).
- **state** is one of a small set (`ok`, `starting`, `stopped`,
  `degraded`, `down`, `crashloop`, `pending`, `updating`, `unknown`), so
  the server judges every source the same way. **word** is the human
  version. **facts** are kind-specific and small.
- **Labels:** only `app.kubernetes.io/*` and a configurable allowlist.
  Annotations: only our own, `obs.cairnobs.org/*`.
- **libvirt and Proxmox** send the same `vm` records (below), so the
  server doesn't know or care where a VM runs.

### On the wire

```
POST /api/v1/agent/workloads        (client certificate; protocol.md has the details)
{"source": "k8s", "generation": "…", "full": "begin" | "page" | "end" | "",
 "records": [...], "removed": ["id", ...]}
```

- On start, and every 30 minutes, the agent sends everything: a `begin`,
  pages of 500 records, an `end`. When `end` arrives, the server drops
  every record from that cluster not seen in that generation. That's how
  deletions missed during a gap are caught.
- In between it sends changes, batched every 5 s, and only the latest
  state of each object. A rollout of 300 pods is a few requests, not
  thousands.
- Reports from the agent's own host (its CPU, memory) don't apply: a
  cluster agent sends no host report and isn't counted as a host.

## Storing it

A new `workloads` table, written by whichever copy took the request, as
`hosts_live` is:

```
workloads (source TEXT, id TEXT, kind TEXT, namespace TEXT, name TEXT,
           owner TEXT, node TEXT, state TEXT, record TEXT,
           generation TEXT, changed_ms BIGINT, seq BIGINT,
           PRIMARY KEY (source, id))
```

- `seq` comes from one sequence and goes up with every write. Copies are
  told `workloads <source>` on the bus and read every row with a higher
  `seq` than they've seen, so a missed notice costs only a delay, as
  elsewhere.
- Each copy keeps the records in memory, indexed by source, kind,
  namespace, owner and node. 10,000 records is about 10 MB.
- On SQLite (the single container) it's the same table. The only difference
  is that there's no bus.

**History.** Hourly numbers, as `host_hours` has for hosts, are kept for
nodes, VMs and controllers, which last. They're not kept for pods, which
come and go: a pod's history is its controller's. That's the answer this
note proposes to open question 2 in scale-and-availability.md. The
changes of state (the `status_log`) are kept for everything, with the
same retention.

## Judging and alerting

The evaluator judges workloads as it judges hosts, from the records in
memory. The conditions:

| Condition | Severity |
|---|---|
| The cluster agent is silent, or can't reach its API | Critical, one incident for the cluster |
| A node isn't Ready (and isn't being updated, below) | Critical |
| A node under memory, disk or PID pressure | Warning |
| A controller with fewer ready than desired for `for` (5 min) | Warning; Critical when none are ready |
| A rollout stuck (`ProgressDeadlineExceeded`) | Warning |
| Pods in CrashLoopBackOff or ImagePullBackOff, by controller | Warning |
| An OOM kill | Warning |
| A Job failed, or a CronJob hasn't succeeded in `max_age` | Warning |
| A PVC Pending for 5 min, or Lost | Warning; Lost is Critical |
| A DataVolume's import or clone failed, or failing and restarting | Warning, on its volume |
| A VM that should run isn't Running, or is in ErrorUnschedulable or CrashLoopBackOff | Critical |
| A failed live migration | Warning |
| A VM's file system at 90% / 95% (from the guest agent) | Warning / Critical |
| A ClusterOperator Degraded, or not Available | Critical |
| An OpenShift update failing, or a MachineConfigPool degraded | Critical |

**Grouping**, with the root-cause grouping that's there now:

- A pod is judged as part of its controller. A Deployment with 3 crashing
  pods is one condition naming 3 pods. A pod with no controller stands
  alone.
- A node that's down explains its pods and VMs: one incident for the node,
  its workloads listed under it as affected.
- A silent cluster agent explains the whole cluster.
- Except: an incident that has already fired stays on its own, as with
  hosts. Folding it under the node would tell everyone it's resolved, then
  open it again when the node is back.
- PVCs that won't bind for one storage class are grouped by class and
  provisioner: one CSI driver failing is one incident.

**Updates aren't outages.** OpenShift and OKD upgrades reboot every node
in turn. While a MachineConfigPool is updating, the node it's working on
is **Updating**, not down: it's cordoned, then drained, then rebooted.
The same goes for any node cordoned by hand. Pods and VMs that move off
it (VMs by live migration) are expected to. If a node stays Updating
longer than `update_stall` (30 min), that's a warning. A maintenance
window can also cover a whole cluster, a node or a namespace.

### What pages, and who decides

Out of the box, at fleet size, paging on every namespace would be noise.
So:

- **Nothing pages until someone says so.** Cluster-level conditions
  (nodes, operators, the update, the agent) page from the start. Workload
  conditions are warnings, shown on the pages, until they're adopted.
- **Adopt**, as for hosts' containers today: Configure → Clusters → a
  cluster shows what's running and lets an admin adopt namespaces or
  single controllers and VMs. Adopted ones page, and one that goes
  missing is reported. Adopting a VM records whether it should be running
  or is stopped on purpose.
- **Or from the cluster.** An `obs.cairnobs.org/alert` annotation
  (`page`, `warn` or `off`) on a namespace, controller or VM sets the
  same thing. Teams that own a namespace can then decide in their own
  manifests, with no access to Cairn OBS. The annotation is read; the
  agent never writes it.
- `openshift-*` and `kube-*` namespaces are judged through their
  ClusterOperators, not pod by pod, unless adopted.
- **When they disagree** (settled while building 2b): what's set in Cairn
  OBS for the workload itself wins, then the annotation, then what's set
  in Cairn OBS for its namespace. An admin has the last word on any one
  workload, and a team's annotation is kept unless an admin set that very
  workload. Adopting is kept in expected.yaml's `clusters:`, beside what
  should run on hosts, so it's in a file or in the UI the same way and
  exports with it.

## A VM is a VM, wherever it runs

**libvirt/KVM:** the host agent reads its own host's domains through
libvirt's read-only socket, through the root helper as it does for Docker.
That covers state, vCPUs, memory, disk and network counters, and the
domain UUID. It's sent as `vm` records with the host as source.

**Proxmox:** the agent on one node of a Proxmox cluster, with a
`proxmox:` section and an API token holding the built-in PVEAuditor role,
reads every VM and container in that cluster from the local API. It sends
`vm` records with the Proxmox cluster as source. Proxmox nodes are Debian,
so that's our normal agent package. This is how Proxmox support starts.

**Linking a VM to its agent:** the host agent reports the machine's
SMBIOS UUID (`/sys/class/dmi/id/product_uuid`, read by the root helper).
KubeVirt's firmware UUID, libvirt's domain UUID and Proxmox's `smbios1`
UUID are the same number seen from outside. When they match, the pages
show one machine: the hypervisor's view (running, where, migrations) and
the agent's (services, containers, disks). A VM that's down then explains
its host going silent: one incident, not two.

As built (step 4b): the host page says which VM it is, and where it
runs; the VM's row on Workloads names its host agent. A silent host that's
a VM says what the hypervisor sees ("It's VM db in vms (prod), which is
stopped", or "running as far as the hypervisor knows: look at the guest,
its agent or the network"). Grouping treats a host as needing its VM and
the VM its node, so a VM down, or its node down, is one incident with
the host under it. A UUID two hosts claim, or two VMs (a clone that kept
it), links nothing, and neither does a firmware placeholder
(`03000200-0400-0500-0006-000700080009` and the like): a wrong link would
hide a real outage.

## The pages

Built on the paged, server-side lists from #69. No page ever lists
everything at once.

- **Dashboard:** one line per cluster: version, nodes ready of total,
  workloads by state, open incidents. The "needs attention" list takes
  workloads too, capped as for hosts.
- **Clusters:** each cluster's nodes, operators and update state, then
  its namespaces with counts by state, problems first.
- **Workloads:** one table for every kind, filtered by cluster, namespace,
  kind, node and state and searched by name, problems first, paged. The
  same table filtered to `vm` is the **VMs** page.
- **A controller, VM or node:** its record, its pods (for a controller or
  a node), its warning events, its history, its incidents. For operators
  and admins, the last log lines of a failing container, when the cluster
  allows logs.
- **Maps:** the service map can name a controller or a namespace as a
  service's part, so a Deployment that's down takes down the service that
  uses it. The network map draws clusters and nodes, not pods.

## Security

- The ClusterRole is `get`, `list` and `watch` on the kinds above and
  nothing more. It's in the repo, and the manifest the server hands out is
  that file with the token added.
- The agent runs as non-root under OpenShift's restricted-v2 SCC (or Pod
  Security `restricted`), with a read-only root file system, no
  capabilities, and resource limits.
- Records leave the cluster trimmed. A pod spec, an environment variable
  or a Secret's name in a volume never reaches obs-beacond.
- For the certificate inventory (cert-inventory.md, step 4) the agent
  tries a TLS handshake on each TLS Ingress and Route host once a day,
  as any visitor would, and keeps only the certificate it's served
  (`-cert-scan=false` stops it).
- It writes nothing, with one opt-in exception: `-cert-renew` and the
  optional `cert-renew.yaml` role (`patch` on cert-manager's
  `certificates/status`, nothing else) let an admin's "Renew now" ask
  cert-manager to renew a Certificate (cert-rotation.md, step 3).
  `/cluster.yaml?cert_renew=1` hands out both.
- `pods/log` only with `logs: true`, read on request for one container,
  and shown only to operators and admins.
- obs-beacond holds no cluster credentials. Removing a cluster on
  Configure → Clusters revokes its agent's certificate. Its namespace is
  then deleted with one `kubectl delete -f` of the same manifest.

## How it will be tested

- **The 10,000-workload test** (step 6 of scale-and-availability.md):
  `scripts/fleet cluster` plays cluster agents. It sends 3 clusters, each
  with 30 nodes, 3,000 pods under 300 controllers and 300 VMs, beside the
  1,000 host agents already there. It also plays a rollout (300 pods
  changing in a minute), a node going down with 100 pods on it, and a
  full re-list. Measures: ingest per copy, the time from a change to
  every copy showing it, page times at p95, and one incident for the
  node.
- **kind** (allowed for testing): the real agent against real
  Deployments, StatefulSets, PVCs and Jobs, and KubeVirt in emulation mode
  for VMs, on bench. This is free; it needs no nested virtualization.
- **OKD** for the OpenShift parts (ClusterOperators, an upgrade rolling
  through MachineConfigPools): only on the EX280 lab cluster, and only
  with John's go-ahead each time.
- **Proxmox** and **libvirt:** against their documented APIs with
  fakes, plus libvirt for real on bench, which has KVM. Each PR says what
  was tested against the real thing and what against a fake.

### What kind showed (step 2a, 2026-11-13)

A kind cluster on bench, with three obs-beacond copies on CloudNativePG
inside it. The cluster was added the way a customer would add one: a join
token, then `curl … /cluster.yaml | kubectl apply -f -`.

- **Joining:** the agent joined as `kind` and listed the whole cluster
  in 201 ms. Its key and certificate went into its Secret. Applying the
  manifest again without the token, with a newer image, upgraded it with
  no new join; it loaded its certificate from the Secret.
- **Judging,** with a demo namespace made to fail: a Deployment whose
  pods crash-loop (critical, from its annotation), one whose image can't
  be pulled (warning), a Job past its backoff limit and a PVC that never
  binds. Each was judged as expected and the incidents fired after their
  wait.
- **A silent agent:** with the agent scaled to 0, `cluster/kind/silent`
  opened 2 minutes after its last send and fired 2 minutes after that.
  The crash-loop incident stayed fired the whole time; it didn't resolve
  just because nothing was being heard. With the agent back (upgraded
  as above, while the three copies rolled to the fixed build), the
  silence resolved 46 seconds after the agent started. Most of that was
  the evaluator changing hands in the roll; it checks every 15 seconds.

Bugs it found, all fixed in the same PR:

- A crash-looping pod flapped between restarting and running, because
  between restarts its container is briefly running. A container that
  has just ended, with restarts, now counts as crash-looping too.
- A pod didn't carry its Deployment's `alert` annotation. Pods now take
  the controller's when they have none of their own.
- The manifest put the Secret after the Deployment. The first pod could
  start before its Secret existed. The Secret now comes first.
- A joined cluster agent showed up as a silent host on the copies that
  hadn't handled the join, after they reloaded the joined list. Reloading
  now keeps clusters out of the hosts, as joining does
  (TestJoinedClusterOnTheOtherCopy).
- A Deployment mid-rollout was briefly called degraded. Controllers now
  count as degraded or down only after 5 minutes.

### What OKD showed (step 3, 2026-11-13)

A single-node OKD 5.0.0-okd-scos.3, restored from a lab image
on one rented server for half an hour, offline as a lab session is.
obs-beacond ran in it from `deploy/kubernetes/openshift` (under the
restricted-v2 SCC, its volume on local-path), and the cluster was added
the customer's way.

- The agent joined and listed the cluster in 201 ms. The server showed
  OKD 5.0.0-okd-scos.3, 34 of 34 operators available, and the master and
  worker pools up to date.
- The lab's own fixtures can't pull their images offline: ten
  Deployments in user namespaces opened warnings (nothing adopted). The
  same trouble in `openshift-insights`, `openshift-marketplace` and
  `openshift-operators` raised nothing, as those namespaces are left to
  the operators.
- **An update:** a MachineConfig adding one file. The node rebooted
  within 20 seconds, with obs-beacond and the agent on it, and the API
  was back 7 minutes later. Cairn OBS then showed the pool "Updating: 0
  of 1 nodes done" and the node "Updating its machine config", not
  down; both were up to date 20 seconds later. No incident opened or
  closed for the cluster, its node or its operators. The agent came back
  with its certificate from its Secret, rode out a few seconds of
  cluster DNS not answering, and carried on.
- Not seen there: the agent silent mid-update with obs-beacond
  outside the cluster, which is what a real deployment has. That case
  (a warning for up to 30 minutes, then critical) is covered by
  `TestOpenShiftClusterJudged`. It depends on the last batch before the
  reboot showing the pool updating; the agent sends every 5 seconds, and
  the operator marks the pool before it reboots the node.

### What KubeVirt showed (step 4a, 2026-11-13)

KubeVirt v1.9.1 and CDI v1.66.1 on the kind cluster (Kubernetes 1.32,
one node, with KVM). The agent found both by their APIs, nothing
configured, and listed them with the rest in 200 ms.

- A Fedora 41 VM on a disk CDI imported from a registry: "Running", on
  its node, with its guest OS, address and firmware UUID, and its file
  systems from the guest agent ("/ 56% of 4.5 GiB; /boot 9% …"). Fedora's
  btrfs subvolumes (`/`, `/home`, `/var`) are one file system, so they're
  said once; link-local addresses are left out.
- A VM asking for 512 GiB: "Can't be scheduled (ErrorUnschedulable): 0/1
  nodes are available: 1 Insufficient memory.", from its PodScheduled
  condition (its Ready condition only says the guest isn't running). A
  warning, as nothing in its namespace was adopted.
- A DataVolume CDI couldn't make a claim for (the storage profile had no
  access mode): "Can't make its volume (ErrClaimNotValid)". With that
  fixed, an import from a URL that refuses: "Import or clone failing,
  restarted 2 times: Unable to connect to http data source…", a warning
  on the volume. CDI's importer pod, crash-looping meanwhile, is judged
  through the volume, not as a second alert.
- A halted VM is "Stopped" and raises nothing; one whose run strategy is
  Always, stopped, is down.
- Not seen there: a live migration. A one-node cluster has nowhere to
  move a VM (KubeVirt refuses the migration outright), so a failed one is
  covered by `TestVMRecords` and `TestVMDisksMigrationsAndDataVolumes`.
  Container disks don't boot on kind's Kubernetes 1.32 either: KubeVirt
  1.9 mounts its helper as an image volume, which 1.32 doesn't serve.

### Metrics and history (step 5, 2026-11-13)

- **Read.** Every minute the agent lists `nodes` and `pods` from
  metrics.k8s.io (the ClusterRole has `get` and `list` there, nothing
  else). Where nothing serves the API (no metrics-server, as on a bare
  kind cluster) there's no usage and no problem; a 403 is a problem on
  the cluster's record, as for any other list.
- **Summed in the cluster.** A node's use is against its allocatable. A
  controller's is the sum of its pods' (a ReplicaSet's pods count for
  its Deployment), against the sum of their limits; if any of its pods
  has no CPU (or memory) limit, it has no CPU (or memory) ceiling. A
  VM's is its virt-launcher pod's. Bare pods and Jobs' pods aren't
  summed: a pod's history is its controller's.
- **Sent beside the records, not in them.** A reading goes once, with
  the next batch (`usage`, `usage_at`; protocol.md), even when nothing
  else changed. Records aren't rewritten every minute, so the
  `workloads` table and the bus see no more traffic than before.
- **Stored.** The latest reading per cluster is one row in
  `workload_usage`, so every copy shows the same numbers; a reading over
  three minutes old isn't shown. Each copy's recorder adds each reading
  once to the hour, and at the hour's turn writes average and peak CPU
  and memory, and the ceilings, to `workload_hours` (source, workload,
  hour). A workload there is `namespace/name` or a node's name, not the
  uid, so a Deployment made again keeps its history. Kept 400 days, as
  `host_hours` is; on Postgres it's partitioned by month like the other
  history tables. Schema 14.
- **Shown.** Workloads has a Using column (cores and bytes, with the
  percentage of the ceiling where there is one, marked from 90%), and a
  row's detail has the last day's two charts from `GET
  /api/v1/workloads/hours` (`days=` up to 400). Arrays' performance and
  space are kept in the same table (docs/pure.md, step 4).
- **Upgrade obs-beacond first.** An older server ignores the new fields;
  nothing breaks, the numbers just aren't kept.
- **On kind** (no metrics-server): with the old ClusterRole the agent
  reported "can't read metrics: 403" on the cluster's record (the API
  server checks RBAC before it knows the group isn't served); with the
  new one, nothing, and no usage. The numbers themselves were checked
  against metrics.k8s.io's documented JSON in the tests, and on the
  fleet simulator's 30 nodes, 300 controllers and 300 VMs. Against a live
  metrics-server (v0.8.0 on kind, 2026-11-15): the agent picked the API
  up within a minute of it answering, and what obs-beacond stored matched
  `kubectl top` (the node 0.173 cores and 4,337 MiB against 185m and
  4,342 Mi a moment apart; the KubeVirt VM's launcher pod likewise).

### libvirt (step 6, 2026-11-13)

- **Read by the root helper**, from libvirt's read-only socket
  (`/run/libvirt/libvirt-sock-ro`, else virtqemud's), at every report.
  The helper speaks libvirt's RPC protocol itself, and knows five calls:
  the authentication list, polkit (the read-only socket's default; root
  is allowed), open read-only, every domain's stats (state, CPU time,
  balloon, vCPUs) and autostart. No other call exists in it, so it can't
  change anything, whatever the socket would allow, and it brings no
  dependency.
- **Sent in the host's report** (`vms`, protocol.md), not as a cluster
  agent's batches: a host has tens of VMs, its report already reaches
  every copy through `hosts_live`, and the host's own silence is the
  source's. obs-beacond shows them as a source of their own, named for
  the host: on Workloads (the source list has "Hosts running VMs"), and
  as "Runs 12 VMs, 10 running" on the host's page. Not on Clusters.
- **States:** running is `ok`; paused `degraded`, but by a disk I/O error,
  its watchdog or a guest crash `down`, and migrating `updating`; shut
  off `stopped`, but crashed or failed to start `down`. A VM shut off is
  no condition unless it's down; autostart is shown, not judged.
- **Alerts:** warnings only, as for a cluster's workloads nobody adopted.
  Adopting a host's VMs (as containers are) is to come.
- **Usage:** CPU from the change in CPU time between reports, against
  its vCPUs; memory as its QEMU process's RSS, against its maximum. Kept
  hourly with the clusters' (step 5).
- **Linked and grouped:** the domain UUID is the guest's SMBIOS UUID, so a
  guest running our agent is linked to its VM ("VM db-02 on kvm1"), and
  a VM needs its host: the hypervisor silent is one incident, with its
  guests' silence under it.
- **Tested** against a fake libvirtd answering from the protocol's own
  definitions (remote_protocol.x), and in the fleet simulator (a
  hypervisor in every 20 hosts, 12 VMs each, five of them fleet hosts).
  Not yet against a real libvirtd: bench has KVM but no libvirt.

### Proxmox (step 7, 2026-11-13)

- **Who reads it:** the host agent on one node, with a `proxmox:` section
  (formats.md) and an API token whose user has PVEAuditor: it can look,
  never change, and the client only has GET. It reads
  `/cluster/resources` and `/cluster/status` every 30 s, `/version`
  every half hour, and each VM's config once (50 a round, then every half
  hour) for its `smbios1` UUID.
- **The token**, on any node:

  ```
  pveum user add obs@pve --comment "Cairn OBS, read only"
  pveum user token add obs@pve beacon --privsep 1
  pveum acl modify / --tokens 'obs@pve!beacon' --roles PVEAuditor
  ```

  Put `obs@pve!beacon=<the secret it printed>` in the agent's
  `token_file`, readable by the agent only.
- **How it's sent:** as a cluster agent's batches (full listings, changes,
  heartbeat, usage), not in the host's report: a Proxmox cluster can hold
  thousands of guests. Any agent's batches are now a source under its own
  name, so the cluster shows on Clusters as "pve-prod · read by pve1",
  with its version ("Proxmox VE 8.4.1"); alerts name it "Cluster pve-prod
  (pve1)". Two nodes with `proxmox:` would be two copies of one cluster:
  set it on one.
- **Records:** the cluster itself (quorate, or "Not quorate: 1 of 3 nodes
  online", which is down: one critical condition, as a cluster that can't
  be reached is); nodes (offline is down); VMs and containers as `vm`
  records, named as Proxmox's tree names them ("100 (db)", since two may
  share a name), on their node, with the `smbios1` UUID as
  `firmware_uuid` so a guest running our agent is linked. Templates are
  left out. Storage isn't read yet.
- **States:** running `ok`, stopped `stopped`; HA overrides: `error` and
  `fence`/`recovery` are down, `migrate`/`relocate` updating, and
  `started` while stopped is starting. A migration lock is updating.
- **Usage:** from the same listing: CPU share × vCPUs, memory against
  its maximum, for nodes and running guests. Kept hourly as step 5's.
- **Tested** against a fake of the documented API (a TLS server, CA and
  pinned fingerprint both; a refused config; quorum lost; a node on its
  own), and in the fleet simulator: `-proxmox 400` makes an 8-node
  cluster read by fleet-0060, with one guest in HA error, one migrating,
  and hosts 3, 23, 43 … as its VMs. Not against a real Proxmox VE.

### vSphere (step 8, 2026-11-13)

- **Who reads it:** any host agent that can reach the vCenter, with a
  `vsphere:` section (formats.md): a user holding vCenter's built-in
  Read-only role, propagated from the top. It logs in once and keeps the
  session (logging in again when vCenter's idle timeout ends it), and
  every minute reads, through one container view, each VM's, ESXi host's
  and datastore's properties it needs, then the names of the alarms
  they've triggered. Nothing but property reads is asked.
- **How it's sent:** as Proxmox is (step 7): a cluster agent's batches
  under the agent's name; Clusters shows "vcsa-01 · read by mgmt1",
  "vCenter 8.0.3". The Proxmox loop is now one loop for every platform.
  An agent read one platform until the Pure note's first step (pure.md,
  decision 1): now each it reads is a source of its own, named in its
  batches ("mgmt1:vcsa-01").
- **Records:** the vCenter; ESXi hosts as nodes (not responding or
  disconnected is down, maintenance mode updating, standby stopped);
  VMs (powered off and suspended stopped; inaccessible, orphaned and
  invalid down; a snapshot over 7 days old degraded; VMware Tools not
  running noted); datastores as volumes (85% full degraded, 95% or not
  accessible down). Templates are left out. A name two VMs share gets
  the VM's id after it, as Proxmox's guests are named by number.
- **vCenter's alarms:** listed on each record ("Host CPU usage (red)");
  a red one nobody acknowledged makes a record that's otherwise fine
  degraded, saying which. vCenter judged it; we pass it on.
- **Linking:** a VM's BIOS UUID is its `firmware_uuid`; for vSphere VMs
  the byte-swapped order matches too (older virtual hardware), and only
  for them.
- **Usage:** a host's CPU MHz over its core speed in cores, its memory;
  a VM's CPU MHz over its host's core speed, its consumed memory.
- **Size:** govmomi makes the agent binary 16.6 MB, from 10.0 MB, on
  every host whether it reads a vCenter or not. Splitting the platform
  readers into a binary of their own is the way out, if that matters.
- **Tested** against govmomi's vCenter simulator (vcsim: CA and pinned
  certificate, a wrong pin refused, a lapsed session, a VM powered off,
  a host into maintenance, a snapshot ten days on), alarms against fixed
  states, and in the fleet simulator: `-vsphere 600` makes a vCenter of 16
  ESXi hosts read by fleet-0080, with a host in maintenance, one with a
  red alarm, a datastore 91% full, an inaccessible VM, an old snapshot,
  and hosts 7, 27, 47 … as its VMs, every other one byte-swapped. Not
  against a real vCenter.

### Nutanix (step 9, 2026-11-13)

- **Who reads it:** any host agent that can reach Prism Central, with a
  `nutanix:` section (formats.md): a service account's API key (Prism
  Central 2024.3 and later) or a user and password, holding the built-in
  Prism Viewer role over every cluster. Every minute it reads, through
  Prism Central's v4 API (the v4.0 paths, which every Prism Central since
  pc.2024.1 answers), all VMs, clusters, hosts and storage containers, a
  hundred a page, and the alerts not yet resolved. Only GET is asked.
- **How it's sent:** as Proxmox and vSphere are: a cluster agent's
  batches under the agent's name; Clusters shows "pc-01 · read by mgmt1",
  "Prism Central pc.2024.3.1". One Prism Central covers many Prism
  Element clusters: they're listed on its record, and each host, VM and
  container says which it's in. A cluster Prism Central says isn't
  available makes its record degraded, naming it.
- **Records:** AHV hosts as nodes (disconnected is down; maintenance mode
  and being removed updating; HA moving VMs off it down; marked degraded
  degraded); VMs (off and paused stopped; guest tools installed but not
  answering noted); storage containers as volumes, without Nutanix's own
  (marked for removal updating). A name two VMs share gets the VM's id
  after it.
- **Prism's alerts:** an open alert is listed on the record it's about,
  its source or one it affects ("Disk Failure (critical)"); one about a
  cluster goes on Prism Central's, with the cluster's name. A critical
  one nobody acknowledged makes a record that's otherwise fine degraded,
  saying which: so a container filling up shows through Prism's own
  space alert, as v4 keeps used space with the stats.
- **Linking:** a VM's `biosUuid` is its `firmware_uuid`.
- **Usage:** not read. v4 keeps it in a separate stats API, asked per
  entity over a time range; reading it for thousands of VMs is its own
  piece of work, for later.
- **Size:** no SDK, only HTTP and JSON: the agent binary doesn't grow.
- **Tested** against a fake of the documented API only, as agreed (John,
  2026-11-13; no Community Edition run): field names from Nutanix's
  published v4 Go client models; paging (150 VMs over two pages), basic
  auth and API key, a CA and a pinned certificate, a wrong pin and an
  unknown CA refused, a wrong password, a 403 on alerts (a problem on
  Prism Central's record) and on VMs (the read fails), and in the fleet
  simulator: `-nutanix 450` makes Prism Central pc-01 with three clusters
  of six AHV hosts read by fleet-0100, with a host in maintenance, one
  with a critical alert, a container over its space threshold, and hosts
  11, 31, 51 … as its VMs. Not against a real Prism Central.

## Plan

One PR each, in order:

1. **Workloads in the server** (done): the record, `POST
   /api/v1/agent/workloads`, the `workloads` table (both back ends) and
   the bus notice, judging and grouping, the Workloads and Clusters pages,
   and fake cluster agents in `scripts/fleet` (`config -clusters`). Then
   the 10,000-workload test, with its results in
   scale-and-availability.md. Cluster agents are in obs-beacond.yaml
   (`cluster: true`) until step 2.
2. **The cluster agent for Kubernetes**, in two PRs:
   - **2a** (done): the watches, trimming, re-listing, the manifest, Add
     a cluster (joining, as hosts do), Configure → Clusters, tested on
     kind. Not yet: Warning events as incident detail, and pod logs
     (`logs: true`), which come with the incident pages.
   - **2b** (done): adopting namespaces, controllers and VMs on
     Configure → Clusters → a cluster, kept in expected.yaml's
     `clusters:` with what should run on hosts; missing adopted
     workloads reported.
3. **OpenShift and OKD** (done): ClusterVersion, ClusterOperators,
   MachineConfigPools, and Updating instead of down; tested on OKD
   (What OKD showed, above).
4. **KubeVirt VMs**, in two PRs:
   - **4a** (done): VMs, VMIs, migrations, DataVolumes and guest file
     systems (KubeVirt v1.9, CDI v1.66, tested on kind; What KubeVirt
     showed, below).
   - **4b** (done): linking a VM to its agent by SMBIOS UUID, both ways
     on the pages, and a VM that's down, or on a node that's down,
     explaining its host going silent: one incident. Checked on kind: the
     Fedora guest's `product_uuid` is its VMI's firmware UUID, character
     for character.
5. **Metrics and history** (done): CPU and memory from metrics.k8s.io,
   now and hourly, for nodes, controllers and VMs (Metrics and history,
   below).
6. **libvirt** in the host agent (done): the host's domains as `vm`
   records with the host as their source (libvirt, below).
7. **Proxmox** (done): a node's agent sends its Proxmox cluster as a
   cluster agent would (Proxmox, below).
8. **VMware vSphere** (done; vSphere, below), through vCenter's API with a read-only role:
   VMs (power state, VMware Tools, snapshot age, which ESXi host), ESXi
   hosts (connected, in maintenance), datastores (how full) and
   vCenter's own triggered alarms, sent as the same `vm` records with
   the vCenter as source. A VM's BIOS UUID is the SMBIOS UUID the host
   agent reports; on older VM hardware the guest sees its first three
   fields byte-swapped, so both orders match. Written with govmomi
   (VMware's Go SDK, Apache 2.0) and tested against its vCenter
   simulator, `vcsim`; a real vCenter only if John wants a run on one
   (it needs a paid licence or a 60-day trial).
9. **Nutanix** (done; Nutanix, above), through Prism Central's v4 API, read-only: VMs, hosts,
   storage containers and Prism's alerts, as `vm` records. Tested
   against fakes of the documented API only (John, 2026-11-13): no
   Community Edition run.

VMware comes first because it's in far more enterprises, and many Red
Hat shops are moving from it to OpenShift Virtualization: watching both
sides during that move is the point (John, 2026-11-13). Neither being
closed source matters, as only their documented APIs are read.

Pure, Vault, the certificate inventory and Akamai come after, in that
order, as already agreed.

## Decisions

2026-11-13, John went with the recommendation on each:

1. **A cluster agent in each cluster**, including the one obs-beacond runs
   on. There's one code path; obs-beacond never reads a cluster's API.
2. **Pod logs are an opt-in per cluster** (`logs: true`), read on request
   for one failing container and shown only to operators and admins.
3. **No history for each pod**, only for its controller. Nodes, VMs and
   controllers keep hourly numbers for 400 days, as hosts do.
4. **Only cluster-level trouble pages** until workloads are adopted or
   annotated. Everything else is a warning on the pages.
5. **The annotation is `obs.cairnobs.org/alert`.**
