#!/usr/bin/env bash
#
# Push this checkout to the demo host and restart the stack.
#
# The demo host does not hold a git checkout -- the tree was copied there --
# so there is no `git pull` to run on the far end and this has to push.
#
# What it syncs: exactly the files git tracks, via `git ls-files`. That is the
# whole point of the script rather than an rsync invocation typed from memory.
# An exclude list has to name every host-owned file, and the one it missed --
# `hack/dev-certs/out/`, gitignored and generated per machine -- took the demo
# down on 2026-09-10: ingest mounts that directory and began serving a cert
# signed by the *developer's* CA, while the host's agent simulator still
# trusted the host's own. Every check-in failed `x509: certificate signed by
# unknown authority` until the simulator was restarted. Syncing only tracked
# files makes that class of mistake impossible: anything gitignored is, by
# construction, never sent.
#
# What it still excludes, from the tracked set:
#   docker-compose.yml -- edited in place on the host to bind every published
#     port to 127.0.0.1. Overriding `ports:` from the override file does not
#     work (Compose concatenates list-type keys rather than replacing them),
#     so the host's copy is authoritative and must not be overwritten.
#   .env -- tracked, and so NOT covered by the git-tracked rule above. The
#     host's copy carries ALERTING_SERVICE_TOKEN, which the repo's does not;
#     sending the repo's would drop it and take alerting down.
#
# Untracked host state -- .env, docker-compose.override.yml, demo-*.json,
# bin/, the dev certs -- is never in `git ls-files`, so it needs no mention.
#
# Usage:
#   deploy/demo-sync.sh            # dry run: show what would change
#   deploy/demo-sync.sh --yes      # sync, rebuild, restart
#
# Env:
#   DEMO_HOST  ssh target (default: Web_Host)
#   DEMO_PATH  remote directory (default: ~/cairnobs-demo)
set -euo pipefail

HOST="${DEMO_HOST:-Web_Host}"
# Tilde, not $HOME: rsync and ssh hand this to the *remote* shell, which
# expands it there. A literal $HOME expands locally (or not at all) and
# silently targets the wrong directory.
DEST="${DEMO_PATH:-~/cairnobs-demo}"
APPLY=0
[ "${1:-}" = "--yes" ] || [ "${1:-}" = "-y" ] && APPLY=1

cd "$(dirname "$0")/.."
command -v git >/dev/null || { echo "git not found" >&2; exit 1; }
git rev-parse --git-dir >/dev/null 2>&1 || { echo "not a git checkout" >&2; exit 1; }

if [ -n "$(git status --porcelain)" ]; then
  echo "==> working tree is dirty; the demo would get uncommitted changes:" >&2
  git status --short >&2
  [ "$APPLY" = "1" ] && { echo "==> refusing to sync a dirty tree" >&2; exit 1; }
fi

echo "==> source: $(git log --oneline -1)"
echo "==> target: $HOST:$DEST"

# Only tracked files, minus the one the host owns.
git ls-files | grep -vxE 'docker-compose\.yml|\.env' > /tmp/cairnobs-demo-files.$$
trap 'rm -f /tmp/cairnobs-demo-files.$$' EXIT
echo "==> $(wc -l < /tmp/cairnobs-demo-files.$$) tracked files to consider"

RSYNC_ARGS=(-az --files-from=/tmp/cairnobs-demo-files.$$ ./ "$HOST:$DEST/")
if [ "$APPLY" = "0" ]; then
  echo "==> DRY RUN (pass --yes to apply)"
  rsync --dry-run --itemize-changes "${RSYNC_ARGS[@]}" | grep -v '^\.d' || true
  exit 0
fi

rsync "${RSYNC_ARGS[@]}"
echo "==> synced; rebuilding"
ssh "$HOST" "cd $DEST && docker compose build && docker compose up -d"

# The simulator reads the CA and its client cert once, at startup. It runs on
# the host, outside Compose, so `docker compose up -d` does not touch it -- and
# if anything about the mTLS material changed underneath it, every check-in
# fails until it is restarted. Cheap to do unconditionally.
echo "==> restarting the agent simulator"
ssh "$HOST" 'systemctl restart cairnobs-demo-simulator.service 2>/dev/null || sudo systemctl restart cairnobs-demo-simulator.service'

echo "==> health"
ssh "$HOST" 'curl -sS -m 10 -o /dev/null -w "  api /healthz -> %{http_code}\n" http://127.0.0.1:8080/healthz; docker compose -f '"$DEST"'/docker-compose.yml ps --format "  {{.Service}}\t{{.State}}" 2>/dev/null | head -12'
