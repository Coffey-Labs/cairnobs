# Cairn OBS: install obs-beacon-agent on this Windows host and join it to
# the obs-beacond that served this script. In PowerShell as Administrator:
#
#   & ([scriptblock]::Create((irm https://<server>/install.ps1))) -Join <token>
#
# Options: -Join <token> (from Hosts, Add host), -Packages <url> (where the
# agent's zip and SHA256SUMS are), -Force (replace an existing agent.yaml).
# It throws rather than exits: run as a script block, exit would close the
# PowerShell window it was pasted into.
param([string]$Join = '', [string]$Packages = '', [switch]$Force)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$Version = @VERSION@
if (-not $Packages) { $Packages = @PACKAGES@ }
$Packages = $Packages.TrimEnd('/')
function Say([string]$m) { Write-Host "obs-beacon install: $m" }

if (-not $Join) { throw 'run it with -Join <token>: Hosts, Add host in the web interface makes one' }
$me = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $me.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'run it in PowerShell as Administrator' }
if ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64') { throw "no agent for $env:PROCESSOR_ARCHITECTURE yet" }
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$exe = Join-Path $env:ProgramFiles 'Cairn OBS\obs-beacon-agent.exe'
if (Test-Path $exe) {
  Say "obs-beacon-agent is installed already: $(& $exe -version)"
} else {
  if (-not $Packages) { throw "this server doesn't say where agent packages are (join.packages_dir or join.packages_url in obs-beacond.yaml). Install the agent from its zip by hand (README.txt in it), then: obs-beacon-agent.exe join <token>" }
  $file = "obs-beacon-agent_$($Version.TrimStart('v'))_windows_amd64.zip"
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('obs-beacon-' + [guid]::NewGuid())
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Say "fetching $file from $Packages"
    Invoke-WebRequest -UseBasicParsing -Uri "$Packages/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
    Invoke-WebRequest -UseBasicParsing -Uri "$Packages/$file" -OutFile (Join-Path $tmp $file)
    $line = Get-Content (Join-Path $tmp 'SHA256SUMS') | Where-Object { ($_ -split '\s+')[1] -eq $file } | Select-Object -First 1
    if (-not $line) { throw "$file isn't in SHA256SUMS" }
    $want = ($line -split '\s+')[0].ToLower()
    $got = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $file)).Hash.ToLower()
    if ($got -ne $want) { throw "$file doesn't match SHA256SUMS" }
    Expand-Archive -Path (Join-Path $tmp $file) -DestinationPath (Join-Path $tmp 'zip')
    & (Join-Path $tmp 'zip\obs-beacon-agent.exe') service install
    if ($LASTEXITCODE -ne 0) { throw 'service install failed' }
  } finally {
    Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
  }
  Say "installed $(& $exe -version)"
}

$joinArgs = @('join')
if ($Force) { $joinArgs += '-force' }
$joinArgs += $Join
& $exe @joinArgs
if ($LASTEXITCODE -ne 0) { throw 'join failed' }
Restart-Service -Name obs-beacon-agent
Say 'done: obs-beacon-agent is running and reporting'
