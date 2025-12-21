#!/usr/bin/env bash
set -euo pipefail

NODE_NAME="${NODE_NAME:-app}"

log() {
  echo "[$(date -Iseconds)] [app/${NODE_NAME}] $*"
}

log "starting placeholder application server"
exec /usr/local/bin/acecinema-app
