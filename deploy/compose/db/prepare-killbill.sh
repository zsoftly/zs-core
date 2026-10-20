#!/bin/sh
# Runs once per start in the pinned Kill Bill image, as root.
# 1. Gives the plugin volume to the tomcat user. A new named volume is owned by
#    root, and the KPM bundle cannot write its plugin list there otherwise.
# 2. Copies the schema files that ship inside the Kill Bill jars into
#    /ddl/killbill, in the order they must be applied. The files come from the
#    image itself, so they always match the pinned version.
set -eu
lib=/var/lib/tomcat/webapps/ROOT/WEB-INF/lib
out=/ddl/killbill
version="$KILLBILL_VERSION"
rm -rf "$out"
mkdir -p "$out"
n=0
extract() {
  jar="$1"
  entry="$2"
  n=$((n + 1))
  name="$(printf '%02d' "$n")-$(basename "$entry")"
  unzip -p "$lib/$jar" "$entry" > "$out/$name"
}
extract "killbill-util-$version.jar" org/killbill/billing/util/ddl-postgresql.sql
extract "killbill-util-$version.jar" org/killbill/billing/util/ddl.sql
for module in account catalog entitlement invoice payment subscription tenant usage beatrix; do
  extract "killbill-$module-$version.jar" "org/killbill/billing/$module/ddl.sql"
done
queue="$(ls "$lib" | grep -E '^killbill-queue-.*\.jar$')"
extract "$queue" org/killbill/queue/ddl.sql
echo "exported $n Kill Bill schema files for $version"
chown -R tomcat:tomcat /var/lib/killbill/bundles/plugins
