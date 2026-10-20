# Shared setup for the rehearsal scripts. Source it; do not run it.
# Loads deploy/compose/.env and defines the addresses of the stack on this host.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
compose_dir="$(cd "$here/.." && pwd)"
set -a
# shellcheck disable=SC1091
. "$compose_dir/.env"
set +a
KC="http://127.0.0.1:${KEYCLOAK_HOST_PORT:-8180}"
KB="http://127.0.0.1:${KILLBILL_HOST_PORT:-8081}"
COMPOSE=(docker compose --project-directory "$compose_dir")

keycloak_token() {
  curl -fsS -d grant_type=password -d client_id=admin-cli -d "username=${KEYCLOAK_ADMIN_USER:-admin}" \
    --data-urlencode "password=$KEYCLOAK_ADMIN_PASSWORD" "$KC/realms/master/protocol/openid-connect/token" |
    python3 -c 'import sys, json; print(json.load(sys.stdin)["access_token"])'
}

# kb_tenant_curl calls the Kill Bill API as the zs-reference tenant.
kb_tenant_curl() {
  curl -fsS -u "admin:$KILLBILL_ADMIN_PASSWORD" \
    -H "X-Killbill-ApiKey: $KILLBILL_TENANT_API_KEY" -H "X-Killbill-ApiSecret: $KILLBILL_TENANT_API_SECRET" \
    -H 'X-Killbill-CreatedBy: rehearsal' "$@"
}

# psql_as_postgres runs psql in the postgres container as the superuser.
psql_as_postgres() {
  "${COMPOSE[@]}" exec -T -e PGPASSWORD="$POSTGRES_PASSWORD" postgres psql -U postgres -tA "$@"
}
