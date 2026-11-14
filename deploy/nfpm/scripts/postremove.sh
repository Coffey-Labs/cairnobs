#!/bin/sh
set -e
command -v systemctl >/dev/null && systemctl daemon-reload || true
