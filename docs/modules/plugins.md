# Kill Bill plugins

## TL;DR

Core runs only the Kill Bill plugins that `components.yaml` approves and pins, each by version and by the SHA-256 of its jar. Brand apply installs the plugins a brand names, through KPM, and uploads their per-tenant configuration. Kill Bill does not create a plugin's own tables, and Core's service never writes to the Kill Bill database. So the tables are a deployment step: `zs-core plugin ddl apply`, run by the compose bootstrap or the Ansible role. It creates only the tables that are missing, and Core records each DDL it applied. `zs-core plugin mirror sync` keeps every pinned jar in a mirror directory, checked and listed in a manifest signed with the mirror secret. Every new plugin install requires Core's verified mirror, configured with `ZS_PLUGIN_MIRROR_URL` and `ZS_PLUGIN_MIRROR_SECRET`. KPM receives only bytes verified against the pin. Doctor checks the plugin tables, the mirror and the installed versions. Nothing in this path needs a console action or a manual database change.

## The rule: configuration as code

Every step that brings up a stack, applies a brand, deploys a plugin, uploads a template or fills the mirror is a command in this repository. None needs a Kill Bill or Keycloak console action or a manual database change.

- Core writes to its own database through its migrations and its commands.
- Keycloak and Kill Bill change only through their APIs, called by brand apply, the bootstrap and the tenant routes.
- The only write to the Kill Bill database outside Kill Bill itself is `zs-core plugin ddl apply`. The compose bootstrap and the Ansible role run it. It is idempotent and recorded in Core's `killbill_plugin_ddl` table.
- Kill Bill's own schema is loaded once by the stack's `db-init` helper from the files inside the pinned image. That is Kill Bill's install step, not a change Core makes.

`make stack-verify` checks the rule on the reference stack. From a clean stack it runs `make stack-up`, `make stack-bootstrap` and `make stack-apply`, which applies `brands/zs-reference.yaml` in the core container, and then `zs-core doctor`. It passes only when doctor reports no FAIL and no SKIP. See [../../deploy/compose/README.md](../../deploy/compose/README.md).

## Approved lists

`components.yaml` lists every plugin Core knows under `killbill_plugins`:

| List | Holds | A brand names it in |
| --- | --- | --- |
| `approved_payment_plugins` | Stripe 8.0.3, Adyen 0.10.0, and PayPal Express with no approved Java artifact | `billing.payment_plugins` |
| `approved_invoice_plugins` | AvaTax 0.9.3 | `billing.invoice_plugin` |
| `installable` | email-notifications 0.8.1 | `billing.email` |

Each entry with an artifact pins the Maven artifact, the version, the SHA-256 of the jar and the tables its `ddl.sql` creates. The SHA-256 pins match the downloaded Maven Central jar bytes. Stripe 8.0.3 and AvaTax 0.9.3 also have matching published `.sha256` sidecars. Adyen 0.10.0 and email-notifications 0.8.1 have no sidecar at those URLs; their pins were checked by hashing the jars. No Java simple-tax artifact is selected in this manifest; simple-tax remains unimplemented. The AvaTax plugin 0.9.y is the line its documentation names for Kill Bill 0.24, and it is built on the same parent as Stripe 8.0.3.

A plugin is installed when a brand names it. Every plugin entry must specify one of `required`, `verified_install`, `no_java_artifact` or `pinned_not_installed`. Missing or unknown statuses fail manifest validation. Only a plugin with status `required` must run on every stack. No plugin has that status in this release, so a stack without brands needs none. Doctor checks every installed plugin against its pin and reports an unlisted non-platform plugin as a failure, whether running or stopped.

Anything outside these lists is refused on every path:

- Brand apply refuses a plugin that is not in the right list with `422 plugin_unavailable`, and a manifest version other than the pin with `400 brand_manifest_invalid`.
- `zs-core plugin ddl show` and `apply` refuse a name that is not pinned or has no artifact.
- `zs-core plugin mirror sync` downloads only pinned jars.
- The mirror route serves only a pinned plugin and version that its signed manifest lists. Any other file in the mirror directory is never served.
- A configuration is uploaded only under the service name of an approved plugin, `killbill-<name>`.

## Plugin tables

```bash
zs-core plugin ddl show stripe             # print the DDL of the pinned jar
zs-core plugin ddl apply --operator <you> stripe
zs-core plugin ddl apply --all --operator <you>   # every pinned plugin with tables
```

`ddl show` reads the jar from `ZS_PLUGIN_MIRROR_DIR` when it holds it, otherwise from the repository of `components.yaml`. It checks the jar's SHA-256 against the pin and prints `ddl-postgresql.sql`, the Postgres prelude, then `ddl.sql`, each under a comment line. The digest of that text is the DDL's digest.

`ddl apply` needs `ZS_DATABASE_URL` and `ZS_KILLBILL_DATABASE_URL`. It works like this:

1. It reads and checks the jar as `ddl show` does.
2. It parses every statement. Each must create, drop, alter or index a table, or create a domain. Any other statement stops the step. So does a statement on a table the DDL does not create, or a DDL whose tables differ from the `tables` of `components.yaml`.
3. It locks the plugin's Core record through the record commit and refuses a DDL whose digest differs from one already recorded for the same plugin version, unless `--force` is given.
4. It takes a schema-wide lock in the Kill Bill database, then checks object existence and runs the missing tables' and domains' statements in the same transaction. This serializes concurrent applies, including plugins with shared domains. A table that exists is never dropped or changed, even when the plugin's DDL starts with `DROP TABLE`. A second run changes nothing.
5. It records the plugin, the version, the digest, the tables and the tables it created in Core's `killbill_plugin_ddl` table, with the operator.

It prints one JSON line per plugin with `created_tables` and `present_tables`.

Plugin DDLs are written for MySQL. The Kill Bill schema bridge that `db-init` loads creates the MySQL-compatible types the plugins use. Its event trigger turns `serial` columns into `bigint` identity columns, as it does for Kill Bill's own tables.

### What checks the tables

- `zs-core doctor` with `ZS_KILLBILL_DATABASE_URL` checks the tables of every installed or required plugin. A missing table is `FAIL killbill: plugin stripe 8.0.3 tables missing: stripe_responses`. Without the URL the check is a SKIP.
- `zs-core brand apply` on the command line with `ZS_KILLBILL_DATABASE_URL` runs the same check for every plugin the manifest names, before anything changes. A missing table is `422 plugin_tables_missing`, and nothing is installed or uploaded.
- `serve` never reads the Kill Bill database, so `POST /v1/brands/{id}/apply` does not run the check. Doctor covers it.

## The mirror

```bash
zs-core plugin mirror sync
```

`sync` needs `ZS_PLUGIN_MIRROR_DIR` and `ZS_PLUGIN_MIRROR_SECRET`. For every pinned jar it keeps a file that already matches its pin and downloads any other into a temporary file. It renames the file into place only when its SHA-256 matches. Then it writes `manifest.json` with each jar's name, version, file, digest, size and source, and signs it with an HMAC-SHA256 under the mirror secret. A jar whose download does not match its pin is never written, and sync stops.

`serve` serves the mirror at `GET /v1/plugins/{name}/{version}.jar` when `ZS_PLUGIN_MIRROR_DIR` and `ZS_PLUGIN_MIRROR_SECRET` are set. Every request names its expected pin in the required `sha256` query parameter, including requests with bearer authentication. It needs the secret as a bearer credential, or the jar's token in `token`. KPM sends no headers, so the install URL carries the token. The token is the HMAC-SHA256 of the plugin, version and digest under the secret. It opens those exact pinned bytes and never reveals the secret. Core logs routes by their pattern. Core's redaction controls do not cover upstream Kill Bill or plugin logging. The selected [reference Compose controls](../../deploy/compose/README.md) suppress the upstream email-handler, KPM and Beatrix event-dispatch categories, including errors, at the cost of diagnostic detail and event-dispatch diagnostics. Other deployments must configure equivalent controls. Treat suppression as unproven until the effective JVM configuration, selected configuration and every enabled sink have been checked with synthetic mirror-token and SMTP-credential values.

Before it serves a jar, Core checks that `components.yaml` pins the plugin and version and that the signed manifest lists it with the pinned digest. Every request reads a fresh snapshot within the 256 MiB jar limit, hashes it, and serves those exact verified bytes. Size and modification time never substitute for a checksum. A source file changed after verification cannot change the response snapshot. A file that fails verification is refused with `500` and logged as `plugin.mirror.refused`.

Brand apply checks the configured mirror's response against the pin before sending an install command, then passes KPM the mirror URL with the same expected digest and its bound token. The mirror refuses a requested digest different from its approved pin, then verifies and serves a fresh snapshot to KPM. Changing the serving Core's pin after preflight therefore refuses the install download. The upstream repository can be unreachable during the install, because only mirror sync downloads from it. Without the mirror URL and secret, a missing plugin is refused with `plugin_unavailable` before any apply changes. An installed pinned plugin that is running needs no download, and a stopped pinned plugin can still be started without the mirror.

In the 2026-10-20 Phase 5a rehearsal, Kill Bill 0.24.22's KPM bundle accepted a `pluginUri` with a token query string. Installed file names contained the plugin key and version, without the query string. The current digest-bound download URL requires a new stack acceptance run.

Doctor reports the mirror: the manifest signature, and each pinned jar as present and matching, missing, or not matching. A missing jar fails doctor when installs use the mirror.

## Install and configuration

Brand apply installs a missing plugin with an `INSTALL_PLUGIN` node command and starts it with `START_PLUGIN`. It waits up to five minutes for `RUNNING` on every node and records each node in `killbill_plugin_installs`. A plugin at another version than the pin is refused with `422 brand_apply_rejected`, and doctor fails on it. The diff names the install source `mirror`. A jar that does not match its pin is refused before KPM sees it. A failed checksum preflight is audited as `brand.killbill.plugin.created` with outcome `failure` and the error `brand_apply_rejected`.

Apply then uploads each plugin's per-tenant configuration under its service name when it differs. Payment and invoice plugin configurations come from secret references and pass the test-mode rules. See [billing.md](billing.md#test-mode-only). The email-notifications configuration is built by Core from `billing.email`. See [invoices.md](invoices.md#email).

## Prior Phase 5a rehearsal on Kill Bill 0.24.22

The 2026-10-20 rehearsal recorded these results. They describe the earlier source revision. The current takeover revisions require a new stack acceptance run.

- KPM installed Stripe 8.0.3, Adyen 0.10.0, AvaTax 0.9.3 and email-notifications 0.8.1, and each reached `RUNNING`.
- KPM installed Stripe from a `pluginUri` with a query string while the upstream repository was unreachable.
- The stack tests sent `STOP_PLUGIN` then `UNINSTALL_PLUGIN` and observed Stripe absent before reinstalling it. This did not establish a separate stopped-state observation.
- The jars ship `ddl.sql` and `ddl-postgresql.sql`. Adyen, AvaTax and email-notifications start each table with `DROP TABLE IF EXISTS`.

## Tests

- `internal/kbplugin`: statement parsing, DDL extraction, the table check against `components.yaml`, jar sources and digests, the signed mirror manifest, tokens, and `Mirror.Open` refusing a tampered jar, a forged manifest and a jar the manifest does not list. The integration test applies a DDL twice, keeps rows across a second apply, repairs one dropped table and refuses a changed DDL without `--force`.
- `internal/doctor`: the table check, the mirror state and its signature, and the PDF renderer.
- `internal/httpapi`: the mirror route's credentials, tokens and refusals.
- `TestPluginDeploymentOnTheStack` in `internal/stacktest` runs the whole path on the reference stack. See [../development.md](../development.md#tests-against-the-reference-stack).
