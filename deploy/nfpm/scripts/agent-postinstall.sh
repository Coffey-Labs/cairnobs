#!/bin/sh
# Install and upgrade. Nothing starts until /etc/obs-beacon/agent.yaml
# exists: an agent without a server to report to has nothing to do.
# Once it does, every install or upgrade (re)starts the agent and helper,
# and the backup scan timer when backups.yaml is there too.
set -e
command -v systemctl >/dev/null || exit 0
systemctl daemon-reload || true
if [ -f /etc/obs-beacon/agent.yaml ]; then
  systemctl enable obs-beacon-agent.service obs-beacon-agent-helper.service >/dev/null 2>&1 || true
  systemctl restart obs-beacon-agent-helper.service || true
  systemctl restart obs-beacon-agent.service
  if [ -f /etc/obs-beacon/backups.yaml ]; then
    systemctl enable --now obs-beacon-scan.timer >/dev/null 2>&1 || true
  fi
else
  echo "obs-beacon-agent: write /etc/obs-beacon/agent.yaml (example in /usr/share/doc/obs-beacon-agent/examples), then reinstall or run: systemctl enable --now obs-beacon-agent obs-beacon-agent-helper"
fi
