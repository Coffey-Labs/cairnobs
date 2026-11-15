# Installing from a release

Each release has two packages, for amd64 and arm64, as `.deb`
(Debian, Ubuntu) and `.rpm` (RHEL, AlmaLinux, Rocky, Fedora):

| Package | What it installs |
|---|---|
| `obs-beacon-server` | `obs-beacond` and `obs-beacond.service` |
| `obs-beacon-agent` | `obs-beacon-agent`, its unit, the root helper's unit (Docker reads, the map sample, the SMBIOS UUID, libvirt's VMs and certificate files), and the backup scan's service and timer |

The static binaries are attached too, for anything else. Check every
download against `SHA256SUMS`, and `SHA256SUMS` against its signature
with [minisign](https://jedisct1.github.io/minisign/) and the Cairn OBS
release key:

```sh
minisign -Vm SHA256SUMS -P RWTkUAZH2N3bJSX65If0MZANPBxw9mxjByMWuMkIuEBrUiAB1+7XZupZ
sha256sum --ignore-missing -c SHA256SUMS
```

While Cairn OBS is private, releases are on this repo's Releases page and
need a signed-in account with access to it.

To run the server on OpenShift, OKD or Kubernetes instead, see
[kubernetes.md](kubernetes.md); agents are installed on hosts as below.

## Install

```sh
sudo apt install ./obs-beacon-server_0.1.0_amd64.deb      # on the server
sudo apt install ./obs-beacon-agent_0.1.0_amd64.deb       # on every host
# or: sudo dnf install ./obs-beacon-agent-0.1.0-1.x86_64.rpm
```

Both packages make the `obs-beacon` system user and `/etc/obs-beacon`
(root:obs-beacon, 0750). **Nothing starts until its config exists**:

- server: `/etc/obs-beacon/obs-beacond.yaml`
- agent: `/etc/obs-beacon/agent.yaml` (the join command writes it)
- backup scan timer: `/etc/obs-beacon/backups.yaml`

Examples are in `/usr/share/doc/obs-beacon-*/examples/`, and every field
is described in [formats.md](formats.md).

### The server

The package makes `/etc/obs-beacon/state.key` (it seals secrets in the
database; keep a copy with your backups). This is enough to start:

```yaml
# /etc/obs-beacon/obs-beacond.yaml (root:obs-beacon, 0640)
listen: ":8150"
agent_listen: ":8461"
database: /var/lib/obs-beacon/beacon.db
state_key_file: /etc/obs-beacon/state.key
```

```sh
sudo systemctl enable --now obs-beacond
sudo journalctl -u obs-beacond | grep -A2 'first admin'
```

With no sign-in configured, obs-beacond logs a one-time link that makes
the first admin. Open it, give your name and email, and set up how you
sign in: a passkey needs HTTPS, so on a plain `http://` address you set a
password and an authenticator app instead (both are needed every time).
The page says when the connection isn't encrypted; treat that as a first
look on a network you trust, and put HTTPS in front before real use.

For HTTPS without a proxy, give obs-beacond a DNS name whose ports 443
and 80 reach it, and it gets its own certificate from Let's Encrypt
(formats.md, HTTPS for the web interface):

```yaml
listen: ":443"
tls:
  acme:
    hostnames: [obs.example.org]
    accept_terms: true
    http_listen: ":80"
```

For hosts to join, obs-beacond needs to know the address they reach it at:
set `alerts.public_url` (its web address, which also turns on passkeys
when it is https) or `join.agent_url`. On its first start it makes its
agent CA.

### Adding a host

In the web interface, **Configure › Hosts**, give the host a name and press
**Make the install command**. Run the line it shows on the new host:

```sh
curl -fsSL https://monitor.example.org/install.sh | sudo sh -s -- --join cobsj1_…
```

`install.sh` installs `obs-beacon-agent` for the host's distribution and
architecture, checked against `SHA256SUMS`, then runs `obs-beacon-agent
join`, which makes the agent's key, joins the server (it pins the agent CA
named in the token) and writes `agent.yaml`, and starts the agent. The
command works once, for an hour, and shows in **Hosts** until it's used,
where it can be cancelled.

The script fetches the agent package from wherever the server says
(`join` in [formats.md](formats.md)):

- `join.packages_dir`: a directory on the server holding a release's agent
  `.deb`/`.rpm` files and `SHA256SUMS`, served at `/agent-packages/`. For
  networks that can't reach the download, and while Cairn OBS is private.
- `join.packages_url`: a release download URL with the same files.

With neither, or with the agent installed already, it skips to joining;
`sudo obs-beacon-agent join <token>` does the same by hand.

Removing a host in **Configure › Hosts** revokes its certificate at once. Uninstall the
agent there with the package manager.

### Windows hosts

The same box has a **Windows** line, for PowerShell as Administrator:

```powershell
& ([scriptblock]::Create((irm https://monitor.example.org/install.ps1))) -Join cobsj1_…
```

`install.ps1` fetches `obs-beacon-agent_<version>_windows_<amd64|arm64>.zip` from
the same agent packages (put it in `join.packages_dir` beside the Linux
packages), checks it against `SHA256SUMS`, runs `obs-beacon-agent.exe
service install`, joins and starts the service. By hand, from the zip:
`.\obs-beacon-agent.exe service install`, then `& "C:\Program Files\Cairn
OBS\obs-beacon-agent.exe" join <token>` and `Start-Service obs-beacon-agent`.

The service runs as its own account, `NT SERVICE\obs-beacon-agent`, with no
administrator rights and no helper; its config and state are in
`C:\ProgramData\obs-beacon`, which only it, SYSTEM and Administrators may
open, and it logs to the Application event log. It reports the host, the
Windows services named for it (as units: `units: [W32Time]` in
expected.yaml), the map sample, the machine's certificate stores, errors
and criticals from the System and Application event logs, and Hyper-V's
VMs where the role is installed (the account must be in Hyper-V
Administrators for that; docs/windows.md). No containers. Running `service
install` from a newer zip upgrades it; `service uninstall` removes it and
keeps ProgramData. Windows Server 2016 and later, and Windows 10 and 11,
on amd64 and arm64 (docs/windows.md).

### Hosts in the config file

Agents can also be listed in `obs-beacond.yaml` (`agents`), which suits
hosts managed with configuration tools. `obs-beacond token` prints a token
and its hash: the hash goes in `agents` (restart obs-beacond), the token in
the agent's token file (`obs-beacon:obs-beacon`, 0600). Write `agent.yaml`
with `server: https://<server>:8461`, `ca_fingerprint` (`sudo obs-beacond
ca` prints it) and `token_file`, then `sudo systemctl enable --now
obs-beacon-agent obs-beacon-agent-helper`. The helper is the agent's root
half: it passes on Docker reads when Docker is there and takes the map
sample (docs/maps.md); `map: false` in agent.yaml turns the sample off. The agent enrolls with its token, gets its own
certificate and renews it from then on. A name can be in the config file
or joined, not both.

Agents only connect out to the server's agent port, so hosts behind NAT
need nothing; the server needs that one port reachable, not through a
proxy that ends TLS (docs/protocol.md, Transport).

### What to watch

With hosts reporting, add what they should watch from the web interface:
websites, ports, DNS, ping and block lists on **Checks**. On **Alerts**,
add where alerts go (ntfy, Discord, a webhook or email; send each one a
test) and daily maintenance windows (a nightly reboot, say), so they
don't wake anyone. Under **Configure › Hosts**, open a host and
**Adopt what's running**
turns the containers and services there now into the list of what must
stay up. All of these export and import as YAML. To keep them
in files instead, see "Settings edited in the UI" in
[formats.md](formats.md).

## Upgrade and remove

Installing a newer package restarts the services on the new version.
Upgrade obs-beacond before the agents: a server ignores what it doesn't
know yet, but an older one may refuse it.
Removing a package stops and disables its services but keeps the config,
the database in `/var/lib/obs-beacon` and the user, so reinstalling picks
up where it left off.

## Making a release

`make release TAG=v0.1.0` builds everything into `dist/` exactly as CI
does. To publish one, push an annotated tag on main (`vX.Y.Z`, or
`vX.Y.Z-rc.N` for a pre-release); the tag's message becomes the release
notes and `.gitea/workflows/release.yml` builds and attaches the files.
