#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Coffey Labs LLC
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# build-image.sh TAG DIST [IMAGE]: the obs-beacond image for linux/amd64 and
# linux/arm64 from a release's binaries in DIST (scripts/build-release.sh).
# Without IMAGE it writes an OCI archive, DIST/obs-beacond-image-TAG.tar,
# that `skopeo copy oci-archive:...` or `oc image mirror` can push anywhere.
# With IMAGE (e.g. registry.example.org/ops/obs-beacond) it pushes
# IMAGE:VERSION. Needs docker buildx.
#
# Each release's image is published as ghcr.io/coffey-labs/cairnobs by
# .github/workflows/release.yml; this script is how it's built.
set -euo pipefail
TAG="${1:?usage: build-image.sh TAG DIST [IMAGE]}"
DIST="${2:?usage: build-image.sh TAG DIST [IMAGE]}"
IMAGE="${3:-}"
VERSION="${TAG#v}"
for arch in amd64 arm64; do
  for bin in obs-beacond obs-beacon-agent; do
    [ -f "$DIST/$bin-linux-$arch" ] || { echo "$DIST/$bin-linux-$arch is missing: run scripts/build-release.sh $TAG $DIST first" >&2; exit 1; }
  done
done
# A small build context: only what the image holds.
ctx="$(mktemp -d)"; trap 'rm -rf "$ctx"' EXIT
cp LICENSE THIRD_PARTY_NOTICES.md "$DIST"/obs-beacond-linux-{amd64,arm64} "$DIST"/obs-beacon-agent-linux-{amd64,arm64} "$ctx/"
args=(--file deploy/container/Containerfile --platform linux/amd64,linux/arm64
      --build-arg "VERSION=$VERSION" --provenance=false)
if [ -n "$IMAGE" ]; then
  docker buildx build "${args[@]}" --tag "$IMAGE:$VERSION" --push "$ctx"
else
  out="$(cd "$DIST" && pwd)/obs-beacond-image-$TAG.tar"
  docker buildx build "${args[@]}" --output "type=oci,dest=$out" "$ctx"
  echo "$out"
fi
