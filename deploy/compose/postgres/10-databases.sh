#!/bin/sh
# Creates one database and one login role per component. Runs once, when the
# data directory is first initialised, as the superuser given by POSTGRES_USER.
set -eu

create_database() {
  db="$1"
  role="$2"
  password="$3"
  if [ -z "$password" ]; then
    echo "password for role $role is empty" >&2
    exit 1
  fi
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
    -v db="$db" -v role="$role" -v password="$password" <<'SQL'
CREATE ROLE :"role" LOGIN PASSWORD :'password';
CREATE DATABASE :"db" OWNER :"role";
REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
SQL
}

create_database keycloak keycloak "$KEYCLOAK_DB_PASSWORD"
create_database killbill killbill "$KILLBILL_DB_PASSWORD"
create_database kaui kaui "$KAUI_DB_PASSWORD"
create_database zs_core zs_core "$ZS_CORE_DB_PASSWORD"
