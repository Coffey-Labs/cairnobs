# The maps: design note

Status: decided 2026-11-13 (Decisions, at the end); being built in the
order under Plan. Steps 2 to 5 (the agent's side, the graph in the
server and its API, both map pages and corrections) are done; grouping
from learned edges (6) is in review. Kubernetes edges (7) go alongside
the cluster watcher's KubeVirt step.

Today both maps come from topology.yaml, which someone writes by hand:
the hosts and how they're meshed, every service with its tier, what it
needs and what it's made of. That works for four hosts. It doesn't work
for a thousand hosts and ten thousand workloads, and it goes stale the
day someone adds a container and forgets the file. formats.md says Cairn
OBS "never guesses the layout from traffic". This note reverses that:
**the maps draw themselves** from what the agents and cluster agents
already see, plus a little more. topology.yaml becomes optional. It holds
only corrections and things Cairn OBS can't see.

It also covers drawing the maps at that size. The current pages draw
everything at once, and that stops being readable at about forty boxes.

## What we're aiming for

- **Nothing to write by hand.** A host that joins, a compose project that
  starts, a cluster that's added: each shows up on the right map within a
  few reports, in the right place, with what it talks to.
- **Corrections, not configuration.** People can rename, hide, merge,
  confirm or reject what was found, on the page. Those edits are kept,
  and later discovery never undoes them.
- **Thousands of things, still readable.** 1,000 agents and 10,000
  workloads, the targets from scale-and-availability.md. A view never
  draws more than about 200 boxes; past that, things are grouped and
  you open the group. Map requests under 300 ms at p95, like the other
  pages.
- **Root-cause grouping keeps working**, using learned dependencies. A
  wrong guess must never stop something paging.
- **Small and read-only.** No packet capture, no eBPF, no payloads, no
  command lines. One sample of the socket table per report.

## Not in this note

- **Flow volumes and bandwidth between services.** Edges say "this talks
  to that, on this port, this often", not bytes. Interface traffic per
  host stays as it is.
- **Pods on the network map.** It draws clusters and nodes, as
  cluster-watcher.md already says. Pods are on the Workloads page and,
  through their controllers, on the service map.
- **Windows and IBM i.** The same report fields fit them
  (`GetExtendedTcpTable` gives owners on Windows). They come with those
  agents.

## What's seen, and where it comes from

### For the network map

| What | Where it comes from |
| --- | --- |
| Hosts | Every joined agent, as now. |
| Networks and sites | Each agent reports its interfaces: name, kind (physical, bridge, VLAN, WireGuard, …), addresses with prefixes, and the default gateway. Hosts that share a private subnet (RFC 1918, ULA or CGNAT) are on one network. A **site** is the hosts joined by private networks, so a provider's private network becomes one site with no setup. Container bridges and Docker's own networks don't count. |
| WireGuard links | The root helper reads `wg show all dump`: each interface's public key, and each peer's public key, endpoint, allowed IPs, last handshake and byte counters. obs-beacond matches a peer's key to another host's interface key, so a mesh link is a fact, not a guess, and is "up" while the handshake is under three minutes old. A peer that isn't one of our hosts is an outside peer (someone's laptop, another office), counted on the host and listed in its detail. |
| Other links between hosts | The socket sample (below): host A has connections to host B. Drawn as a plain line with its ports, separately from WireGuard. |
| The way in from the Internet | Listeners on public addresses that have connections from public addresses: "Internet → node2 :443, :25". With Cloudflare set up, the Cloudflare box and its pools come from Cloudflare's API as today. |
| Clusters | The cluster agents. A cluster is a box of its nodes. A node is linked to its host agent, when it has one, by machine ID or SMBIOS UUID (as VMs are in cluster-watcher.md), and by name and address otherwise. |
| Remote hosts | An agent that shares no private network with a site but has a WireGuard link to one of its hosts (a desk machine at home, say) is drawn off-site with that link, as `remote:` does by hand now. |

### For the service map

A **service** is a named group of parts. Discovery finds them like this,
first match wins:

1. **A compose project** is a service; its containers are the parts.
2. **Kubernetes:** controllers with the same `app.kubernetes.io/part-of`
   label are one service. Without it, `app.kubernetes.io/name` or
   `app.kubernetes.io/instance`; without those, each controller is its own
   service, grouped under its namespace. A KubeVirt VM is a part, like a
   controller.
3. **A systemd unit that listens on a port** (postgresql, patroni, haproxy,
   nginx, a Go binary under its own unit) is a service of its own, named
   after the unit. The same unit on several hosts is one service with a
   part on each: patroni on node1 to node3 is "patroni", three parts.
4. **A container outside compose** is grouped with others from the same
   image name.

What a service is (PostgreSQL, Redis, nginx, …) is guessed from its
image name, unit name and listening ports. That sets its icon and helps
place it, nothing else.

**What a service needs** comes from the socket sample: a part that has a
connection to a port that another service's part listens on needs that
service. In Kubernetes, the API says it outright: an Ingress or Route
points at a Service, and a Service's selector picks the controller. Those
edges are declared, not learned.

### The socket sample

At every report the root helper reads the TCP and UDP tables (`/proc/net/tcp{,6}`, `udp{,6}`) in the host's namespace and in
each container's, and works out who owns each socket: socket inode →
process (`/proc/<pid>/fd`) → its cgroup → a container ID or a systemd unit.
Only listening sockets and established connections are kept. The agent
then shrinks it before sending:

- **Listeners:** owner, protocol, address and port.
- **Outgoing connections**, to ports that aren't in the ephemeral range or
  that a listener on the far side is known to own: owner, remote address,
  remote port, and how many.
- **Incoming from private addresses:** owner, local port, the remote
  address, and how many.
- **Incoming from public addresses:** owner, local port and a count. **No
  public client address leaves the host.** People's IP addresses from the
  Internet are never stored anywhere.

The agent sends it once a minute, merged: every listener seen and each
connection group at its busiest. A busy host sends a few hundred lines at
most; it's capped at 500 of each and says so. In the fleet simulator's
shape the sample adds about 8% to the average report. Connections that open and close between two samples are
missed, but anything that happens regularly turns up within an hour, and
that's what a dependency map needs.

## Learned edges: when to believe them

An edge has a source: `declared` (topology.yaml, a Kubernetes object, or
confirmed on the page) or `learned` (the socket sample). Learned edges go
through three stages:

- **Seen:** drawn at once, thin and dashed, so a new thing shows up on the
  map right away.
- **Steady:** seen in at least three separate hours within seven days.
  Drawn solid. Only declared and steady edges are used for root-cause
  grouping.
- **Fading:** not seen for 14 days. Drawn faint, then dropped after 30
  days unless declared.

Grouping keeps today's safety rules (formats.md, Root-cause grouping):
the group takes its worst severity, so a critical symptom still pages; an
alert already sent stays on its own. Two additions:

- An incident grouped by a learned edge says so: "Grouped because
  webmail has talked to postgresql :5432 since Nov 2." A wrong guess is
  there in the open.
- **Not a dependency**, on the incident and on the map, rejects that edge.
  It's kept as a correction and never relearned.

## Corrections

Everything discovered can be corrected on the map, by admins. The edits
are kept with the other settings edited in the UI:

- **Rename** a service, network or site ("Office LAN" rather than
  "10.20.0.0/24").
- **Merge** two services into one, or **split** a part out of one.
- **Hide** a service or a host from the maps, or a whole namespace.
- **Confirm** a learned edge (it becomes declared) or **reject** one.
- **Add** what can't be seen: an outside service (a payment provider, a
  mail relay at another company), an edge to it, a note.
- **Pin** a box's position, if the layout's choice is wrong.

Corrections key on stable names (the compose project, the unit, the
controller's namespace and name, the interface public key), so they
survive restarts, redeploys and pods being replaced.

As built (step 5), they're the `maps` section of the settings kept in
the database, reloaded by every copy when one saves, and exported like
the others (`/api/v1/settings/maps/export`). Without a database they
can't be kept, so the page doesn't offer them. Each change is one
`POST /api/v1/map/corrections` (admins), and `GET` lists them in words,
each with the change that undoes it:

| Change | What it keys on |
| --- | --- |
| Rename | a service id (`compose:mail`) or a site id (`site:10.0.0.0/24`) |
| Tier | a service id: edge, apps or data |
| Hide | a service id, or `host:<name>` (the host leaves both maps) |
| Merge | a service id, into another service id |
| Move a part | the part's host and owner (`node1 container:mail-1`), into a service, or into a new one by name (`page:<name>`); this is how a part is split out |
| Confirm, reject | from, to and port (port 0: every port) |
| Outside service | an id `outside:<name>`, with addresses (`203.0.113.10`, `…:443` or a prefix) and what needs it |

A rejected edge is never drawn and, from step 6, never groups alerts;
the learned row stays in `map_edges` and ages out as usual, so undoing
the rejection brings it straight back. Pinning a box's place waits until
positions are kept on the server; for now each browser keeps its own
order.

**topology.yaml stays, as the file form of the same corrections.** A
current file keeps working: its services become declared services with
their parts and `needs`, its hosts' names and roles apply to the
discovered hosts, and its mesh details are checked against what's found
(a difference is shown, not silently taken). People who keep their setup
in git can keep doing that.

## Drawing it at scale

### Levels

Both maps open on an overview and let you go in, instead of drawing
everything.

**Network map:**

1. **Overview:** the Internet and edge (Cloudflare when set up), each site,
   each cluster, the remote hosts, and the links between them. One box per
   site or cluster, with its counts and its worst state.
2. **A site or a cluster:** its hosts or nodes and the links between them.
   Up to 60 are drawn as cards; past that they're grouped, by role first
   (from corrections), then by name pattern (`web-01` to `web-40`), then by
   subnet, and a group opens like a site does.
3. **A host:** it in the middle, everything it has links or connections to
   around it, its interfaces and WireGuard peers, and a link to its page.

**Service map:**

1. **Overview:** every service, in tiers. Past 150 services, they're
   grouped by namespace, compose project's host or cluster, with problems
   kept visible (below).
2. **A service, focused:** what it needs, all the way down, and what needs
   it, all the way up, to three steps each way by default. This is
   today's hover lighting, made into a view that can be linked to. It's
   also where the incident page sends you.

On every level: a search box that goes straight to any host, service or
workload; **problems first**, so a group in trouble takes the worst colour
of what's in it and lists its failing members on the box; and the URL
holds the level and focus, so a view can be shared and reloaded.

### Tiers and layout

Tiers are worked out, not written. Things that take traffic from the
Internet or an Ingress are the edge; things that only get connected to
are the data layer; everything else sits between them in layers by the
longest path down. Hosts and nodes stay as the bottom row of the service
map. A correction can move a service to another tier.

Layout is our own, in the browser, on the already-grouped graph: layers
from the tiers, then order within each layer to cut crossings (a few
passes of the barycentre method), then positions. With at most ~200
boxes it takes milliseconds and needs no layout library. **Positions are
kept** per view, so a new service slots in without everything else
moving, which matters when someone looks at the map every day. For now
the order is kept in the browser (local storage, per view); keeping it
on the server, shared by everyone, can follow if browsers prove not
enough.

Drawing stays SVG, as now. The 200-box cap is what makes that fine; no
canvas or WebGL.

### The server's part

- Each copy builds the graph from the hosts' latest map samples, which
  it already has (reports go round on the bus as before), at most every
  10 seconds. At 1,000 hosts that takes a few milliseconds.
- Learned edges are stored in a `map_edges` table (source, target, port,
  first seen, last seen, the hours seen in, when it became steady), both
  back ends. Only the evaluator writes, at most once an hour per edge;
  every copy rereads the table each minute. Edges are between services,
  not sockets, so it stays small: tens of thousands of rows at the target
  size. Edges not seen for 30 days are dropped once a day.
- The API returns one level at a time, already grouped:
  `GET /api/v1/map/network` (focus `site:<prefix>`, `group:<id>`,
  `host:<name>`) and `GET /api/v1/map/services` (focus `group:<id>`,
  `svc:<id>` with `steps`, default 3). Each answer is boxes, lines between
  them and the crumbs back up. A response is capped at 300 boxes and 1,000
  lines; the server groups further rather than sending more.
  `/api/v1/topology` (the hand-drawn maps' state) went with them in step 8.
- What's plumbing on every host (sshd, the resolver, Docker's proxy, the
  agent itself) is never a service, and connections to it aren't
  dependencies. A service's colour is the worst of its parts: a
  container's verdict, or the host of a unit or process.

## Security and privacy

- The new collection runs in the **root helper** that already reads Docker
  for the agent: it needs to read other processes' `/proc/<pid>/fd` and
  WireGuard state. The agent itself gains no privileges. The helper now
  runs on hosts without Docker too, in the host's network namespace,
  limited to `CAP_CHOWN`, `CAP_DAC_READ_SEARCH`, `CAP_SYS_PTRACE` and
  `CAP_NET_ADMIN`, with only unix and netlink sockets. The kernel's
  WireGuard answer includes the private key; the helper wipes it where it
  lies and never reads it out.
- What's sent is addresses, ports, owners (container name, compose
  project, unit name) and counts. No payloads, no process command lines,
  no environment, no public client addresses.
- **`map: false`** in agent.yaml turns off the socket sample and the
  WireGuard read on that host. The host is still drawn, from its
  interfaces.
- The maps are seen by the same roles that see hosts now. Only admins
  correct them.
- Cluster agents gain `get`, `list` and `watch` on Services,
  EndpointSlices, Ingresses and (on OpenShift) Routes. Still read-only,
  still no Secrets or ConfigMaps.

## How it will be tested

- **Unit tests** for each step: interfaces to networks and sites, peer
  keys to links, sockets to owners (from fixture `/proc` trees, as
  procfs_test.go does), discovery rules, the steady/fading stages,
  corrections surviving a re-discovery, grouping only on steady edges.
- **The fleet simulator** gains interfaces, WireGuard peers and
  connections for its fake hosts, shaped like real sites (web tier →
  apps → databases, a few hundred services), and its cluster agents gain
  Services and Ingresses. At 1,000 agents and 10,000 workloads: map
  responses under 300 ms at p95, report size up no more than 10%,
  `map_edges` size after a simulated week. Results go in
  scale-and-availability.md.
- **kind and OKD** for the Kubernetes edges, as before (OKD only with
  John's OK each time).
- **Our own hosts, side by side:** discovery runs next to the hand-written
  topology.yaml on prod, and the two are compared box by box and line by
  line before the hand-written file shrinks to corrections. Prod changes
  only with John's go.

## Plan

One PR each, in order:

1. **This note.**
2. **The agent's side:** interfaces, WireGuard peers and the socket sample
   in the root helper, owners attributed, `map: false`, the report field
   (old servers ignore it). The fleet simulator sends them.
3. **The graph in the server:** networks and sites, links, services found
   by the discovery rules, learned edges with their stages, the
   `map_edges` table, the level-at-a-time API. topology.yaml read as
   corrections.
4. **The network map at scale:** levels, groups, search, problems first,
   kept positions. Search is `GET /api/v1/map/find?q=`: hosts, sites,
   groups and services anywhere, exact names first, at most 20, each
   with the map and focus to open it on. The view sits in the address
   (`?focus=`), so it can be linked to. Where topology.yaml is set, the
   hand-drawn map stays on a second tab until step 8.
5. **The service map at scale:** discovered services in worked-out tiers,
   the focused view, a "Found since you last looked" list (new services
   and edges, to confirm or hide), and corrections on the page. When
   each service was first seen is kept in `map_services` (written by
   the evaluator beside the edges); `GET /api/v1/map/found?since=` lists
   services and learned edges first seen since then, at most a week
   back. "Since you last looked" is per browser for now. Layers are
   ordered by the barycentre method the first time a view is drawn, then
   kept. The network map gains renaming a site and hiding a host.
6. **Grouping from learned edges:** steady edges only, the incident
   wording, Not a dependency. As built: one dependency graph from
   topology.yaml's needs and the map's declared and steady edges (a
   declared edge wins over a learned one between the same two); a
   discovered service on one host depends on that host; reachability is
   worked out only for the nodes failing now (about 1 ms at 1,000
   hosts). The edges that grouped an incident, and its place on the
   maps, are stored with it (the incidents table's `grouping` column),
   so every copy shows them. Moving SQLite to PostgreSQL now copies
   `map_edges` and `map_services` too.
7. **Kubernetes edges:** Services, EndpointSlices, Ingresses, Routes and
   pod addresses in the cluster agent; controllers as service parts; kind
   and OKD. As built: the cluster agent sends a `service` record for
   each Service (its addresses, ports and the controllers or VMs behind
   its ready endpoints) and an `ingress` record for each Ingress or
   Route (its hosts and the Services it sends to). On the service map a
   cluster's controllers and VMs are parts, grouped by the discovery
   rules above; a Service's ports go on what's behind it; each cluster
   has one ingress box, with declared edges to the services its
   Ingresses and Routes reach. A host's connection to a Service's cluster
   or external address is a learned edge to what's behind it. Left off:
   namespaces where the platform runs itself (`kube-*`, `openshift-*`,
   `openshift`), CronJobs, and pod addresses (pods come and go, and a
   connection straight to a pod is rare outside the cluster). Services
   and ingresses aren't judged; what they send to is. A part links to
   its workload rather than to a host. On the overview each cluster is
   one box, opening into its services; they aren't listed as found,
   since the API says they're there. On kind (2026-11-13), with the
   ClusterRole before this step, the agent logged a 403 for each new
   kind and carried on; with it, Routes were a quiet 404. Ten Services
   and two Ingresses came through right: ready counts, a crash-looping
   Deployment's Service as "No ready endpoints", a KubeVirt VM's Service
   traced through its launcher pod to the VM, UDP ports marked,
   ExternalName and selectorless Services worded, a named Ingress port
   kept. obs-beacond now drops a record
   kind it doesn't know instead of refusing the batch (protocol.md).
8. **Our own hosts:** side-by-side on prod, then the hand-written file cut
   down to corrections. So far: prod moved to Cairn OBS (0.1.0-rc.2,
   2026-11-14) and the two maps compared. Every declared service and
   host was found with its parts; the network map matched the drawn one.
   Three discovery mistakes, all from sockets in network namespaces
   other than the host's: a container's own listener (Gitea's Postgres
   on 5432) taken for the host's (Patroni), so "Patroni needs Git"; a
   container's own loopback (Discourse to its Redis) taken for the
   host's; and a service in a namespace of its own with a public address
   (inbuxa's mail namespace) reaching the host's mesh address counted as
   the Internet coming in. Listeners and connections now say when
   they're `inside` a namespace of their own, the server matches only
   the host's own listeners to its addresses, and a connection whose
   both ends are on the host is counted once, going out (0.1.0-rc.3, on
   prod the same day; the wrong lines stopped). Then the hand-drawn maps
   went: no second tab, no `/api/v1/topology`, and topology.yaml keeps
   only corrections (site name, roles, declared services with their
   parts, tier, needs and checks; formats.md). Ours lost its drawing
   fields and gained what the comparison showed missing: two web
   containers under Websites and the image builders under CI runners.

Between this and the cluster watcher's step 4 (KubeVirt), the order is
John's call (question 6).

## Decisions

2026-11-13, John went with the recommendation on each:

1. **Discovery is on by default**, with `map: false` per agent to turn it
   off.
2. **Learned edges group alerts only once steady** (three separate hours
   in seven days), under grouping's existing rules, so nothing stops
   paging.
3. **No public client addresses leave the host**; incoming from the
   Internet is a count per port.
4. **The cluster agent reads Services, EndpointSlices, Ingresses and
   Routes**, read-only, no Secrets.
5. **Our own layout and SVG**, no layout library, at most ~200 boxes per
   view.
6. **Order:** maps steps 2 to 6 come before the cluster watcher's KubeVirt
   step; step 7 here goes alongside it.
