# Windows agent: design note

Status: decided 2026-11-14 (Decisions, at the end): every question went
the recommended way. Built: steps 1–4 under Plan (2026-11-14), and run on
a Windows Server 2025 evaluation VM (decision 5) the same day; what that
turned up was fixed in #10. The items under Later wait until someone asks.

John, 2026-11-12: "will need windows agent as well". Plenty of the fleets
Cairn OBS is for have Windows servers beside the Linux ones (domain
controllers, file servers, SQL Server, the odd vendor appliance), and
today they're invisible to it.

## What there is now

obs-beacon-agent is Linux only. It reads `/proc` and `statfs` for the
host, systemd over D-Bus for units, and a root helper on a Unix socket
for containers, the map sample, libvirt, the SMBIOS UUID and certificate
files. Everything else it does is plain Go and would build anywhere:
enrolling and mTLS to obs-beacond, the queue of unsent reports, checks,
fetches of status pages, the certificate scan of listening ports, the
cluster watcher and the platform readers (Proxmox, vSphere, Nutanix,
Pure). `GOOS=windows go build` stops at the first Linux-only file.

## What a Windows agent reports

The same report, so obs-beacond judges a Windows host with the code it
has; where Windows has no answer the field stays empty.

| report field | Windows source |
|---|---|
| `os`, `kernel` | product name and build (`RtlGetVersion`, the registry's `ProductName`, `DisplayVersion`) |
| `uptime_s` | `GetTickCount64` |
| `cpus`, `cpu_pct` | `GetSystemTimes`, between reports, as on Linux |
| `load` | none: Windows has no load average. Left at zero, and the UI shows "—" for Windows hosts rather than 0.00 |
| `mem` | `GlobalMemoryStatusEx`; the page file as swap |
| `disks` | fixed volumes (`GetLogicalDriveStrings`, `GetDiskFreeSpaceEx`); mount points by drive letter or folder |
| `net` | `GetIfTable2` byte counters, loopback and virtual adapters left out as on Linux |
| `machine_uuid` | SMBIOS from `GetSystemFirmwareTable('RSMB')`, so a Windows VM links to its VMware, Hyper-V, Proxmox or KubeVirt host as Linux VMs do |
| `units`, `services` | Windows services from the Service Control Manager: Running → active, Stopped with start type Automatic → failed (a service that should be running isn't), the rest inactive. `expected.yaml` names them by service name (`units: [W32Time, MSSQLSERVER]`) |
| `checks`, `fetches`, `certs` | unchanged: plain Go |
| `map` | listeners and connections with their owning process from `GetExtendedTcpTable`/`GetExtendedUdpTable`, addresses from `GetAdaptersAddresses`; the same shrinking and the same rule that Internet clients' addresses never leave the host (maps.md) |
| `containers`, `vms`, `backups`, `cert_files` | not at first (see Later) |

No cgo: `golang.org/x/sys/windows` (already in the module) has every call
above, so the Windows agent cross-builds on Linux like the rest.

## Privileges

On Linux the agent runs unprivileged and a small root helper does the few
things that need root. Windows needs no helper for the list above: a
service running as its own virtual account (`NT SERVICE\obs-beacon-agent`,
which Windows creates with the service, with no password and no rights
beyond a normal user's) can query the Service Control Manager, read the
TCP tables with their process IDs, the firmware table and the counters.
Two things it can't see without more: the names of a few protected
processes in the map (shown as "pid 1234"), and the event log's Security
channel (not needed).

## Install

A zip per architecture in the release (`obs-beacon-agent_<v>_windows_amd64.zip`:
the exe, agent.yaml.example, the licence files), checked against
SHA256SUMS as the Linux packages are. The exe installs itself:

```
obs-beacon-agent.exe service install -config C:\ProgramData\obs-beacon\agent.yaml
```

registers the service (automatic start, restart on failure, the virtual
account), puts the config and state under `C:\ProgramData\obs-beacon`
with access for that account and Administrators only, and starts it.
`service uninstall` takes it away. Join tokens get a PowerShell twin of
install.sh: `install.ps1 -Join cobsj1_…`, copied from the Hosts page as
the Linux line is, which downloads the zip, checks its hash, installs and
joins. An MSI can come later for people who deploy with Group Policy or
Intune (`wixl` from msitools builds one on Linux).

## Signing

Windows warns about unsigned executables (SmartScreen, and some endpoint
protection quarantines them), so a public release wants a code-signing
certificate. That costs money: Azure Artifact Signing is the cheapest
($9.99 a month; whether a young LLC qualifies is unclear), a classic OV
certificate a few hundred dollars a year. While the code is private this
doesn't matter: the release's SHA256SUMS (and its minisign signature, once
the repo has MINISIGN_SECRET_KEY) say the files are ours.

## Testing

Unit tests for the parts that are plain Go run on Linux as now; the
Windows calls need Windows. Options, with what each costs:

- A **Windows Server 2025 evaluation** VM on bench (free for 180 days,
  renewable by reinstalling): installs QEMU/libvirt on bench and downloads
  the ~6 GB ISO from Microsoft. Also gives a real libvirt host for the
  libvirt reader, which has only been tested against fakes.
- **GitHub Actions** Windows runners: free minutes, but the private code
  goes to GitHub before launch.
- **Cross-build and unit tests only** until launch, real Windows at the
  first user who has one: cheapest, least sure.

## Plan

1. **Builds and reports the host.** Linux-only code behind build tags;
   the Windows host collector (table above, less units and map); the
   Windows service and `service install|uninstall`; the zip in releases;
   obs-beacond and the UI show a Windows host properly ("—" for load,
   "Windows services" for systemd units, the OS).
2. **Services**: the SCM as units, and the services list for adopting
   what's running.
3. **Map sample and certificate scan**: TCP/UDP tables and adapters, and
   the existing scan of listening ports.
4. **install.ps1 and join tokens** from the Hosts page.

Later, when someone asks: the event log (errors in System and
Application as warnings, with the source), the machine's certificate
store (LocalMachine\My into the certificate inventory, like
`cert_files`), Hyper-V VMs (like libvirt), an MSI, arm64.

## Questions

1. **Privileges.** (a) The service's own virtual account, no admin, no
   helper. (b) LocalSystem: everything readable, but the agent then holds
   the most powerful account on the machine.
2. **What first.** (a) Steps 1–4 as above, the rest later. (b) Steps 1–2
   only (host and services) until someone runs it.
3. **Install.** (a) Zip + `service install` + install.ps1; an MSI later.
   (b) An MSI first.
4. **Signing.** (a) Unsigned while private; decide on a certificate
   before launch. (b) Buy Azure Artifact Signing now ($9.99/month).
5. **Testing.** (a) A Windows Server 2025 evaluation VM on bench
   (installs QEMU/libvirt there, ~6 GB download, nothing billed).
   (b) GitHub Actions Windows runners (private code on GitHub). (c) Cross-
   build and unit tests only until launch.

IBM i (John, 2026-11-12) is a separate note: no Go agent there; the idea
so far is reading it agentlessly through IBM's Mapepire server with a
read-only profile.

## Decisions

John, 2026-11-14: "go with you recommendations".

1. **The service's own virtual account**, no admin, no helper.
2. **Steps 1–4** under Plan; the event log, certificate store, Hyper-V,
   an MSI and arm64 later.
3. **Zip + `service install` + install.ps1**; an MSI later.
4. **Unsigned while private**; a code-signing certificate is decided
   before launch.
5. **A Windows Server 2025 evaluation VM on bench** (QEMU/libvirt there,
   the ISO from Microsoft; nothing billed).
