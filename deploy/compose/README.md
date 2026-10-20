# Reference stack

## TL;DR

This directory holds the Docker Compose stack that runs every pinned component of a Core release: Postgres 18, Keycloak, Kill Bill, KAUI, Mailpit and Core itself. It is for development and tests. Run it on a build host, never on a workstation, because it starts several JVM services and real databases. `make stack-up` builds what needs building, starts everything and waits until every healthcheck passes. `make stack-bootstrap` then creates the `zs-reference` Kill Bill tenant and uploads a small test catalogue. `make doctor` proves the running stack matches `components.yaml`. Every port binds to `127.0.0.1` on the build host, so you reach the stack through SSH port forwards. `make stack-reset` deletes all data.

## Run it on the build host

The `make stack-*` targets run `docker compose` in the shell you are in. The build host needs Docker with the Compose plugin, `make`, `rsync`, `curl` and `python3`.

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
| `make stack-bootstrap` | Runs `killbill/bootstrap.sh`: tenant, catalogue and, when `STRIPE_TEST_SECRET_KEY` is set, the Stripe plugin. Safe to repeat. |
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
| `core` | built from the `Dockerfile` at the repository root | 8080 | `zs-core serve`, running as a non-root user. |
| `killbill-prep`, `kaui-ddl`, `db-init` | upstream images | none | One-shot helpers that load the Kill Bill and KAUI schemas. They exit when done. |

The image defaults in `compose.yaml` are pinned by digest (`image@sha256:...`, with the tag in a comment beside it) and must equal the `image` and `digest` of `components.yaml`. A unit test in `internal/components` fails when they differ. To rehearse an upgrade, override an image in `.env` with a plain tag or a digest (`KEYCLOAK_IMAGE`, `KILLBILL_IMAGE`, `KAUI_IMAGE`, `POSTGRES_IMAGE`, `MAILPIT_IMAGE`). `zs-core doctor` then reports the mismatch. See [../../docs/upgrades/keycloak.md](../../docs/upgrades/keycloak.md) and [../../docs/upgrades/killbill.md](../../docs/upgrades/killbill.md).

## Design notes

### Postgres

One container. `postgres/10-databases.sh` runs once, when the data directory is first created, and makes one database and one login role per component. Postgres 18 keeps its data under `/var/lib/postgresql`, so the volume is mounted there and not at `/var/lib/postgresql/data`.

### Keycloak

Keycloak runs in production mode: `start --optimized --import-realm`. The optimized start needs a build step, so `keycloak/Dockerfile` runs the `kc.sh build` command Keycloak documents, with the database vendor and the health and metrics endpoints fixed, and copies the result into a second stage that starts from the same upstream image. It adds no files of its own and does not change the upstream image. The base image is the `KEYCLOAK_IMAGE` build argument, which `compose.yaml` takes from the pinned tag.

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

### KAUI

KAUI needs a database of its own, so the stack creates a fourth database, `kaui`. Users sign in with their Kill Bill credentials, and `db-init` allows the `admin` user, as the upstream KAUI database template does.

KAUI 4.0.31 has an upstream fault. On two cold starts, while the healthcheck probed the sign-in page, that page answered HTTP 500 with "Asset ... was not declared to be precompiled in production" and kept doing so until the container restarted. Both times, the first requests overlapped during the first render. The healthcheck probes the sign-in page and requires HTTP 200, so healthy means the page renders. Its curl call gives up after 120 seconds, before the 130-second probe timeout, and Docker runs one probe at a time, so the probes never overlap a slow first render. A cold start on the build host with this healthcheck came up healthy without the fault. If you see the 500, run `docker compose restart kaui` and load the sign-in page once before anything else. File the upstream report before you remove this note.

### Core

`Dockerfile` at the repository root is a two-stage build: a Go build stage and a small Alpine runtime that runs `zs-core` as user 10001. The image also carries `components.yaml`, which `zs-core doctor` reads. Core applies its own migrations at start and waits for Postgres to be healthy.

## Kill Bill bootstrap and plugins

`killbill/bootstrap.sh` is idempotent. It reads `.env`, waits for Kill Bill, then:

1. creates the tenant `zs-reference` (API key `zs-reference`, secret `KILLBILL_TENANT_API_SECRET`) unless it exists;
2. uploads `killbill/catalogue.xml` unless the plan `core-test-monthly` is already there. The catalogue has one product, `core-test`, one monthly plan with a 14-day trial priced in CAD and USD, and one consumable usage unit, `api-call`;
3. when `STRIPE_TEST_SECRET_KEY` is set, installs the Stripe plugin and uploads its per-tenant configuration. Without the key it does nothing more and Kill Bill keeps its built-in `__EXTERNAL_PAYMENT__` plugin.

### How plugin installation behaves on this image

KPM is bundled and works. The script handles three limits.

- The image carries two KPM parts: the KPM command line tool `kpm` (version 0.12.4) and the KPM OSGi bundle `killbill-platform-osgi-bundles-kpm` (version 0.41.23). The command line tool in this version installs Java plugins only. It cannot install Ruby plugins.
- The bundle installs a plugin at runtime when you send an `INSTALL_PLUGIN` node command to `POST /1.0/kb/nodesInfo`. It needs the admin credentials in `KB_org_killbill_billing_plugin_kpm_adminUsername` and `..._adminPassword`, which `compose.yaml` sets. Its default download address has a doubled slash that Maven Central rejects, so the script passes the artifact address in the `pluginUri` property instead of letting the bundle build it. After the install the plugin is `STOPPED`, so the script sends a `START_PLUGIN` command.
- The plugin's own tables are not created by Kill Bill. The script loads the `ddl.sql` shipped inside the plugin jar into the `killbill` database before it starts the plugin.

Nothing is baked into the image, so `deploy/compose/killbill/Dockerfile` does not exist. Installed plugins live in the `killbill-plugins` volume.

The Stripe version is read from `components.yaml`, which pins the Java plugin 8.0.3 (`org.kill-bill.billing.plugin.java:stripe-plugin`). The 6.x Stripe releases are Ruby plugins and cannot be installed on this Kill Bill image. `STRIPE_PLUGIN_VERSION` overrides the pin for a rehearsal. `zs-core doctor` reports any installed Stripe version that differs from the pin.

The Stripe configuration the script uploads sets only `org.killbill.billing.plugin.stripe.apiKey`. Use a test-mode key (`sk_test_...`). The script refuses anything else. This stack has not run a live Stripe payment.

## Files

| Path | Purpose |
| --- | --- |
| `compose.yaml` | The stack. |
| `.env.example` | Every variable the stack reads, with placeholder values. |
| `postgres/10-databases.sh` | Creates the databases and roles. |
| `keycloak/Dockerfile` | The documented Keycloak build step. |
| `keycloak/zs-reference-realm.json` | Realm imported at first start. |
| `db/prepare-killbill.sh`, `db/export-kaui-ddl.sh`, `db/apply-ddl.sh` | Schema extraction and loading. |
| `killbill/bootstrap.sh` | Tenant, catalogue and payment plugin. |
| `killbill/catalogue.xml` | The test catalogue. |
