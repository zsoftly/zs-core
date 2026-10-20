#!/usr/bin/env bash
# Creates the data an upgrade rehearsal must see again afterwards: one Keycloak
# user in the zs-reference realm, and one Kill Bill account with a subscription
# to core-test-monthly. Run bootstrap.sh first. Safe to run once per stack.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$here/common.sh"

token="$(keycloak_token)"
status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$KC/admin/realms/zs-reference/users" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  -d '{"username":"rehearsal-user","email":"rehearsal-user@example.test","firstName":"Rehearsal","lastName":"User","enabled":true,"emailVerified":true,"attributes":{"locale":["fr"]},"credentials":[{"type":"password","value":"rehearsal-pw-1","temporary":false}]}')"
echo "keycloak: create user rehearsal-user returned HTTP $status"

account="$(kb_tenant_curl -H 'Content-Type: application/json' -X POST "$KB/1.0/kb/accounts" -D - -o /dev/null \
  -d '{"name":"Rehearsal Account","email":"rehearsal-account@example.test","currency":"CAD","externalKey":"rehearsal-account"}' |
  tr -d '\r' | awk -F': ' 'tolower($1) == "location" {print $2}')"
echo "killbill: account ${account##*/}"
subscription="$(kb_tenant_curl -H 'Content-Type: application/json' -X POST "$KB/1.0/kb/subscriptions" -D - -o /dev/null \
  -d "{\"accountId\":\"${account##*/}\",\"planName\":\"core-test-monthly\"}" |
  tr -d '\r' | awk -F': ' 'tolower($1) == "location" {print $2}')"
echo "killbill: subscription ${subscription##*/}"
