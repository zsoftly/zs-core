#!/bin/sh
# Applies the Kill Bill and KAUI schemas when their databases are empty.
# Safe to run on every start: a database that already has its tables is left
# alone. Schema changes between component versions are handled by the upgrade
# runbooks in docs/upgrades, not here.
#
# The PostgreSQL bridge file creates MySQL-compatible types, helper functions
# and an event trigger, which needs a superuser, so it runs as postgres. The
# tables themselves are created by the component's own role.
set -eu

apply_if_empty() {
  db="$1"
  role="$2"
  password="$3"
  probe="$4"
  bridge="$5"
  dir="$6"
  export PGPASSWORD="$password"
  present="$(psql -h postgres -U "$role" -d "$db" -tAc "SELECT to_regclass('public.$probe') IS NOT NULL")"
  if [ "$present" = "t" ]; then
    echo "$db: schema already present"
    return 0
  fi
  echo "$db: applying bridge $(basename "$bridge") as postgres"
  PGPASSWORD="$POSTGRES_PASSWORD" psql -v ON_ERROR_STOP=1 -q -h postgres -U postgres -d "$db" -f "$bridge" > /dev/null
  for file in "$dir"/*.sql; do
    echo "$db: applying $(basename "$file")"
    psql -v ON_ERROR_STOP=1 -q -h postgres -U "$role" -d "$db" -f "$file" > /dev/null
  done
  echo "$db: schema applied"
}

bridge=/ddl/killbill/01-ddl-postgresql.sql
mkdir -p /tmp/killbill-ddl /tmp/kaui-ddl
for file in /ddl/killbill/*.sql; do
  [ "$file" = "$bridge" ] || cp "$file" /tmp/killbill-ddl/
done
cp /ddl/kaui/01-ddl.sql /tmp/kaui-ddl/
# KAUI only lets users in its allowed list sign in. The upstream database
# template allows the Kill Bill admin user, and so does this stack.
cat > /tmp/kaui-ddl/02-allowed-user.sql <<'SQL'
INSERT INTO kaui_allowed_users (kb_username, description, created_at, updated_at)
VALUES ('admin', 'super admin', NOW(), NOW());
SQL

apply_if_empty killbill killbill "$KILLBILL_DB_PASSWORD" accounts "$bridge" /tmp/killbill-ddl
apply_if_empty kaui kaui "$KAUI_DB_PASSWORD" kaui_users "$bridge" /tmp/kaui-ddl
