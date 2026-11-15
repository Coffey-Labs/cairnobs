#!/bin/bash
# SPDX-FileCopyrightText: 2026 Coffey Labs LLC
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Build a release: the web interface, obs-beacond and obs-beacon-agent for
# linux/amd64 and linux/arm64, the .deb and .rpm packages, the Windows
# agent's zips (windows/amd64 and windows/arm64, docs/windows.md), and SHA256SUMS.
#
# Usage: scripts/build-release.sh TAG OUTDIR
#        scripts/build-release.sh v0.1.0 dist
#
# CI runs exactly this on a v* tag (.gitea/workflows/release.yml), so a
# release can be built and checked on any machine with Go and Node first.
# Binaries are static (CGO off): one build runs on every supported
# distribution. The tag (without the v) is stamped in, so `-version` and the
# release can't disagree.
set -euo pipefail
cd "$(dirname "$0")/.."

TAG="${1:?usage: build-release.sh TAG OUTDIR}"
OUT="${2:?usage: build-release.sh TAG OUTDIR}"
[[ "$TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$ ]] \
  || { echo "tag $TAG is not vX.Y.Z or vX.Y.Z-rc.N" >&2; exit 1; }
VERSION="${TAG#v}"
NFPM="github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0"   # 2026-10-12
MODULE="$(go list -m)"

# The interface first: obs-beacond embeds it.
make web

rm -rf "$OUT"; mkdir -p "$OUT"
stage=bin/package   # the nfpm configs read the binaries from here
for arch in amd64 arm64; do
  rm -rf "$stage"; mkdir -p "$stage"
  for cmd in obs-beacond obs-beacon-agent; do
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X $MODULE/internal/version.Version=$VERSION" -o "$stage/$cmd" "./cmd/$cmd"
    cp "$stage/$cmd" "$OUT/$cmd-linux-$arch"
  done
  for pkg in obs-beacon-agent obs-beacon-server; do
    for fmt in deb rpm; do
      ARCH="$arch" VERSION="$VERSION" \
        go run "$NFPM" package --config "deploy/nfpm/$pkg.yaml" --packager "$fmt" --target "$OUT/"
    done
  done
done

# Windows: the agent alone, in a zip with its example config and licences.
for arch in amd64 arm64; do
  rm -rf "$stage"; mkdir -p "$stage"
  CGO_ENABLED=0 GOOS=windows GOARCH=$arch go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X $MODULE/internal/version.Version=$VERSION" -o "$stage/obs-beacon-agent.exe" ./cmd/obs-beacon-agent
  go run ./scripts/zipfiles "$OUT/obs-beacon-agent_${VERSION}_windows_${arch}.zip" \
    "$(git log -1 --format=%cI HEAD 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)" \
    obs-beacon-agent.exe="$stage/obs-beacon-agent.exe" \
    agent.example.yaml=deploy/windows/agent.example.yaml \
    README.txt=deploy/windows/README.txt \
    LICENSE.txt=LICENSE \
    THIRD_PARTY_NOTICES.md=THIRD_PARTY_NOTICES.md
done
rm -rf "$stage"

(cd "$OUT" && sha256sum -- * > SHA256SUMS)
cat "$OUT/SHA256SUMS"
