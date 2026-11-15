# IBM i: design note

Status: proposed 2026-11-14; questions at the end, my recommendation
first in each.

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
| `QSYS2.SYSTEM_STATUS_INFO_BASIC` and `SYSTEM_ACTIVITY_INFO`: CPU use, system ASP % used, jobs, temporary storage, IPL time | the partition's own record: "PROD1 · read by mgmt1", "IBM i 7.5 TR6", CPU and storage graphs, up since |
| `QSYS2.ASP_INFO` and `SYSDISKSTAT`: each ASP's state and % used, each disk unit's status | parts: an ASP not varied on, or a disk unit not active, is degraded; ASP % used warns at the ASP's own threshold (IBM i's, not ours) |
| `QSYS2.SUBSYSTEM_INFO`: each subsystem and whether it's active | parts, as units are on Linux: `expected.yaml` names the subsystems that must be active (`subsystems: [QINTER, QBATCH, ORDERS]`) |
| `QSYS2.ACTIVE_JOB_INFO`, jobs waiting on a message (MSGW) | listed on the record: a job in MSGW is someone's batch stuck on a question; warns |
| `QSYS2.MESSAGE_QUEUE_INFO` for QSYSOPR: inquiry messages not yet answered, and messages of severity 80 and up | listed on the record; an unanswered inquiry warns, severity 90+ is critical |
| `QSYS2.JOB_QUEUE_INFO`: held job queues, and queues with jobs waiting longer than they should | listed; a held queue that should be released warns |
| `QSYS2.NETSTAT_INTERFACE_INFO`: interfaces and their state | parts: an interface that should be active and isn't is degraded |

What the partition says about itself it judges itself, as Purity and
vCenter do: IBM i's ASP threshold, QSYSOPR's severities. Cairn OBS adds
only what's expected to be running (subsystems) and what's been stuck too
long (MSGW, waiting jobs).

Later, when asked: the history log (`HISTORY_LOG_INFO`), PTF currency
(`GROUP_PTF_CURRENCY`, which asks IBM, so off by default), expiring
certificates in the Digital Certificate Manager store (needs more
authority than the rest), and the Power servers themselves through the
HMC (below).

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
Nothing new to listen on, but SSH needs golang.org/x/crypto/ssh (a new
dependency), a shell for the profile, and a process per query.

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

## The Power servers: the HMC

IBM i (and AIX, and Linux on Power) partitions run on Power servers
managed by an HMC (Hardware Management Console). Its REST API (port
12443, a read-only `hmcviewer` user) is to Power what vCenter is to
VMware: every server, its state, every partition on it and whether it's
running, processor and memory pools, serviceable events. Read like
vSphere (cluster-watcher.md), it would put the Power servers and all
their partitions (IBM i, AIX and Linux) on the VMs pages, and link each
IBM i partition to the server it runs on. A separate step; question 4.

## Plan

1. **The reader and the partition record**: the Mapepire client (connect,
   sign in, run a SELECT, read the rows), `ibmi:` in agent.yaml (a list,
   as `pure:` is: name, address, user, password file, CA), the partition
   as a source, SYSTEM_STATUS_INFO and ASP_INFO.
2. **Subsystems, disks, interfaces**, with `subsystems:` in
   expected.yaml.
3. **Jobs and messages**: MSGW, QSYSOPR, job queues.
4. **The HMC** (if question 4 says so).

Tested against a fake Mapepire server built from IBM's documented
results, then on a real partition (question 5).

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
