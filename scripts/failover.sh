#!/usr/bin/env bash
# The database failover rehearsal (ADR-0040, T41c), weekly in the nfr job:
# a PostgreSQL 17 primary, a streaming replica of it and PgBouncer in
# transaction mode in front of the primary, all in Docker, and
# TestResilienceDatabaseFailover run against them. The package database is
# created and migrated on the primary directly (the migration runner holds a
# session lock, which PgBouncer's transaction mode cannot carry); the stack
# connects through PgBouncer. The test restarts the primary as a standby,
# promotes the replica and repoints PgBouncer (PAUSE, RELOAD, RESUME).
# The containers and the network are removed on exit (with the package
# database, which pgtest then fails to drop on the former primary, now a
# standby: its "drop it by hand" warning is expected here).
set -euo pipefail
cd "$(dirname "$0")/.."
PREFIX="erp-failover-$$"
PGBOUNCER_IMAGE="ghcr.io/cloudnative-pg/pgbouncer:1.26.0-trixie@sha256:01f76846eb3d2b2c6f05fc0b45466d43364001cfdd68a4883067c69c8e14817f"
PRIMARY_PORT="${ERP_FAILOVER_PRIMARY_PORT:-55460}"
REPLICA_PORT="${ERP_FAILOVER_REPLICA_PORT:-55461}"
BOUNCER_PORT="${ERP_FAILOVER_PGBOUNCER_PORT:-55462}"
dir="$(mktemp -d)"

cleanup() {
  docker rm -f "$PREFIX-primary" "$PREFIX-replica" "$PREFIX-pgbouncer" >/dev/null 2>&1 || true
  docker network rm "$PREFIX" >/dev/null 2>&1 || true
  rm -rf "$dir"
}
# shellcheck source=scripts/lib/on-exit.sh
. scripts/lib/on-exit.sh
on_exit cleanup

ready() { # container port
  for _ in $(seq 1 120); do
    if docker exec "$1" pg_isready -q -h 127.0.0.1 -p 5432 -U postgres; then return 0; fi
    sleep 0.5
  done
  echo "failover: $1 did not become ready" >&2
  docker logs "$1" >&2
  exit 1
}

docker network create "$PREFIX" >/dev/null
docker run -d --name "$PREFIX-primary" --network "$PREFIX" -p "$PRIMARY_PORT:5432" \
  -e POSTGRES_PASSWORD=dev postgres:17 \
  -c wal_level=replica -c max_wal_senders=5 -c max_connections=300 >/dev/null
ready "$PREFIX-primary"
docker exec "$PREFIX-primary" bash -c 'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
docker exec "$PREFIX-primary" psql -q -U postgres -c 'SELECT pg_reload_conf()' >/dev/null

# The replica: a base backup of the primary, streaming from it (-R).
docker run -d --name "$PREFIX-replica" --network "$PREFIX" -p "$REPLICA_PORT:5432" \
  --user postgres -e PGPASSWORD=dev -e PGDATA=/var/lib/postgresql/data/pgdata --entrypoint bash postgres:17 -c \
  "pg_basebackup -h $PREFIX-primary -U postgres -D \"\$PGDATA\" -R -X stream && chmod 700 \"\$PGDATA\" && exec postgres -c hot_standby=on -c max_connections=300" >/dev/null
ready "$PREFIX-replica"

# PgBouncer reads its database host from this file; the test rewrites it
# to the replica's and reloads.
cat >"$dir/pgbouncer.ini" <<EOF
[databases]
* = host=$PREFIX-primary port=5432
[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
pool_mode = transaction
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
admin_users = postgres
default_pool_size = 40
max_client_conn = 1000
ignore_startup_parameters = extra_float_digits
EOF
echo '"postgres" "dev"' >"$dir/userlist.txt"
chmod 644 "$dir"/*
docker run -d --name "$PREFIX-pgbouncer" --network "$PREFIX" -p "$BOUNCER_PORT:6432" \
  -v "$dir:/etc/pgbouncer" "$PGBOUNCER_IMAGE" >/dev/null
for _ in $(seq 1 60); do
  if docker exec "$PREFIX-primary" pg_isready -q -h "$PREFIX-pgbouncer" -p 6432; then break; fi
  sleep 0.5
done

DATABASE_URL="postgres://postgres:dev@localhost:$PRIMARY_PORT/postgres?sslmode=disable" \
ERP_TEST_POOLER_HOST="localhost:$BOUNCER_PORT" \
ERP_FAILOVER_PRIMARY_URL="postgres://postgres:dev@localhost:$PRIMARY_PORT/postgres?sslmode=disable" \
ERP_FAILOVER_REPLICA_URL="postgres://postgres:dev@localhost:$REPLICA_PORT/postgres?sslmode=disable" \
ERP_FAILOVER_PGBOUNCER_URL="postgres://postgres:dev@localhost:$BOUNCER_PORT/pgbouncer?sslmode=disable&default_query_exec_mode=simple_protocol" \
ERP_FAILOVER_PGBOUNCER_INI="$dir/pgbouncer.ini" \
ERP_FAILOVER_REPLICA_HOST="$PREFIX-replica" \
ERP_FAILOVER_PRIMARY_CONTAINER="$PREFIX-primary" \
  go test -count=1 -v -timeout 10m ./internal/nfr -run '^TestResilienceDatabaseFailover$'
finished
