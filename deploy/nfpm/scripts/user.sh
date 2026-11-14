#!/bin/sh
# Both packages: the obs-beacon system user and group, and /etc/obs-beacon
# (root:obs-beacon 0750: the services read their config, only root writes it).
set -e
getent group obs-beacon >/dev/null || groupadd --system obs-beacon
getent passwd obs-beacon >/dev/null || useradd --system --gid obs-beacon \
  --home-dir /var/lib/obs-beacon --no-create-home --shell /usr/sbin/nologin \
  --comment "Cairn OBS" obs-beacon
install -d -m 0750 -o root -g obs-beacon /etc/obs-beacon
