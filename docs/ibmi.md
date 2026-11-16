# IBM i: design note

Status: decided 2026-11-14 (Decisions, at the end); all four steps
built. Not yet tried against a real partition or HMC (decision 5).

John, 2026-11-12: "is there anything we can do for iseries?" Yes. The
shops Cairn OBS is aimed at often still run their core business on IBM i
(AS/400, iSeries) partitions on Power servers, next to the Linux, Windows
and OpenShift estate. Today those partitions are invisible to Cairn OBS.

## What there is now

Nothing for IBM i. An agent on it, as on Windows, is a long way off:
Go's AIX port might run in PASE (IBM i's AIX runtime), but nobody ships
Go programs there that we know of, much of what matters isn't visible
from PASE, and installing software on a production IBM i partition is a
change most shops make slowly, if at all.

What IBM i does have is **SQL services**: IBM ships views and table
functions in QSYS2 and SYSTOOLS that answer, in SQL, everything an
operator looks at on a green screen (WRKSYSSTS, WRKACTJOB, WRKSBS,
DSPMSG QSYSOPR, WRKDSKSTS, WRKJOBQ). Every supported release has them
(7.4, 7.5, 7.6), they cost nothing, and they're how IBM i monitoring is
done now. So the plan is the same as Pure's and Nutanix's: **a host agent
nearby reads each partition over the network, read-only**, and the
partition is a source on the same pages as everything else.

## What's read

Every minute, one query per line, all SELECTs:

| SQL service | Becomes |
|---|---|
| `QSYS2.SYSTEM_STATUS_INFO_BASIC`: CPU use, system ASP % used, jobs, temporary storage, restricted state, the attention light; `SYSIBMADM.ENV_SYS_INFO` and `QSYS2.GROUP_PTF_INFO`: the release and the Technology Refresh | the partition's own record: "PROD1 · read by mgmt1", "IBM i 7.5 TR6", CPU and storage graphs |
| `QSYS2.ASP_INFO` and `SYSDISKSTAT`: each ASP's state and % used, each disk unit's status | parts: a failed ASP is down, one with disk units missing or overflowing into the system ASP is degraded, a disk unit in trouble (failed, not ready, parity being rebuilt, mirroring suspended, an SSD's predictive failure warning) makes its ASP degraded, named on it; ASP % used warns at the ASP's own threshold (IBM i's, not ours). An independent ASP varied off is said, not judged: PowerHA keeps it so on the node that isn't using it |
| `QSYS2.SUBSYSTEM_INFO`: each subsystem and whether it's active | parts, as units are on Linux: `expected.yaml` names the subsystems that must be active (`subsystems: [QINTER, QBATCH, ORDERS]`), and obs-beacond judges them; the rest are counted, not alerted on |
| `QSYS2.MESSAGE_QUEUE_INFO` for QSYSOPR: inquiry messages of the last 30 days with no reply, and messages of severity 80 and up of the last day | parts, named for the message and the job that sent it ("Unanswered inquiry CPA0701 from 418276/BATCH/NIGHTLY"): an unanswered inquiry is a job in MSGW (decision 2) and warns; a message of severity 90 and up is critical, 80 to 89 warns. At most 25 of each; the partition's own record counts them all |
| `QSYS2.JOB_QUEUE_INFO`: queues with jobs ready to run | parts, only for a queue that's held, or that no active subsystem takes from: its jobs can't start; warns |
| `QSYS2.NETSTAT_INTERFACE_INFO`: interfaces and their state | parts: an interface set to start with TCP/IP (IBM i's own `AUTOSTART`) that isn't active is degraded, or down when its line failed (RCYPND, RCYCNL, FAILED); one not set to start is said, not judged |

What the partition says about itself it judges itself, as Purity and
vCenter do: IBM i's ASP threshold, QSYSOPR's severities. Cairn OBS adds
only what's expected to be running (subsystems) and what's been stuck too
long (MSGW, waiting jobs).

Later, when asked: the history log (`HISTORY_LOG_INFO`), PTF currency
(`GROUP_PTF_CURRENCY`, which asks IBM, so off by default), expiring
certificates in the Digital Certificate Manager store (needs more
authority than the rest). The Power servers themselves are read through
the HMC (below).

## How to reach it

Three ways, in the order I'd choose:

**Mapepire** (recommended). IBM's own database server for IBM i (2024,
open source; its licence to be checked before we build against it),
made for clients that aren't Java: SQL over a secure
WebSocket on one port (8076), results as JSON. The partition's admin
installs it from IBM's open-source package repository (`yum install
mapepire-server`) and starts it with Service Commander. The agent needs
no new dependency (golang.org/x/net, already in the module, has a
WebSocket client) and no driver; one TLS port through the firewall. The
cost: something new installed on the IBM i, and Java running for it.

**SSH and db2util.** Many shops already run OpenSSH on IBM i. The agent
logs in with a key and runs `db2util -o json "<query>"` for each read.
Nothing new to listen on, but SSH adds golang.org/x/crypto/ssh to the
agent, a shell for the profile, and a process per query.

**The host servers' own protocol** (the database host server on 8471,
which ODBC and JDBC use). Nothing to install on the IBM i, but the
protocol is IBM's own and unpublished outside the Java toolbox; writing
it in Go is weeks, and IBM's ODBC driver for Linux is proprietary and
needs cgo. Not worth it.

## The profile

A user profile just for this: user class *USER, no special authorities
to start with, password or key in a file on the agent's host (like
Pure's token), with `LMTCPB(*YES)` and `INLMNU(*SIGNOFF)` so a
green-screen sign-on gets no command line and no menu.
Most of the views above work for any user. Seeing other users' jobs in
ACTIVE_JOB_INFO (for MSGW) needs *JOBCTL, which is also the authority
to end and hold jobs: more than we want to hold. Question 2.

## Setting it up

On the partition, as a security officer:

1. **Mapepire**, from IBM's open-source packages (ACS's Open Source
   Package Management, or `yum install mapepire-server` in an SSH
   session), started with Service Commander: `sc start mapepire`, and
   `sc` again after an IPL unless it's set to start with TCP/IP. It
   listens on 8076 with a certificate it makes itself; the agent pins
   that with `fingerprint`:
   `openssl s_client -connect prod1:8076 </dev/null | openssl x509 -noout -fingerprint -sha256`.
2. **The profile**, with no special authorities and no command line:

   ```
   CRTUSRPRF USRPRF(CAIRNOBS) PASSWORD(...) USRCLS(*USER) SPCAUT(*NONE)
             LMTCPB(*YES) INLMNU(*SIGNOFF) TEXT('Cairn OBS, read only')
   ```

   Its password goes alone in the agent's `password_file`. If the
   shop's passwords expire, so does this one, and the partition then
   shows as not answering until it's changed: `PWDEXPITV(*NOMAX)` is the
   shop's call.
3. **Mapepire's own rule** for the profile, so it's read only whatever
   the agent asks, in `/QOpenSys/etc/mapepire/iprules.conf` (the last
   rule that matches wins):

   ```
   deny cairnobs@*
   allowbasiconly cairnobs@10.0.4.21
   ```

   with the agent host's address.
4. **Optional:** `*USE` on two objects names the partition as the HMC
   does and gives its Technology Refresh. Without them the partition is
   named by its host name and its version is just the release. The
   profile sees the subsystems whose descriptions it may use: IBM's
   usually are public, a shop's own may need `GRTOBJAUT ...
   OBJTYPE(*SBSD) AUT(*USE)` before it can be expected.

   ```
   GRTOBJAUT OBJ(QSYS/QPMLPMGT) OBJTYPE(*SRVPGM) USER(CAIRNOBS) AUT(*USE)
   GRTOBJAUT OBJ(QSYS/WRKPTFGRP) OBJTYPE(*CMD) USER(CAIRNOBS) AUT(*USE)
   ```

Then on the agent's host, in agent.yaml (formats.md):

```yaml
ibmi:
  - address: prod1.example.com
    user: CAIRNOBS
    password_file: /etc/obs-beacon/prod1.password
    fingerprint: "AB:CD:..."
```

The partition shows on Clusters as "PROD1 · read by mgmt1", once its
first read is in; its CPU from the second.

## The Power servers: the HMC

IBM i (and AIX, and Linux on Power) partitions run on Power servers
managed by an HMC (Hardware Management Console). Its REST API is to
Power what vCenter is to VMware, and it's read the same way
(cluster-watcher.md): a host agent with an `hmc:` section reads it every
minute, and the HMC is a source of its own on Clusters, "HMC hmc1 · read
by mgmt1".

**What's read**, all GETs with `group=None` (which leaves out the large
storage and network mappings): the console's name, every managed system,
and each one's partitions and Virtual I/O Servers. The one call that
isn't a GET is the logon, a PUT of the user and password that the API
answers with a session. The agent keeps that session between reads, as
an HMC limits how many there are, logs on again when the HMC says it
lapsed, and logs off when it stops.

| HMC | Becomes |
|---|---|
| the console (`ManagementConsole`) | the source's own record: "API answering", or what couldn't be read |
| each managed system (`ManagedSystem`): its state, model and serial, processor units and memory installed and free, its attention light | a node, a "Server": down when the HMC has no connection to it or it's in error; degraded when its attention light is on (firmware turns it on with a serviceable event open), when the HMC can't sign in to its service processor, or can't read all of it; stopped in standby or powered off |
| each partition and VIOS (`LogicalPartition`, `VirtualIOServer`): state, type, OS version, reference code, processors and memory, RMC state, attention light | a VM on the platform pages, on its server, with what runs in it (IBM i, AIX, Linux, VIOS): down in error (with its reference code) or stopped in Open Firmware; stopped when not activated; updating while it starts, stops or moves (Live Partition Mobility). A VIOS is degraded when it isn't activated on a running server, or when the HMC can't reach it over RMC: the partitions it serves have one path less, and moving them needs it |

What the HMC says it judges itself, as vCenter does. Usage isn't read:
the HMC keeps it in its performance and capacity monitoring, which has to
be turned on for each server; for later, with the serviceable events'
own text.

An IBM i partition also read through Mapepire finds its server here: the
partition's own record has the server's serial and its partition number,
the HMC's has the same two, and Clusters names the server on the
partition's card, linked to the partition on the HMC. If the partition's
number can't be read, the link isn't made.

### Setting it up

On the HMC, as an administrator (hscroot or one like it): a user just
for this, with the built-in **hmcviewer** task role (it can look at
everything and change nothing) and remote access through the web
allowed, which the REST API needs. In the HMC's web console: Users and
Security, Users and Roles, Manage User Profiles and Access; add the
user, choose hmcviewer, and under User Properties allow remote access via
the web. Its password goes alone in the agent's `password_file`.

The REST API listens on 12443 with the HMC's own certificate; the agent
pins a self-signed one with `fingerprint`:
`openssl s_client -connect hmc1:12443 </dev/null | openssl x509 -noout -fingerprint -sha256`.

```yaml
hmc:
  - url: https://hmc1.example.com
    username: cairnobs
    password_file: /etc/obs-beacon/hmc1.password
    fingerprint: "AB:CD:..."
```

Where two HMCs manage the same servers (a redundant pair), read one:
both would list the same servers and partitions.

## Plan

1. **The reader and the partition record**: the Mapepire client (connect,
   sign in, run a SELECT, read the rows), `ibmi:` in agent.yaml (a list,
   as `pure:` is: name, address, user, password file, CA), the partition
   as a source, SYSTEM_STATUS_INFO_BASIC, ENV_SYS_INFO, GROUP_PTF_INFO and
   ASP_INFO.
2. **Subsystems, disks, interfaces**, with `subsystems:` in
   expected.yaml.
3. **Jobs and messages**: MSGW, QSYSOPR, job queues.
4. **The HMC**: its Power servers and all their partitions, IBM i, AIX,
   Linux and VIOS; and an IBM i partition linked to its server.

Tested against a fake Mapepire server built from IBM's documented
results (internal/mapepire's Fake, internal/ibmi's FakePartition), and a
fake HMC built from the answers IBM's own Python library (pypowervm)
keeps from real HMCs (internal/hmc's Fake); its field names were checked
against those answers. Then on a real partition and HMC (question 5).
The demo simulator's `-ibmi 4` adds four partitions read by phx1-ibmi01:
prod1 past its system ASP threshold, ha1 a PowerHA standby with its
independent ASP varied off, dev1 on 7.4; and hmc1, their HMC, with the
server they run on, a DR server with its attention light on and a
partition stopped in Open Firmware, and an old server in standby.

## Questions

1. **How to reach it.** (a) Mapepire. (b) SSH and db2util.
2. **The profile's authority.** (a) No special authorities: MSGW is seen
   only for jobs the profile may see, and the note says how to grant
   *JOBCTL for the rest, the shop's call. (b) Ask for *JOBCTL from the
   start.
3. **Where it shows.** (a) A source like the Pure arrays, on the
   platform pages, with expected subsystems on the host settings page.
   (b) As a host of its own on the Hosts page, which needs the server to
   take a host's numbers from a reader instead of an agent: more work,
   and better later when Windows, Linux and IBM i hosts sit in one list.
4. **The HMC.** (a) After steps 1–3, as step 4. (b) Not until asked.
5. **A real partition to test on.** (a) PUB400.COM, a free public IBM i
   for learning: a personal account (sign-up is John's),
   enough to check the SQL services' real output, but not ours to
   install Mapepire on. (b) IBM Power Virtual Server: a real partition of
   our own, billed by the hour (costs; John's call). (c) Fakes only until
   a customer has one.

## Decisions

John, 2026-11-14: "go with your recommendations".

1. **Mapepire.** Checked against its repositories: the server is GPL-3.0;
   it runs on the customer's IBM i, installed by them, and we neither ship
   nor link it. IBM's Go client (Apache-2.0) brings gorilla/websocket and
   godotenv and hasn't changed since June 2025, so the agent speaks the
   protocol itself (internal/mapepire): one JSON request per WebSocket
   message, answers matched by `id`, `connect`, `sql`, `sqlmore`,
   `sqlclose`, and nothing else. golang.org/x/net/websocket, already in
   the module, is the WebSocket.

   **Read only, on Mapepire's side too.** The agent always connects with
   the property `access=read only`. That puts Mapepire in its "basic
   query" mode: before running anything it asks IBM i's own
   `QSYS2.PARSE_STATEMENT` and refuses what isn't a query of tables and
   views, and the JDBC connection itself is read only. The shop can
   enforce the same without trusting the agent: `allowbasiconly
   cairnobs@<agent's address>` in `/QOpenSys/etc/mapepire/iprules.conf`
   (the setup page says so). Basic mode allows views and unqualified
   built-in functions only, not IBM's table functions, so every read
   here is a view. That rules out `SYSTEM_ACTIVITY_INFO` (a table
   function, and it needs *JOBCTL): CPU use comes from
   `SYSTEM_STATUS_INFO_BASIC`'s `ELAPSED_` columns, which average since
   the connection's first read, so two reads give the minute between
   them.
2. **No special authorities.** And with the table functions out, MSGW
   isn't read from `ACTIVE_JOB_INFO` at all: a batch job in MSGW has sent
   an inquiry to QSYSOPR (or the message queue its job description
   names), so step 3 lists QSYSOPR's unanswered inquiries with the job
   that sent each, from the `MESSAGE_QUEUE_INFO` view. That needs no
   *JOBCTL. An interactive job waiting on its own external queue isn't
   seen; that's someone at a screen.
3. **A source like the Pure arrays**, on Clusters, with the partition as
   the source's own record and its ASPs, subsystems and interfaces as its
   parts. A host of its own on Hosts comes later, with Windows, Linux and
   IBM i in one list.
4. **The HMC as step 4**, after steps 1 to 3.
5. **PUB400.COM for real output, fakes until then.** The sign-up is
   John's, when step 3 is in; until then the tests and the demo simulator
   answer as IBM documents the views.

   Changed 2026-11-15, John: "let's skip it for now". PUB400 doesn't run
   Mapepire, and calls itself a playground, not for commercial use, so it
   could only have checked the SQL over SSH. The fakes stand until a
   shop with IBM i (and an HMC) tries it. Still to check then: Mapepire's
   timestamps and `TIMESTAMPDIFF` and `NOW() - 1 DAY` in its basic query
   mode, QSYSOPR's default authority for a `*USER` profile, and on the
   HMC, that hmcviewer with remote web access is enough for the REST API.

