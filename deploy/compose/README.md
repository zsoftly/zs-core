# Reference stack

## TL;DR

This directory holds the Docker Compose stack that runs every pinned component of a Core release: Postgres 18, Keycloak, Kill Bill, KAUI, Mailpit, the Gotenberg PDF renderer and Core itself. It is for development and tests. Run it on a build host, never on a workstation, because it starts several JVM services and real databases. `make stack-up` builds what needs building, starts everything and waits until every healthcheck passes. `make stack-bootstrap` then creates the `zs-reference` Kill Bill tenant, uploads a small test catalogue, fills Core's plugin mirror and creates the pinned plugins' tables. `make stack-apply` applies `brands/zs-reference.yaml` from the core container. `make doctor` proves the running stack matches `components.yaml`. `make stack-verify` runs these steps in order and requires a green doctor, so a clean stack needs nothing else. Every port binds to `127.0.0.1` on the build host, so you reach the stack through SSH port forwards. `make stack-reset` deletes all data.

## Run it on the build host

The `make stack-*` targets run `docker compose` in the shell you are in. The build host needs Docker with the Compose plugin, `make`, `rsync`, `curl`, `python3`, JDK 21 and Maven. `make stack-up` first runs `mvn -B verify` in `extensions/keycloak/event-listener` so the Keycloak Dockerfile can copy the provider jar.

### The build host marker

The `make stack-*` targets and `make test-integration` run only where `ZS_BUILD_HOST=1` is set in the environment. Everywhere else they refuse and exit non-zero. The build VM sets the marker for every login in `/etc/environment`:

```bash
echo 'ZS_BUILD_HOST=1' | sudo tee -a /etc/environment
```

Never set it on a workstation. `make test-integration` also runs where `ZS_TEST_POSTGRES_URL` points at a Postgres server you already run, because then it starts no containers.

Copy the working tree to the build host, then run the stack there:

```bash
rsync -az --delete --exclude .git --exclude bin --exclude /deploy/compose/.env \
  ./ <user>@<build-host>:/tmp/zs-core/
ssh <user>@<build-host> 'cd /tmp/zs-core && make stack-up && make stack-bootstrap && make doctor'
```

The first `make stack-up` creates `deploy/compose/.env` from `.env.example`. The example holds placeholder passwords. Use them on a loopback-only stack and nowhere else. A clean start, with the upstream images already pulled, takes about two minutes. Pulling the images the first time takes longer.

To reach the web interfaces from your workstation, forward the ports over SSH:

```bash
ssh -N \
  -L 8180:127.0.0.1:8180 -L 8081:127.0.0.1:8081 -L 9090:127.0.0.1:9090 \
  -L 8025:127.0.0.1:8025 -L 8080:127.0.0.1:8080 \
  <user>@<build-host>
```

Then open Keycloak at `http://127.0.0.1:8180`, KAUI at `http://127.0.0.1:9090` and Mailpit at `http://127.0.0.1:8025`. Keycloak's hostname is fixed to `http://127.0.0.1:8180`. Reach it on that exact address and forward it to the same local port.

When you are done, clean up on the build host:

```bash
ssh <user>@<build-host> 'cd /tmp/zs-core && make stack-reset'
ssh <user>@<build-host> 'rm -rf /tmp/zs-core'
```

## Make targets

| Target | What it does |
| --- | --- |
| `make stack-up` | Creates `.env` if missing, builds the Keycloak and Core images, starts the stack and waits for every healthcheck. |
| `make stack-bootstrap` | Runs `killbill/bootstrap.sh`: tenant, catalogue, plugin mirror and plugin tables. Safe to repeat. |
| `make stack-apply` | Runs `zs-core brand apply` of `brands/zs-reference.yaml` in the `core` container. Safe to repeat. |
| `make stack-verify` | `make stack-up`, `make stack-bootstrap`, `make stack-apply`, then `zs-core doctor`. Run it after `make stack-reset` to check that a clean stack reaches a green doctor with no other step. |
| `make doctor` | Runs `zs-core doctor` inside the running `core` container. Exits non-zero on any failed finding. |
| `make stack-logs` | Follows the logs of every service. |
| `make stack-down` | Stops and removes the containers. Data stays in the volumes. |
| `make stack-reset` | Removes the containers and the volumes. Every database is wiped. |

## Services

| Service | Image | Host port | Purpose |
| --- | --- | --- | --- |
| `postgres` | `postgres:18-alpine` | 5432 | One server with the databases `keycloak`, `killbill`, `kaui` and `zs_core`, each owned by its own role. |
| `keycloak` | `quay.io/keycloak/keycloak:26.8.1` plus a build step | 8180, management 9000 | Identity provider. Imports the `zs-reference` realm at first start. |
| `killbill` | `killbill/killbill:0.24.22` | 8081 | Billing engine. |
| `kaui` | `killbill/kaui:4.0.31` | 9090 | Kill Bill admin UI. |
| `mailpit` | `axllent/mailpit:v1.26.2` | 8025 (UI), 1025 (SMTP) | Catches outgoing email. |
| `gotenberg` | `gotenberg/gotenberg:8.38.1` | 3000 | The optional PDF renderer of invoice PDFs. |
| `core` | built from the `Dockerfile` at the repository root | 8080 | `zs-core serve`, running as a non-root user. It serves the plugin mirror. |
| `killbill-prep`, `kaui-ddl`, `db-init` | upstream images | none | One-shot helpers that load the Kill Bill and KAUI schemas. They exit when done. |

The image defaults in `compose.yaml` are pinned by digest (`image@sha256:...`, with the tag in a comment beside it) and must equal the `image` and `digest` of `components.yaml`. A unit test in `internal/components` fails when they differ. To rehearse an upgrade, override an image in `.env` with a plain tag or a digest (`KEYCLOAK_IMAGE`, `KILLBILL_IMAGE`, `KAUI_IMAGE`, `POSTGRES_IMAGE`, `MAILPIT_IMAGE`, `GOTENBERG_IMAGE`). `zs-core doctor` then reports the mismatch. See [../../docs/upgrades/keycloak.md](../../docs/upgrades/keycloak.md) and [../../docs/upgrades/killbill.md](../../docs/upgrades/killbill.md).

## Design notes

### Postgres

One container. `postgres/10-databases.sh` runs once, when the data directory is first created, and makes one database and one login role per component. Postgres 18 keeps its data under `/var/lib/postgresql`, so the volume is mounted there and not at `/var/lib/postgresql/data`.

### Keycloak

Keycloak runs in production mode: `start --optimized --import-realm`. The optimized start needs a build step, so `keycloak/Dockerfile` copies the locally built `zs-audit` provider jar, runs the `kc.sh build` command Keycloak documents with the database vendor and health and metrics endpoints fixed, and copies the result into a second stage that starts from the same upstream image. The HMAC secret remains deployment configuration and is never baked into an image. The base image is the `KEYCLOAK_IMAGE` build argument, which `compose.yaml` takes from the pinned digest.

The hostname is `http://127.0.0.1:8180`, with the back-channel hostname resolved from the request, so other containers can reach it as `http://keycloak:8080` while browsers use the loopback address. TLS is off because nothing leaves the loopback interface. Health and metrics are on the management port, 9000.

`keycloak/zs-reference-realm.json` is imported when the realm does not exist yet. It defines:

- the realm `zs-reference` with the locales `en` and `fr`, `en` being the default;
- `zs-customer`, a public client that uses the authorization code flow with PKCE (S256) and loopback redirect URIs;
- `zs-staff`, a confidential client with a service account. Its secret comes from `ZS_STAFF_CLIENT_SECRET` in `.env`.

Changing the JSON later does not change an existing realm. Reset the stack or use the Admin API.

### Kill Bill

Kill Bill needs its schema before it starts, and the image does not create it. The schema files ship inside the Kill Bill jars. `killbill-prep` extracts them from the pinned image into a shared volume, in the order they must be applied, so they always match the version in use. `db-init` then loads them into the `killbill` database when it is empty. The Postgres bridge file creates MySQL-compatible types, helper functions and an event trigger, which needs a superuser, so `db-init` applies that one file as `postgres` and the rest as the `killbill` role. KAUI's schema is loaded the same way into the `kaui` database. `db-init` skips a database that already has its tables, so a restart never reapplies a schema.

The image reads its database settings from `KILLBILL_DAO_URL`, `KILLBILL_DAO_USER` and `KILLBILL_DAO_PASSWORD`, and the password of the built-in `admin` user from `KB_ADMIN_PASSWORD`. Its unauthenticated health endpoint is `/1.0/healthcheck`. The `/1.0/kb/healthcheck` path does not exist in 0.24.22.

The `killbill-plugins` volume holds `/var/lib/killbill/bundles/plugins`, so installed plugins survive a container being recreated for an upgrade. `killbill-prep` gives that volume to the `tomcat` user, because the KPM bundle cannot write its plugin list into a root-owned directory.

The reference stack mounts a read-only Logback configuration at `/var/lib/tomcat/logback.xml` and a read-only `setenv2.sh` at `/var/lib/tomcat/bin/setenv2.sh`. The mounted `setenv2.sh` preserves the pinned image's database, server-properties and other environment mappings, then fails startup unless the mounted configuration is a regular, readable, nonempty file. It appends `-Dlogback.configurationFile=/var/lib/tomcat/logback.xml` without replacing existing JVM options. The mounted configuration preserves the pinned image's appenders and root INFO logging, while disabling the email configuration handler, KPM logger namespace and Beatrix event-dispatch listener because they can render SMTP properties, authenticated plugin URIs or event-dispatch messages with mirror tokens. This suppresses all diagnostics from those categories, including errors, and loses the Beatrix listener's event-dispatch diagnostics and errors. Core's typed errors, audit records and node-state checks remain the evidence for configuration and plugin operations. The pinned launcher has been read and only generates `shiro.ini` before starting Tomcat. QA must still prove the running process has this property and that every enabled sink suppresses the synthetic values before treating the control as active.

### KAUI

KAUI needs a database of its own, so the stack creates a fourth database, `kaui`. Users sign in with their Kill Bill credentials, and `db-init` allows the `admin` user, as the upstream KAUI database template does.

KAUI 4.0.31 has an upstream fault. On two cold starts, while the healthcheck probed the sign-in page, that page answered HTTP 500 with "Asset ... was not declared to be precompiled in production" and kept doing so until the container restarted. Both times, the first requests overlapped during the first render. The healthcheck probes the sign-in page and requires HTTP 200, so healthy means the page renders. Its curl call gives up after 120 seconds, before the 130-second probe timeout, and Docker runs one probe at a time, so the probes never overlap a slow first render. A cold start on the build host with this healthcheck came up healthy without the fault. If you see the 500, run `docker compose restart kaui` and load the sign-in page once before anything else. File the upstream report before you remove this note.

### Core

`Dockerfile` at the repository root is a two-stage build: a Go build stage and a small runtime image that runs `zs-core` as user 10001. The image also carries `components.yaml`, which `zs-core doctor` and brand apply read. Core applies its own migrations at start and waits for Postgres to be healthy.

`.env` supplies `ZS_KILLBILL_WEBHOOK_SECRET` and `ZS_PLUGIN_MIRROR_SECRET`. The compose file sets `ZS_KILLBILL_CALLBACK_URL=http://core:8080`. A brand apply in the `core` container registers callbacks Kill Bill reaches on the compose network. A brand apply whose manifest names a plugin also installs and starts that plugin through KPM. See [../../docs/modules/billing.md](../../docs/modules/billing.md).

The `core` container also has these settings:

- `ZS_KILLBILL_DATABASE_URL`, the `killbill` database as the `killbill` role, for `zs-core plugin ddl apply`, `doctor` and command line brand applies. `serve` never uses it.
- `ZS_PDF_RENDERER_URL=http://gotenberg:3000`, for invoice PDFs.
- `ZS_PLUGIN_MIRROR_DIR=/var/lib/zs-core/plugins` on the `plugin-mirror` volume, served at `/v1/plugins/...`, and `ZS_PLUGIN_MIRROR_URL`, `http://core:8080` unless `.env` sets it. New plugin installs require this URL and `ZS_PLUGIN_MIRROR_SECRET`; KPM downloads only from the verified mirror, with no repository fallback. The URL must be reachable from Kill Bill. Starting an already-installed plugin needs no jar download.
- `brands/` mounted read-only at `/app/brands`, and the `BRAND_ZS_REFERENCE__` secrets, for `make stack-apply`.

### Gotenberg

Gotenberg disables JavaScript, LibreOffice routes, webhooks and API `downloadFrom`. Chromium rejects public and non-public IP destinations for HTTP, HTTPS, WS and WSS. HTML file access is limited to the current request's temporary directory. Its healthcheck calls `/health`. Core sends it the invoice HTML Kill Bill rendered. See [../../docs/modules/invoices.md](../../docs/modules/invoices.md).

## Kill Bill bootstrap and plugins

`killbill/bootstrap.sh` is idempotent. It reads `.env` and waits for Kill Bill. Then:

1. It creates the tenant `zs-reference` (API key `zs-reference`, secret `KILLBILL_TENANT_API_SECRET`) unless it exists. `brands/zs-reference.yaml` references the same secret as `env://BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET`, so export that name with this value before `zs-core brand apply`.
2. It uploads `killbill/catalogue.xml` unless a catalogue digest is already recorded. The catalogue has one product, `core-test`, one monthly plan with a 14-day trial priced in CAD and USD, and one consumable usage unit, `api-call`. Once brand apply has uploaded the brand's own catalogue, the script leaves the catalogue alone.
3. It runs `zs-core plugin mirror sync` in the `core` container. It downloads every pinned plugin jar the mirror lacks, checks it against its sha256 in `components.yaml` and signs the mirror manifest.
4. It runs `zs-core plugin ddl apply --all` in the `core` container. It creates the tables of every pinned plugin that the `killbill` database lacks, and records each DDL in Core's database.

The script installs no plugin and uploads no plugin configuration. Brand apply does both, for the plugins a brand names, from the mirror. Run bootstrap before the first brand apply so the mirror holds the approved jars and its signed manifest. The repository supplies jars to mirror sync, never directly to KPM. To run Stripe on the stack, apply a brand manifest that names it with a test-mode key, as the stack tests do with `zs-second`. See [../../docs/modules/plugins.md](../../docs/modules/plugins.md).

### How plugin installation behaves on this image

KPM is bundled and works.

- The image carries two KPM parts: the KPM command line tool `kpm` (version 0.12.4) and the KPM OSGi bundle `killbill-platform-osgi-bundles-kpm` (version 0.41.23). The command line tool in this version installs Java plugins only. It cannot install Ruby plugins.
- The bundle installs a plugin at runtime when it gets an `INSTALL_PLUGIN` node command at `POST /1.0/kb/nodesInfo`. It needs the admin credentials in `KB_org_killbill_billing_plugin_kpm_adminUsername` and `..._adminPassword`, which `compose.yaml` sets. Its default download address has a doubled slash that Maven Central rejects, so Core passes the artifact address in the `pluginUri` property. The bundle accepts a `pluginUri` with a query string, which the mirror's token needs. After the install the plugin is `STOPPED`, so Core sends a `START_PLUGIN` command.
- `STOP_PLUGIN` and `UNINSTALL_PLUGIN` remove a plugin.
- The plugin's own tables are not created by Kill Bill. `zs-core plugin ddl apply` creates them before brand apply starts the plugin.

Nothing is baked into the image, so `deploy/compose/killbill/Dockerfile` does not exist. Installed plugins live in the `killbill-plugins` volume.

The 6.x Stripe releases are Ruby plugins and cannot be installed on this Kill Bill image. `components.yaml` pins the Java plugin 8.0.3. `zs-core doctor` reports any installed plugin version that differs from its pin.

## Files

| Path | Purpose |
| --- | --- |
| `compose.yaml` | The stack. |
| `.env.example` | Every variable the stack reads, with placeholder values. |
| `postgres/10-databases.sh` | Creates the databases and roles. |
| `keycloak/Dockerfile` | The documented Keycloak build step. |
| `keycloak/zs-reference-realm.json` | Realm imported at first start. |
| `db/prepare-killbill.sh`, `db/export-kaui-ddl.sh`, `db/apply-ddl.sh` | Schema extraction and loading. |
| `killbill/bootstrap.sh` | Tenant, catalogue, plugin mirror and plugin tables. |
| `killbill/catalogue.xml` | The test catalogue. |
| `killbill/logback.xml` | Pinned-image-derived Logback configuration with sensitive upstream categories disabled. |
| `killbill/setenv2.sh` | Fails closed when the mounted Logback configuration is missing or empty, then selects it without replacing existing JVM options. |
