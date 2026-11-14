#!/bin/sh
# Install and upgrade. obs-beacond starts once /etc/obs-beacon/obs-beacond.yaml
# exists; every install or upgrade after that restarts it.
set -e
# The state key seals secrets in the database. Made once, never replaced:
# a different key can't open what the old one sealed.
if [ ! -e /etc/obs-beacon/state.key ]; then
  (umask 077; head -c 32 /dev/urandom | base64 > /etc/obs-beacon/state.key)
  chown root:obs-beacon /etc/obs-beacon/state.key
  chmod 0640 /etc/obs-beacon/state.key
fi
command -v systemctl >/dev/null || exit 0
systemctl daemon-reload || true
if [ -f /etc/obs-beacon/obs-beacond.yaml ]; then
  systemctl enable obs-beacond.service >/dev/null 2>&1 || true
  systemctl restart obs-beacond.service
else
  echo "obs-beacon-server: write /etc/obs-beacon/obs-beacond.yaml (docs/install.md has a four-line one; a full example is in /usr/share/doc/obs-beacon-server/examples), then run: systemctl enable --now obs-beacond"
fi
