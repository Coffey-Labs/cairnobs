#!/usr/bin/env bash
# Runs obs-beacond, the agent's helper and the agent on this machine, for
# development. Everything stays on 127.0.0.1; state lives in dev/state/
# (delete beacon.db there to start over).
# Open http://localhost:8150/ and use "Sign in for local development".
set -euo pipefail
cd "$(dirname "$0")/.."
state=dev/state
mkdir -p "$state"

if [[ ! -s $state/agent.token ]]; then
	./bin/obs-beacond token | sed -n '2p' | tr -d ' ' > "$state/agent.token"
	chmod 600 "$state/agent.token"
fi
if [[ ! -s $state/state.key ]]; then
	(umask 077; head -c 32 /dev/urandom | base64 > "$state/state.key")
fi
hash=$(tr -d '\n' < "$state/agent.token" | sha256sum | cut -c1-64)
host=$(hostname -s)

# Expect this machine's two running containers, plus one that only exists
# while you run it, so alerts can be watched firing and resolving:
#   docker run -d --rm --name beacon-demo-sleeper <any local image> sleep 600
cat > "$state/expected.yaml" <<YAML
hosts:
  $host:
    running:
      - shop-db-1
      - shop-web-1
      - beacon-demo-sleeper
YAML

# Checks the local agent runs: some that pass, one that fails (nothing
# listens on 8159), so both states show on the Checks page.
cat > "$state/checks.yaml" <<YAML
checks:
  - id: obs-beacond
    name: obs-beacond (local)
    http: http://127.0.0.1:8150/healthz
    contains: ok
    every: 30s
  - id: localhost-dns
    name: localhost resolves
    dns: localhost
    contains: 127.0.0.1
  - id: loopback-ping
    name: loopback ping
    ping: 127.0.0.1
    every: 30s
  - id: nothing-here
    name: nothing on port 8159
    tcp: 127.0.0.1:8159
    severity: warning
    every: 30s
    timeout: 3s
YAML

# A small service map. The worker's container is missing on purpose and the
# frontend's check fails on purpose, so root-cause grouping shows one
# incident (the worker) with the frontend listed as affected.
cat > "$state/topology.yaml" <<YAML
network:
  site: This machine
  hosts:
    - {name: $host, roles: "development"}
services:
  - {id: frontend, name: Frontend, tier: public, needs: [worker], checks: [nothing-here], sub: "the check on :8159"}
  - {id: worker, name: Worker, tier: apps, needs: [$host], containers: ["*/beacon-demo-sleeper"], sub: "beacon-demo-sleeper"}
  - {id: beacon, name: obs-beacond, tier: apps, needs: [$host], checks: [obs-beacond]}
YAML

cat > "$state/obs-beacond.yaml" <<YAML
listen: "127.0.0.1:8150"
agent_listen: "127.0.0.1:8461"      # TLS with obs-beacond's own agent CA
join:                               # Hosts, Add host: joining agents reach it here
  agent_url: "https://127.0.0.1:8461"
emergency_listen: "127.0.0.1:8152"  # http://localhost:8152: the emergency door
dev_signin: true
stale_after: 60s
expected: $state/expected.yaml
checks: $state/checks.yaml
topology: $state/topology.yaml
database: $state/beacon.db          # sign-ins and alerts survive a restart
state_key_file: $state/state.key
alerts:
  for: 20s                          # short, so you can watch it happen
  repeat: 2m
  public_url: "http://localhost:8150"
  channels:
    - id: dev-log                   # alerts print to this log
      type: log
agents:
  - name: $host
    token_sha256: "$hash"
signin:
  providers:
    - id: inbuxa
      type: inbuxa
      label: Sign in with inbuxa (fake, local)
      issuer: "http://127.0.0.1:8151"
      client_id: cairn-obs
      redirect_url: "http://localhost:8150/auth/inbuxa/callback"
    - id: local                     # the local directory: passkeys (Users page)
      type: local
      origin: "http://localhost:8150"
YAML

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# A stand-in for inbuxa's sign-in, so the real flow can be clicked through.
go run ./dev/fakeinbuxa & pids+=($!)
./bin/obs-beacond -config "$state/obs-beacond.yaml" & pids+=($!)

# The agent pins obs-beacond's agent CA, made on its first start. Wait
# until obs-beacond answers: agent-ca.pem may be left from an older database.
for _ in $(seq 100); do curl -sf -o /dev/null http://127.0.0.1:8150/healthz && break; sleep 0.1; done
fingerprint=$(./bin/obs-beacond ca -config "$state/obs-beacond.yaml" | sed -n '2p' | tr -d ' ')

cat > "$state/agent.yaml" <<YAML
server: "https://127.0.0.1:8461"
ca_fingerprint: "$fingerprint"
token_file: $state/agent.token      # enrolls once; the certificate does the rest
state_dir: $state/agent
host: $host
interval: 5s
helper_socket: $state/helper.sock
watch_server:                       # stop obs-beacond for 20s to see it fire
  after: 20s
  public_url: "http://127.0.0.1:8150/healthz"
  channels:
    - id: dev-log
      type: log
YAML

# Locally the helper runs as you (you need to be in the docker group) and
# its socket is yours alone; on a host it runs as root behind the obs-beacon group.
./bin/obs-beacon-agent helper -listen "$state/helper.sock" -docker /var/run/docker.sock -group "" & pids+=($!)
sleep 1
./bin/obs-beacon-agent -config "$state/agent.yaml" & pids+=($!)
echo "Cairn OBS dev: http://localhost:8150/  (Ctrl-C stops everything)"
wait
