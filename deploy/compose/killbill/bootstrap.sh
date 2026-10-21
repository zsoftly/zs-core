#!/usr/bin/env bash
# Creates the zs-reference Kill Bill tenant, uploads the test catalogue, fills
# Core's plugin mirror with the pinned plugin jars and creates the tables of
# every pinned plugin in the killbill database. Safe to run again: every step
# checks the current state first.
#
# Run it on the host that runs the stack, from anywhere:
#   deploy/compose/killbill/bootstrap.sh
#
# It reads deploy/compose/.env when the variables are not already set. The
# plugin steps run zs-core in the core container through docker compose, so
# they need docker compose on this host. Plugins are installed by brand apply,
# never by this script: see docs/modules/plugins.md.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
compose_dir="$(cd "$here/.." && pwd)"

if [ -f "$compose_dir/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$compose_dir/.env"
  set +a
fi

KILLBILL_URL="${KILLBILL_URL:-http://127.0.0.1:${KILLBILL_HOST_PORT:-8081}}"
KILLBILL_ADMIN_USER="${KILLBILL_ADMIN_USER:-admin}"
: "${KILLBILL_ADMIN_PASSWORD:?KILLBILL_ADMIN_PASSWORD is required}"
TENANT_API_KEY="${KILLBILL_TENANT_API_KEY:-zs-reference}"
: "${KILLBILL_TENANT_API_SECRET:?KILLBILL_TENANT_API_SECRET is required}"
CATALOGUE_FILE="${CATALOGUE_FILE:-$here/catalogue.xml}"
CREATED_BY="zs-core-bootstrap"
WAIT_SECONDS="${BOOTSTRAP_WAIT_SECONDS:-180}"
COMPOSE=(docker compose --project-directory "$compose_dir")

log() { printf 'bootstrap: %s\n' "$*"; }
die() { printf 'bootstrap: error: %s\n' "$*" >&2; exit 1; }

# Credentials reach curl through config files readable only by this user,
# never on the command line, where any local user could read them.
secrets_dir="$(umask 077 && mktemp -d)"
trap 'rm -rf "$secrets_dir"' EXIT

# curl_quote escapes a value for a double-quoted curl config string.
curl_quote() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "$value"
}

(
  umask 077
  printf 'user = %s\n' "$(curl_quote "$KILLBILL_ADMIN_USER:$KILLBILL_ADMIN_PASSWORD")" > "$secrets_dir/admin.conf"
  printf 'header = %s\nheader = %s\n' \
    "$(curl_quote "X-Killbill-ApiKey: $TENANT_API_KEY")" \
    "$(curl_quote "X-Killbill-ApiSecret: $KILLBILL_TENANT_API_SECRET")" > "$secrets_dir/tenant.conf"
)

admin_curl() {
  curl -sS --config "$secrets_dir/admin.conf" -H "X-Killbill-CreatedBy: $CREATED_BY" "$@"
}

tenant_curl() {
  admin_curl --config "$secrets_dir/tenant.conf" "$@"
}

# status_of prints only the HTTP status of a request.
status_of() { "$@" -o /dev/null -w '%{http_code}'; }

wait_for() {
  local description="$1"
  shift
  local waited=0
  until "$@" > /dev/null 2>&1; do
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      die "timed out after ${WAIT_SECONDS}s waiting for $description"
    fi
    sleep 3
    waited=$((waited + 3))
  done
}

kill_bill_up() { curl -fsS "$KILLBILL_URL/1.0/healthcheck" | grep -q 'KillbillHealthcheck":{"healthy":true'; }

wait_for "Kill Bill to be healthy at $KILLBILL_URL" kill_bill_up
log "Kill Bill is healthy at $KILLBILL_URL"

# 1. Tenant.
tenant_status="$(status_of admin_curl "$KILLBILL_URL/1.0/kb/tenants?apiKey=$TENANT_API_KEY")"
if [ "$tenant_status" = "200" ]; then
  log "tenant $TENANT_API_KEY already exists"
else
  # The body holds the tenant secret, so it goes through stdin.
  create_status="$(printf '{"apiKey":"%s","apiSecret":"%s"}' "$TENANT_API_KEY" "$KILLBILL_TENANT_API_SECRET" |
    status_of admin_curl -X POST -H 'Content-Type: application/json' --data-binary @- "$KILLBILL_URL/1.0/kb/tenants")"
  case "$create_status" in
    201 | 409) log "tenant $TENANT_API_KEY created (HTTP $create_status)" ;;
    *) die "creating tenant $TENANT_API_KEY returned HTTP $create_status" ;;
  esac
fi

# 2. Catalogue. The SHA-256 of the last uploaded catalogue.xml is kept in the
# tenant's user key-value store. Upload only when no catalogue was uploaded
# yet, so an unchanged one does not pile up versions in Kill Bill, which keeps
# every upload. Once brand apply has uploaded the brand's catalogue, the
# manifest owns it.
[ -f "$CATALOGUE_FILE" ] || die "catalogue file $CATALOGUE_FILE not found"
catalogue_key="zs-core-catalogue-sha256"
catalogue_digest="$(sha256sum "$CATALOGUE_FILE" | awk '{print $1}')"
stored_digest="$(tenant_curl -H 'Accept: application/json' "$KILLBILL_URL/1.0/kb/tenants/userKeyValue/$catalogue_key" |
  python3 -c 'import json, sys
try:
    values = json.load(sys.stdin).get("values") or []
except ValueError:
    values = []
print(values[-1] if values else "")')"
if [ "$stored_digest" = "$catalogue_digest" ]; then
  log "catalogue unchanged (sha256 $catalogue_digest)"
elif [ -n "$stored_digest" ]; then
  # zs-core brand apply uploaded the brand's own catalogue and recorded its
  # digest under the same key. From then on the brand manifest owns the
  # catalogue, and this script leaves it alone.
  log "catalogue managed by brand apply (sha256 $stored_digest), not uploaded"
else
  upload_status="$(status_of tenant_curl -X POST -H 'Content-Type: text/xml' --data-binary "@$CATALOGUE_FILE" "$KILLBILL_URL/1.0/kb/catalog/xml")"
  case "$upload_status" in
    201) ;;
    400) die "catalogue upload returned HTTP 400. Kill Bill rejects an edited catalogue whose effectiveDate it already holds; give the edited catalogue a later effectiveDate" ;;
    *) die "catalogue upload returned HTTP $upload_status" ;;
  esac
  # Kill Bill appends values to a user key; keep exactly one.
  delete_status="$(status_of tenant_curl -X DELETE "$KILLBILL_URL/1.0/kb/tenants/userKeyValue/$catalogue_key")"
  case "$delete_status" in
    2??) ;;
    *) die "clearing the stored catalogue digest returned HTTP $delete_status" ;;
  esac
  key_status="$(printf '%s' "$catalogue_digest" |
    status_of tenant_curl -X POST -H 'Content-Type: text/plain' --data-binary @- "$KILLBILL_URL/1.0/kb/tenants/userKeyValue/$catalogue_key")"
  [ "$key_status" = "201" ] || die "storing the catalogue digest returned HTTP $key_status"
  log "catalogue uploaded (sha256 $catalogue_digest)"
fi

# 3. Plugin jars and tables. zs-core checks every jar against its sha256 in
# components.yaml, keeps it in the mirror Kill Bill installs plugins from, and
# applies each plugin's DDL once. Both steps record what they did and change
# nothing when run again.
"${COMPOSE[@]}" exec -T core zs-core plugin mirror sync
"${COMPOSE[@]}" exec -T core zs-core plugin ddl apply --all --operator "$CREATED_BY"
log "plugin mirror synced and plugin tables applied"
log "done"
