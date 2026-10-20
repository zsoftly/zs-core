#!/usr/bin/env bash
# Backs up the Keycloak and Kill Bill databases to a directory and proves the
# Keycloak dump can be restored, by loading it into a scratch database that is
# dropped again. Usage: backup.sh <directory>
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$here/common.sh"

dir="${1:?usage: backup.sh <directory>}"
mkdir -p "$dir"
for db in keycloak killbill; do
  "${COMPOSE[@]}" exec -T -e PGPASSWORD="$POSTGRES_PASSWORD" postgres pg_dump -U postgres -Fc "$db" > "$dir/$db.dump"
  echo "backup: $dir/$db.dump ($(wc -c < "$dir/$db.dump") bytes)"
done

psql_as_postgres -c 'CREATE DATABASE restore_check' > /dev/null
trap 'psql_as_postgres -c "DROP DATABASE IF EXISTS restore_check" > /dev/null' EXIT
"${COMPOSE[@]}" exec -T -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
  pg_restore -U postgres -d restore_check --no-owner < "$dir/keycloak.dump"
echo "backup: keycloak.dump restored into a scratch database with $(psql_as_postgres -d restore_check -c 'SELECT count(*) FROM user_entity') users"
