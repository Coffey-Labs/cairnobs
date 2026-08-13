#!/usr/bin/env bash
# Generates a throwaway CA plus a server cert (for ingest) and a client
# cert (for the agent) for local mTLS. Dev/homelab only — never use this
# CA or its certs for anything resembling production; there's no rotation,
# no revocation, and the CA key sits unencrypted on disk right next to
# everything it signed.
#
# Re-run to regenerate from scratch; existing output is overwritten.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"
DAYS="${DEV_CERT_DAYS:-365}"

mkdir -p "${OUT_DIR}"
cd "${OUT_DIR}"

echo "Generating dev CA..."
openssl req -x509 -newkey rsa:4096 -sha256 -days "${DAYS}" -nodes \
    -keyout ca-key.pem -out ca.pem \
    -subj "/O=Sentry Dev/CN=Sentry Dev CA"

gen_leaf() {
    local name="$1" cn="$2" san="$3"
    openssl req -newkey rsa:2048 -nodes -keyout "${name}-key.pem" -out "${name}.csr" \
        -subj "/O=Sentry Dev/CN=${cn}"
    openssl x509 -req -in "${name}.csr" -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
        -out "${name}.pem" -days "${DAYS}" -sha256 \
        -extfile <(printf "subjectAltName=%s" "${san}")
    rm -f "${name}.csr"
}

# SANs cover both "reached by another container on the compose network"
# (ingest) and "reached from the host" (localhost/127.0.0.1, for an
# agent running natively per /agent/README.md's journald caveat).
echo "Generating server (ingest) cert..."
gen_leaf server ingest "DNS:ingest,DNS:localhost,IP:127.0.0.1"

echo "Generating client (agent) cert..."
gen_leaf client sentry-agent "DNS:sentry-agent"

rm -f ca.srl

echo
echo "Done. Certs written to ${OUT_DIR}/:"
ls "${OUT_DIR}"
