Cairn OBS agent for Windows

Reports this host's health to Cairn OBS (obs-beacond): CPU, memory,
volumes, network, uptime and the Windows services you name.

Install, in PowerShell as Administrator, from this folder:

  .\obs-beacon-agent.exe service install

That copies the agent to C:\Program Files\Cairn OBS, registers the
obs-beacon-agent service (automatic start, restarted if it fails, running as
its own account NT SERVICE\obs-beacon-agent with no administrator rights) and
makes C:\ProgramData\obs-beacon for its config and state. Then either join
with a token from Hosts, Add host:

  & "C:\Program Files\Cairn OBS\obs-beacon-agent.exe" join <token>
  Start-Service obs-beacon-agent

or write C:\ProgramData\obs-beacon\agent.yaml from agent.example.yaml and
start the service. Its log is in the Application event log, source
obs-beacon-agent.

Upgrade: run `service install` from the new zip; it stops the service,
replaces the program and starts it again. Remove: `service uninstall`
(the config and state in ProgramData are kept).

Licensed under the GNU Affero General Public License v3 or later
(LICENSE.txt); the components it's built from are in THIRD_PARTY_NOTICES.md.
