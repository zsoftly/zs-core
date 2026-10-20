#!/usr/bin/env bash
# Prints the state an upgrade must preserve. Run it before and after the
# upgrade and compare the two outputs; only the server versions should differ.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$here/common.sh"

token="$(keycloak_token)"
kc_get() { curl -fsS -H "Authorization: Bearer $token" "$KC$1"; }

echo "keycloak server version: $(kc_get /admin/serverinfo | python3 -c 'import sys, json; print(json.load(sys.stdin)["systemInfo"]["version"])')"
user="$(kc_get '/admin/realms/zs-reference/users?username=rehearsal-user&exact=true')"
echo "keycloak user: $(echo "$user" | python3 -c 'import sys, json; u = json.load(sys.stdin)[0]; print(u["id"], u["email"], u["enabled"], u["attributes"])')"
user_id="$(echo "$user" | python3 -c 'import sys, json; print(json.load(sys.stdin)[0]["id"])')"
echo "keycloak user credentials: $(kc_get "/admin/realms/zs-reference/users/$user_id/credentials" | python3 -c 'import sys, json; print([c["type"] for c in json.load(sys.stdin)])')"
echo "keycloak realm: $(kc_get /admin/realms/zs-reference | python3 -c 'import sys, json; r = json.load(sys.stdin); print(r["realm"], r["supportedLocales"], r["defaultLocale"])')"
echo "keycloak zs-staff client credentials grant: HTTP $(curl -sS -o /dev/null -w '%{http_code}' -d grant_type=client_credentials -d client_id=zs-staff --data-urlencode "client_secret=$ZS_STAFF_CLIENT_SECRET" "$KC/realms/zs-reference/protocol/openid-connect/token")"

echo "killbill server version: $(curl -fsS -u "admin:$KILLBILL_ADMIN_PASSWORD" "$KB/1.0/kb/nodesInfo" | python3 -c 'import sys, json; print(json.load(sys.stdin)[0]["kbVersion"])')"
account="$(kb_tenant_curl "$KB/1.0/kb/accounts?externalKey=rehearsal-account")"
echo "killbill account: $(echo "$account" | python3 -c 'import sys, json; a = json.load(sys.stdin); print(a["accountId"], a["name"], a["currency"])')"
account_id="$(echo "$account" | python3 -c 'import sys, json; print(json.load(sys.stdin)["accountId"])')"
kb_tenant_curl "$KB/1.0/kb/accounts/$account_id/bundles" | python3 -c '
import sys, json
for bundle in json.load(sys.stdin):
    for s in bundle["subscriptions"]:
        print("killbill subscription:", s["subscriptionId"], s["planName"], s["state"], s["phaseType"])'
echo "killbill catalogue plans: $(kb_tenant_curl "$KB/1.0/kb/catalog/availableBasePlans" | python3 -c 'import sys, json; print([p["plan"] for p in json.load(sys.stdin)])')"

echo "rows: keycloak users=$(psql_as_postgres -d keycloak -c 'SELECT count(*) FROM user_entity') killbill accounts=$(psql_as_postgres -d killbill -c 'SELECT count(*) FROM accounts') subscriptions=$(psql_as_postgres -d killbill -c 'SELECT count(*) FROM subscriptions')"
echo "keycloak schema changesets applied: $(psql_as_postgres -d keycloak -c 'SELECT count(*) FROM databasechangelog')"
