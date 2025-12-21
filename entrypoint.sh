#!/usr/bin/env bash
set -euo pipefail

ROLE="${ROLE:-api}"
NODE_NAME="${NODE_NAME:-app}"

log() {
  echo "[$(date -Iseconds)] [$ROLE/${NODE_NAME}] $*"
}

case "$ROLE" in
  api|app)
    log "starting API server"
    exec /usr/local/bin/acecinema-api
    ;;
  scanner)
    log "starting scanner"
    exec /usr/local/bin/acecinema-scanner
    ;;
  *)
    log "unknown ROLE '$ROLE' (expected api|app|scanner)"
    exit 1
    ;;
esac
