#!/usr/bin/env bash
# Creates the zs-reference Kill Bill tenant, uploads the test catalogue and,
# when STRIPE_TEST_SECRET_KEY is set, installs the Stripe plugin through KPM and
# uploads its per-tenant configuration. Safe to run again: every step checks
# the current state first.
#
# Run it on the host that runs the stack, from anywhere:
#   deploy/compose/killbill/bootstrap.sh
#
# It reads deploy/compose/.env when the variables are not already set. The
# Stripe step also needs docker compose on this host, to load the plugin's
# tables into the killbill database.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
compose_dir="$(cd "$here/.." && pwd)"
repo_root="$(cd "$compose_dir/../.." && pwd)"

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

admin_curl() {
  curl -sS -u "$KILLBILL_ADMIN_USER:$KILLBILL_ADMIN_PASSWORD" -H "X-Killbill-CreatedBy: $CREATED_BY" "$@"
}

tenant_curl() {
  admin_curl -H "X-Killbill-ApiKey: $TENANT_API_KEY" -H "X-Killbill-ApiSecret: $KILLBILL_TENANT_API_SECRET" "$@"
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

# plugin_state NAME prints "<version> <state>" for an installed plugin, or nothing.
plugin_state() {
  admin_curl "$KILLBILL_URL/1.0/kb/nodesInfo" | python3 -c '
import json, sys
name = sys.argv[1]
for node in json.load(sys.stdin):
    for plugin in node.get("pluginsInfo", []):
        if plugin.get("pluginKey") == name:
            print(plugin.get("version"), plugin.get("state"))
' "$1"
}

stripe_installed() { [ -n "$(plugin_state stripe)" ]; }

kpm_running() {
  admin_curl "$KILLBILL_URL/1.0/kb/nodesInfo" | python3 -c '
import json, sys
for node in json.load(sys.stdin):
    for plugin in node.get("pluginsInfo", []):
        if plugin.get("pluginName", "").endswith("osgi-bundles-kpm") and plugin.get("state") == "RUNNING":
            sys.exit(0)
sys.exit(1)
'
}

wait_for "Kill Bill to be healthy at $KILLBILL_URL" kill_bill_up
log "Kill Bill is healthy at $KILLBILL_URL"

# 1. Tenant.
tenant_status="$(status_of admin_curl "$KILLBILL_URL/1.0/kb/tenants?apiKey=$TENANT_API_KEY")"
if [ "$tenant_status" = "200" ]; then
  log "tenant $TENANT_API_KEY already exists"
else
  body="$(printf '{"apiKey":"%s","apiSecret":"%s"}' "$TENANT_API_KEY" "$KILLBILL_TENANT_API_SECRET")"
  create_status="$(status_of admin_curl -X POST -H 'Content-Type: application/json' -d "$body" "$KILLBILL_URL/1.0/kb/tenants")"
  case "$create_status" in
    201 | 409) log "tenant $TENANT_API_KEY created (HTTP $create_status)" ;;
    *) die "creating tenant $TENANT_API_KEY returned HTTP $create_status" ;;
  esac
fi

# 2. Catalogue. Upload only when the plan is not there yet; Kill Bill keeps
# every uploaded version, so blind re-uploads would pile up versions.
[ -f "$CATALOGUE_FILE" ] || die "catalogue file $CATALOGUE_FILE not found"
if tenant_curl -H 'Accept: application/json' "$KILLBILL_URL/1.0/kb/catalog/availableBasePlans" 2> /dev/null | grep -q '"plan":"core-test-monthly"'; then
  log "catalogue already holds core-test-monthly"
else
  upload_status="$(status_of tenant_curl -X POST -H 'Content-Type: text/xml' --data-binary "@$CATALOGUE_FILE" "$KILLBILL_URL/1.0/kb/catalog/xml")"
  [ "$upload_status" = "201" ] || die "catalogue upload returned HTTP $upload_status"
  log "catalogue uploaded"
fi

# 3. Payment plugin.
if [ -z "${STRIPE_TEST_SECRET_KEY:-}" ]; then
  log "STRIPE_TEST_SECRET_KEY is not set: leaving the built-in __EXTERNAL_PAYMENT__ plugin"
  log "done"
  exit 0
fi

case "$STRIPE_TEST_SECRET_KEY" in
  sk_test_*) ;;
  *) die "STRIPE_TEST_SECRET_KEY must be a test-mode key that starts with sk_test_" ;;
esac

stripe_version="${STRIPE_PLUGIN_VERSION:-$(awk '/- name: stripe$/ {f=1} f && /version:/ {print $2; exit}' "$repo_root/components.yaml")}"
[ -n "$stripe_version" ] || die "could not read the Stripe plugin version from components.yaml; set STRIPE_PLUGIN_VERSION"
artifact_uri="https://repo1.maven.org/maven2/org/kill-bill/billing/plugin/java/stripe-plugin/$stripe_version/stripe-plugin-$stripe_version.jar"

installed="$(plugin_state stripe || true)"
if [ -n "$installed" ] && [ "${installed%% *}" != "$stripe_version" ]; then
  die "Stripe plugin ${installed%% *} is installed but the pin is $stripe_version; follow docs/upgrades/killbill.md to change plugin versions"
fi

if [ -z "$installed" ]; then
  if ! curl -fsI -o /dev/null "$artifact_uri"; then
    die "no Java plugin artifact for Stripe $stripe_version at $artifact_uri. The kpm tooling in the pinned Kill Bill image installs Java plugins only, and the 6.x Stripe releases are Ruby plugins. components.yaml pins the Java line; set STRIPE_PLUGIN_VERSION only for a rehearsal of another Java release."
  fi
  wait_for "the KPM bundle to be running" kpm_running
  install_body="$(printf '{"nodeCommandType":"INSTALL_PLUGIN","isSystemCommandType":true,"nodeCommandProperties":[{"key":"pluginKey","value":"stripe"},{"key":"pluginVersion","value":"%s"},{"key":"pluginUri","value":"%s"}]}' "$stripe_version" "$artifact_uri")"
  install_status="$(status_of admin_curl -X POST -H 'Content-Type: application/json' -d "$install_body" "$KILLBILL_URL/1.0/kb/nodesInfo")"
  [ "$install_status" = "202" ] || die "plugin install request returned HTTP $install_status"
  wait_for "the Stripe plugin to be installed" stripe_installed
  log "Stripe plugin $stripe_version installed through KPM"
else
  log "Stripe plugin $installed"
fi

# The plugin's own tables. They go in before the plugin starts.
stripe_jar="/var/lib/killbill/bundles/plugins/java/stripe-plugin/$stripe_version/stripe-plugin-$stripe_version.jar"
has_tables="$("${COMPOSE[@]}" exec -T -e PGPASSWORD="${KILLBILL_DB_PASSWORD:?KILLBILL_DB_PASSWORD is required}" postgres \
  psql -h 127.0.0.1 -U killbill -d killbill -tAc "SELECT to_regclass('public.stripe_payment_methods') IS NOT NULL" | tr -d '[:space:]')"
if [ "$has_tables" = "t" ]; then
  log "Stripe plugin tables already present"
else
  "${COMPOSE[@]}" exec -T killbill unzip -p "$stripe_jar" ddl.sql |
    "${COMPOSE[@]}" exec -T -e PGPASSWORD="$KILLBILL_DB_PASSWORD" -e PGOPTIONS="-c client_min_messages=warning" postgres \
      psql -v ON_ERROR_STOP=1 -q -h 127.0.0.1 -U killbill -d killbill > /dev/null
  log "Stripe plugin tables created"
fi

state="$(plugin_state stripe || true)"
if [ "${state##* }" != "RUNNING" ]; then
  start_body="$(printf '{"nodeCommandType":"START_PLUGIN","isSystemCommandType":false,"nodeCommandProperties":[{"key":"pluginKey","value":"stripe"},{"key":"pluginVersion","value":"%s"}]}' "$stripe_version")"
  start_status="$(status_of admin_curl -X POST -H 'Content-Type: application/json' -d "$start_body" "$KILLBILL_URL/1.0/kb/nodesInfo")"
  [ "$start_status" = "202" ] || die "plugin start request returned HTTP $start_status"
  waited=0
  until [ "$(plugin_state stripe | awk '{print $2}')" = "RUNNING" ]; do
    [ "$waited" -lt "$WAIT_SECONDS" ] || die "Stripe plugin did not reach RUNNING within ${WAIT_SECONDS}s"
    sleep 3
    waited=$((waited + 3))
  done
fi
log "Stripe plugin is RUNNING"

# Per-tenant configuration. The plugin service name is killbill-stripe.
config_status="$(printf 'org.killbill.billing.plugin.stripe.apiKey=%s' "$STRIPE_TEST_SECRET_KEY" |
  status_of tenant_curl -X POST -H 'Content-Type: text/plain' --data-binary @- \
    "$KILLBILL_URL/1.0/kb/tenants/uploadPluginConfig/killbill-stripe")"
[ "$config_status" = "201" ] || die "uploading the Stripe plugin configuration returned HTTP $config_status"
log "Stripe plugin configuration uploaded for tenant $TENANT_API_KEY"
log "done"
