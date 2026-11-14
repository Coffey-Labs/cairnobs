#!/bin/sh
# Removal only (deb: "remove", rpm: 0); an upgrade keeps it running.
# Config, the database and the user stay.
set -e
case "$1" in
  remove|0)
    command -v systemctl >/dev/null || exit 0
    systemctl disable --now obs-beacond.service >/dev/null 2>&1 || true
    ;;
esac
