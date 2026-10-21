# Kill Bill upgrade runbook

## TL;DR

Core pins one Kill Bill version in `components.yaml`. You change it only as part of a Core release, after the step has been rehearsed on the reference stack and recorded in the table below. Follow these eight steps:

1. Check what is new with `zs-core upgrade check`.
2. Read the release notes.
3. Review the target Logback configuration, startup selection and plugin compatibility before any target service starts.
4. Compare the schema files of the two versions.
5. Back up the `killbill` database.
6. Change the image tag and digest together with its reviewed Logback configuration.
7. Recreate the container.
8. Run `zs-core doctor`, the state check and the logging proof.

Kill Bill does not create or change its schema on its own in this stack, so apply any schema migration before the new version starts. Rolling back means restoring the database backup, the old image tag and digest, and its matching reviewed Logback configuration, then repeating the logging proof. The current pin, 0.24.22, was rehearsed from 0.24.21 on 2026-10-20, and the two versions ship identical schema files.

## Supported path

- Core supports one step at a time: a patch release within the 0.24 line (0.24.21 to 0.24.22). Skipping patch releases in the same line is fine.
- A move to a new minor line (0.25) needs its own rehearsal row and a reading of the upstream migration notes first. Do not skip a minor line.
- Plugins are versioned separately. Their versions are pinned in `components.yaml` under `killbill_plugins`, and a plugin version change is a Core release with its own rehearsal row.

## Pre-checks

1. The stack you rehearse on is green: `make doctor` prints no `FAIL` line.
2. List the candidates: `zs-core upgrade check`.
3. Read the upstream release notes for every version between the pin and the target.
4. Pull the target image on the build host and note its digest for `components.yaml`.
5. Extract and review the target image's effective Logback configuration and how that image selects it at startup. Preserve its appenders, options and other startup settings, then reapply only the approved logging controls to the checked-in copy. Review that copy and its Compose selection against the target image digest before starting it. This also applies to `KILLBILL_IMAGE` overrides and old-image rehearsal runs. Do not reuse the 0.24.22 logging baseline under another image without this review. Do not start any target service, including for schema extraction, until this review and configuration selection are complete.
6. Check the plugins: `zs-core doctor` lists each installed plugin with its version and state. Confirm every installed plugin supports the target Kill Bill version before you start.
7. Compare the schema files. The stack extracts them from each image into the `ddl` volume, so the cheapest comparison is to bring the stack up on the target tag once, copy `/ddl/killbill/*.sql` out of the volume, and `diff -r` it against the copy taken on the current tag. The result decides what happens next:
   - Identical files mean no schema step.
   - Different files mean the upstream migration notes tell you which statements to apply to the live `killbill` database before the new version starts.
   - Record that step in the rehearsal row.

## Back up

Run on the build host, from the repository root:

```bash
deploy/compose/rehearsal/backup.sh /tmp/zs-core-backup
```

It dumps the `keycloak` and `killbill` databases and proves the Keycloak dump restores. For Kill Bill, also keep the plugin directory: the `killbill-plugins` volume holds installed plugin jars, and they are not in the database dump.

## Steps on the reference stack

1. Record the state: `deploy/compose/rehearsal/check.sh > before.txt`.
2. Edit `deploy/compose/compose.yaml` (the `KILLBILL_IMAGE` default) and `components.yaml` to the new tag and digest. For a rehearsal that starts on the old version, set `KILLBILL_IMAGE` in `.env` to the old tag first, bring the stack up, run `make stack-bootstrap` and `deploy/compose/rehearsal/seed.sh`, then remove that line to move to the pinned version.
3. Run `make stack-up`. `killbill-prep` runs again against the new image and refreshes the schema files. `db-init` finds the schema already present and leaves it alone. Compose recreates only the `killbill` container. Wait for Kill Bill to become healthy.
4. Run `make stack-bootstrap`. It confirms the tenant and catalogue are still there.

## Post-checks

1. `make doctor` is green: health, version against the pin, and each installed plugin against its pin.
2. `deploy/compose/rehearsal/check.sh > after.txt` and `diff before.txt after.txt`. Only the server version line differs. The tenant, catalogue plan, account, subscription and row counts must match.
3. Open KAUI at `http://127.0.0.1:9090` through the SSH forward, choose the `zs-reference` tenant and open the rehearsal account.
4. Run the Phase 4 gate tests against the stack: `TestBillingOnTheStack` and `TestExtensionsOnTheStack` in `internal/stacktest`. See [../development.md](../development.md#tests-against-the-reference-stack).
5. Check the usage behaviours the extensions rely on. A new version could change them:
   - Kill Bill refuses a known tracking ID per subscription with HTTP 400 and error code 50000.
   - Kill Bill leaves usage dated before a subscription's start off the invoice.
   - `GET /1.0/kb/usages/{subscriptionId}` needs both dates.
   - A `TARGET_DATE` dry-run invoice a month and a day ahead returns the unbilled usage, and the applied credit as a separate `CBA_ADJ` item.

   The extensions stack test covers the tracking ID refusal, the dry-run amount and the invoice of forwarded usage. Check the other two by hand. See [../modules/metering.md](../modules/metering.md#what-kill-bill-does-with-it).

Repeat the logging proof on the upgraded image before accepting the rehearsal: verify the effective configuration selection, preserve expected normal logging and check every configured output for the synthetic SMTP-credential and mirror-token sentinels. Use synthetic values only. Record the image digest, reviewed configuration hash and proof results with the rehearsal. Startup success alone does not prove suppression.

## Roll back

1. `make stack-down`.
2. Restore the old image tag and digest in `compose.yaml` and `components.yaml`, together with the reviewed Logback copy and startup selection for that exact image. If that image lacks a reviewed copy, extract its effective configuration, preserve its appenders and options, reapply only the approved controls and review the result before starting it.
3. If the new version changed the schema, restore the `killbill` database from the backup (`pg_restore --no-owner` into an empty database, which on the reference stack means `make stack-reset` first). If it did not, the old image runs against the same data, and a restore is not needed.
4. `make stack-up`, then `make doctor`. Repeat the same configuration-selection and synthetic all-output logging proof on the rollback image, and record its digest and configuration hash. A successful upgrade proof does not establish rollback logging safety.

Anything written after the backup is lost on a restore. Decide the cut-off before you start.

## Plugin notes

KPM works on the pinned image for Java plugins and installs them while Kill Bill runs. The details, and why the Stripe 6.x Ruby line cannot install this way, are in [../../deploy/compose/README.md](../../deploy/compose/README.md). Any plugin you install by hand stays in the `killbill-plugins` volume across upgrades, so `zs-core doctor` is the check that it still matches its pin afterwards.

Rehearse a plugin version change in `components.yaml` with these steps on the reference stack. Record the outcome in a new rehearsal row before releasing the updated pin:

1. Update the version, the `sha256` from Maven Central and the `tables` of the new jar. `zs-core plugin ddl show <name>` prints the new DDL. Compare it with the old one.
2. Run `zs-core plugin mirror sync`. It downloads the new jar, checks it and signs the manifest again.
3. Run `zs-core plugin ddl apply --operator <you> <name>`. It creates the tables the new version adds. It never changes a table that exists. When the new DDL changes an existing table, apply that change from the plugin's own migration notes as a separate, reviewed step, and record it in the rehearsal row.
4. Uninstall the old version with `STOP_PLUGIN` and `UNINSTALL_PLUGIN`, then run brand apply. It installs the pinned version from the mirror.
5. Run `zs-core doctor`. It fails while any node runs another version than the pin, or while a table of the new version is missing.

See [../modules/plugins.md](../modules/plugins.md).

### Email-notifications configuration restart

The prior Phase 5a rehearsal observed that a brand whose tenant had invoices before its email configuration was uploaded got no email, and a `RESTART_PLUGIN` node command made the configuration take effect. This is historical evidence only. The pinned plugin source includes configuration-change reload handling, so the cause is unknown. After an email configuration change, Core conservatively sends `STOP_PLUGIN`, waits until every original target reports the pinned plugin `STOPPED`, sends `START_PLUGIN`, then waits until that same target set reports the pinned plugin `RUNNING`. A missing target or either failed observation leaves the marker pending and makes apply fail for retry. These runtime semantics require QA acceptance. Core applies sharing the Core database are serialized, but this does not fence external operators, writers using another Core database, or late accepted commands. An uncertain accepted-command timeout requires operator reconciliation of the engine and plugin state before retrying apply. Delivery during restart is unverified; do not rely on replay.

## Rehearsals

A component version changes in `components.yaml` only with a row here for that step.

| Date | From | To | Outcome | Notes |
| --- | --- | --- | --- | --- |
| 2026-10-20 | 0.24.21 | 0.24.22 | Pass | Reference stack on the build host. Started on 0.24.21, ran `make stack-bootstrap` to create the `zs-reference` tenant and catalogue, then created one account (CAD) with a subscription to `core-test-monthly` through the API (state ACTIVE, phase TRIAL). The 12 schema files exported from the two images were identical, so no schema step was needed. Upgraded by changing the tag and running `make stack-up`. `killbill-prep` and `killbill` were recreated, `db-init` reported the schema already present, and Kill Bill 0.24.22 became healthy. Account, subscription, catalogue plan and tenant were unchanged, and the row counts matched. `zs-core doctor` showed `FAIL killbill: version 0.24.21 does not match the pin 0.24.22` before the upgrade and was green after it. |

This row records the prior rehearsal. It does not establish stack acceptance of the current takeover revisions, which require a new run.
