# Kill Bill upgrade runbook

## TL;DR

Core pins one Kill Bill version in `components.yaml`. You change it only as part of a Core release, after the step has been rehearsed on the reference stack and recorded in the table below. The routine has seven steps:

1. Check what is new with `zs-core upgrade check`.
2. Read the release notes.
3. Compare the schema files of the two versions.
4. Back up the `killbill` database.
5. Change the tag.
6. Recreate the container.
7. Run `zs-core doctor` and the state check.

Kill Bill does not create or change its schema on its own in this stack, so apply any schema migration before the new version starts. Rolling back means restoring the database backup and the old tag. The current pin, 0.24.22, was rehearsed from 0.24.21, and the two versions ship identical schema files.

## Supported path

- Core supports one step at a time: a patch release within the 0.24 line (0.24.21 to 0.24.22). Skipping patch releases in the same line is fine.
- A move to a new minor line (0.25) needs its own rehearsal row and a reading of the upstream migration notes first. Do not skip a minor line.
- Plugins are versioned separately. Their versions are pinned in `components.yaml` under `killbill_plugins`, and a plugin version change is a Core release with its own rehearsal row.

## Pre-checks

1. The stack you rehearse on is green: `make doctor` prints no `FAIL` line.
2. List the candidates: `zs-core upgrade check`.
3. Read the upstream release notes for every version between the pin and the target.
4. Pull the target image on the build host and note its digest for `components.yaml`.
5. Compare the schema files. The stack extracts them from each image into the `ddl` volume, so the cheapest comparison is to bring the stack up on the target tag once, copy `/ddl/killbill/*.sql` out of the volume, and `diff -r` it against the copy taken on the current tag. The result decides what happens next:
   - Identical files mean no schema step.
   - Different files mean the upstream migration notes tell you which statements to apply to the live `killbill` database before the new version starts.
   - Record that step in the rehearsal row.
6. Check the plugins: `zs-core doctor` lists each installed plugin with its version and state. Confirm every installed plugin supports the target Kill Bill version before you start.

## Back up

Run on the build host, from the repository root:

```bash
deploy/compose/rehearsal/backup.sh /tmp/zs-core-backup
```

It dumps the `keycloak` and `killbill` databases and proves the Keycloak dump restores. For Kill Bill, also keep the plugin directory: the `killbill-plugins` volume holds installed plugin jars, and they are not in the database dump.

## Steps on the reference stack

1. Record the state: `deploy/compose/rehearsal/check.sh > before.txt`.
2. Edit `deploy/compose/compose.yaml` (the `KILLBILL_IMAGE` default) and `components.yaml` to the new tag and digest. For a rehearsal that starts on the old version, set `KILLBILL_IMAGE` in `.env` to the old tag first, bring the stack up, run `make stack-bootstrap` and `deploy/compose/rehearsal/seed.sh`, then remove that line to move to the pinned version.
3. Run `make stack-up`. `killbill-prep` runs again against the new image and refreshes the schema files. `db-init` finds the schema already present and leaves it alone. Compose recreates only the `killbill` container. `make stack-up` returned after about 37 seconds with Kill Bill healthy.
4. Run `make stack-bootstrap`. It confirms the tenant and catalogue are still there.

## Post-checks

1. `make doctor` is green: health, version against the pin, and each installed plugin against its pin.
2. `deploy/compose/rehearsal/check.sh > after.txt` and `diff before.txt after.txt`. Only the server version line differs. The tenant, catalogue plan, account, subscription and row counts must match.
3. Open KAUI at `http://127.0.0.1:9090` through the SSH forward, choose the `zs-reference` tenant and open the rehearsal account.
4. Run the Phase 4 gate tests against the stack once they exist.

## Roll back

1. `make stack-down`.
2. Put the old tag back in `compose.yaml` and `components.yaml`.
3. If the new version changed the schema, restore the `killbill` database from the backup (`pg_restore --no-owner` into an empty database, which on the reference stack means `make stack-reset` first). If it did not, the old image runs against the same data, and a restore is not needed.
4. `make stack-up`, then `make doctor`.

Anything written after the backup is lost on a restore. Decide the cut-off before you start.

## Plugin notes

KPM works on the pinned image for Java plugins and installs them while Kill Bill runs. The details, and why the Stripe 6.x Ruby line cannot install this way, are in [../../deploy/compose/README.md](../../deploy/compose/README.md). Any plugin you install by hand stays in the `killbill-plugins` volume across upgrades, so `zs-core doctor` is the check that it still matches its pin afterwards.

## Rehearsals

A component version changes in `components.yaml` only with a row here for that step.

| Date | From | To | Outcome | Notes |
| --- | --- | --- | --- | --- |
| 2026-10-20 | 0.24.21 | 0.24.22 | Pass | Reference stack on the build host. Started on 0.24.21, ran `make stack-bootstrap` to create the `zs-reference` tenant and catalogue, then created one account (CAD) with a subscription to `core-test-monthly` through the API (state ACTIVE, phase TRIAL). The 12 schema files exported from the two images were identical, so no schema step was needed. Upgraded by changing the tag and running `make stack-up`. `killbill-prep` and `killbill` were recreated, `db-init` reported the schema already present, and Kill Bill 0.24.22 became healthy. Account, subscription, catalogue plan and tenant were unchanged, and the row counts matched. `zs-core doctor` showed `FAIL killbill: version 0.24.21 does not match the pin 0.24.22` before the upgrade and was green after it. |
