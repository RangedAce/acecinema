#!/usr/bin/env bash
set -euo pipefail

ROLE="${ROLE:-app}"
NODE_NAME="${NODE_NAME:-node}"
PGDATA="${PGDATA:-/var/lib/postgresql/data}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-ace}"
DB_PASSWORD="${DB_PASSWORD:-acepass}"
DB_NAME="${DB_NAME:-acecinema}"
REPL_USER="${REPL_USER:-repl}"
REPL_PASSWORD="${REPL_PASSWORD:-replpass}"
MASTER_HOST="${MASTER_HOST:-}"
MASTER_PORT="${MASTER_PORT:-5432}"
REPLICATION_SLOT="${REPLICATION_SLOT:-${NODE_NAME}}"

log() {
  echo "[$(date -Iseconds)] [$ROLE/$NODE_NAME] $*"
}

ensure_pgdata() {
  mkdir -p "$PGDATA"
  chmod 700 "$PGDATA"
}

write_conf() {
  local conf_file="$PGDATA/acecinema.conf"
  if [[ ! -f "$conf_file" ]]; then
    cat >"$conf_file" <<EOF
listen_addresses = '*'
port = ${DB_PORT}
wal_level = replica
max_wal_senders = 10
max_replication_slots = 10
wal_keep_size = '256MB'
hot_standby = on
EOF
  fi
  if ! grep -q "acecinema.conf" "$PGDATA/postgresql.conf"; then
    echo "include_if_exists = 'acecinema.conf'" >>"$PGDATA/postgresql.conf"
  fi
}

write_hba() {
  if ! grep -q "acecinema-primary" "$PGDATA/pg_hba.conf" 2>/dev/null; then
    cat >>"$PGDATA/pg_hba.conf" <<EOF
# acecinema-primary
host all all 0.0.0.0/0 md5
host replication ${REPL_USER} 0.0.0.0/0 md5
EOF
  fi
}

init_primary() {
  ensure_pgdata
  if [[ ! -s "$PGDATA/PG_VERSION" ]]; then
    log "initializing primary database in $PGDATA"
    initdb -D "$PGDATA" >/dev/null
    write_conf
    write_hba

    log "bootstrapping roles and database (db_user=${DB_USER}, repl_user=${REPL_USER})"
    pg_ctl -D "$PGDATA" -o "-c listen_addresses='*' -p ${DB_PORT}" -w start
    psql --username=postgres -v ON_ERROR_STOP=1 <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='${DB_USER}') THEN
    CREATE ROLE ${DB_USER} LOGIN PASSWORD '${DB_PASSWORD}';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_database WHERE datname='${DB_NAME}') THEN
    CREATE DATABASE ${DB_NAME} OWNER ${DB_USER};
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='${REPL_USER}') THEN
    CREATE ROLE ${REPL_USER} REPLICATION LOGIN PASSWORD '${REPL_PASSWORD}';
  END IF;
END \$\$;
SQL
    pg_ctl -D "$PGDATA" -m fast -w stop
  else
    write_conf
    write_hba
  fi
  log "starting Postgres primary on port ${DB_PORT}"
  exec postgres -D "$PGDATA" -p "${DB_PORT}"
}

init_replica() {
  ensure_pgdata
  if [[ -z "$MASTER_HOST" ]]; then
    log "MASTER_HOST is required for replicas"
    exit 1
  fi
  if [[ ! -s "$PGDATA/PG_VERSION" ]]; then
    log "performing basebackup from ${MASTER_HOST}:${MASTER_PORT} (slot=${REPLICATION_SLOT})"
    rm -rf "${PGDATA:?}/"*
    PGPASSWORD="$REPL_PASSWORD" pg_basebackup -h "$MASTER_HOST" -p "$MASTER_PORT" -D "$PGDATA" -U "$REPL_USER" -Fp -Xs -R -C -S "$REPLICATION_SLOT"
    write_conf
    write_hba
  else
    write_conf
    write_hba
  fi
  log "starting Postgres replica on port ${DB_PORT} (master=${MASTER_HOST}:${MASTER_PORT})"
  exec postgres -D "$PGDATA" -p "${DB_PORT}"
}

case "$ROLE" in
  app)
    log "starting placeholder application server"
    exec /usr/local/bin/acecinema-app
    ;;
  db-primary)
    init_primary
    ;;
  db-replica)
    init_replica
    ;;
  *)
    log "unknown ROLE '$ROLE' (expected app | db-primary | db-replica)"
    exit 1
    ;;
esac
