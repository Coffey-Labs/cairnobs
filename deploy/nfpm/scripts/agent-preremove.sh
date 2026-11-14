#!/bin/sh
# Removal only (deb: "remove", rpm: 0); an upgrade keeps everything running.
# Config, state and the user stay, so a reinstall picks up where it left off.
set -e
case "$1" in
  remove|0)
    command -v systemctl >/dev/null || exit 0
    systemctl disable --now obs-beacon-scan.timer obs-beacon-agent.service obs-beacon-agent-helper.service >/dev/null 2>&1 || true
    ;;
esac
