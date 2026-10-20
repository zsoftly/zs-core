#!/bin/sh
# Runs in the pinned KAUI image. Copies the schema file that ships inside the
# KAUI gem into /ddl/kaui.
set -eu
out=/ddl/kaui
rm -rf "$out"
mkdir -p "$out"
src="$(ls /var/lib/tomcat/webapps/ROOT/WEB-INF/gems/gems/kaui-*/db/ddl.sql | head -n 1)"
cp "$src" "$out/01-ddl.sql"
echo "exported KAUI schema from $src"
