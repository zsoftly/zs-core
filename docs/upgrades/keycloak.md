# Keycloak upgrade runbook

## TL;DR

Core pins one Keycloak version in `components.yaml`. You change it only as part of a Core release, and only after the step has been rehearsed on the reference stack and recorded in the table below. The routine is the same every time. Check what is new with `zs-core upgrade check`. Read the upstream release notes. Back up the `keycloak` database and prove the backup restores. Change the tag, rebuild, let Keycloak migrate its own schema, then run `zs-core doctor` and the state check. Rolling back means restoring the database backup and the old tag, because Keycloak does not downgrade a schema. The current pin, 26.8.1, was rehearsed from 26.8.0.

## Supported path

- Core supports one step at a time: a patch release within a minor line (26.8.0 to 26.8.1), or from the last patch of one minor line to the first release of the next.
- Skipping patch releases inside a minor line is fine. Skipping a minor line is not supported until a rehearsal row shows the same jump.
- A major version change is a separate project with its own runbook.
- Patch releases carry only security and bug fixes, so they can go into a Core patch release. A new minor line is a minor Core release (see [../releases.md](../releases.md)).

## Pre-checks

1. The stack you rehearse on is green: `make doctor` on the build host prints no `FAIL` line.
2. List the candidates: `zs-core upgrade check`. It prints newer versions with their release dates and never changes anything.
3. Read the upstream release notes and the upgrade guide for every version between the pin and the target. Look for removed options, changed defaults and required manual steps. Record anything that affects `deploy/compose/keycloak/` or the realm JSON.
4. Pull the target image on the build host and note its digest. The digest goes into `components.yaml` with the tag.
5. Check that the realm import file and the `Dockerfile` build step still match the target's documented options.

## Back up

Run on the build host, from the repository root:

```bash
deploy/compose/rehearsal/backup.sh /tmp/zs-core-backup
```

It writes `keycloak.dump` and `killbill.dump`, then restores the Keycloak dump into a scratch database and drops it again. A backup that has not been restored once is not a backup. For a production Keycloak, take the same `pg_dump -Fc` of the `keycloak` database and keep the old image digest next to it.

## Steps on the reference stack

1. Record the state: `deploy/compose/rehearsal/check.sh > before.txt`.
2. Edit `deploy/compose/compose.yaml` (the `KEYCLOAK_IMAGE` default) and `components.yaml` to the new tag and digest. For a rehearsal that starts on the old version, set `KEYCLOAK_IMAGE` in `.env` to the old tag first, bring the stack up, seed it, then remove that line to move to the pinned version.
3. Run `make stack-up`. Compose rebuilds the `zs-core/keycloak:local` image from the new base image and recreates only the `keycloak` container. Keycloak applies its schema changes at start. Watch `make stack-logs` until the log says Keycloak started.
4. Wait for the `keycloak` healthcheck. `make stack-up` already does.

## Post-checks

1. `make doctor` is green. It reads the version from the Admin API and compares it with the pin, and confirms the `zs-reference` realm exists.
2. `deploy/compose/rehearsal/check.sh > after.txt` and `diff before.txt after.txt`. Only the server version line may differ. The user, realm, locales, client credentials grant and row counts must match.
3. Run the Phase 3 and Phase 4 gate tests against the stack once they exist.
4. Sign in to the Keycloak admin console at `http://127.0.0.1:8180` through the SSH forward and look for warnings about deprecated features.

## Roll back

Keycloak does not run against a schema written by a newer version, and it has no downgrade path. To roll back:

1. `make stack-down` (data stays in the volume).
2. Put the old tag back in `compose.yaml` and `components.yaml`.
3. Restore the backup into an empty database. On the reference stack that means `make stack-reset` and a restore of `keycloak.dump`, or restoring into a fresh `keycloak` database with `pg_restore --no-owner`.
4. `make stack-up`, then `make doctor`.

Anything written to Keycloak after the backup is lost. Decide the cut-off before you start.

## Rehearsals

A component version changes in `components.yaml` only with a row here for that step.

| Date | From | To | Outcome | Notes |
| --- | --- | --- | --- | --- |
| 2026-10-20 | 26.8.0 | 26.8.1 | Pass | Reference stack on the build host. Stack started on 26.8.0 with the `zs-reference` realm imported; one user (French locale, password credential) created through the Admin API. Backup restored into a scratch database with 3 users. Upgrade by changing the tag and rebuilding: only the `keycloak` container was recreated; Keycloak 26.8.1 started in 7.7 seconds. The user kept its ID and attributes, the realm kept its locales, the `zs-staff` client credentials grant still returned a token, and the schema changeset count stayed at 237, so this step needed no schema migration. The rehearsal also found that the Keycloak healthcheck reported healthy while `/realms/master` still answered 503; the healthcheck now checks the HTTP status line. `zs-core doctor` showed `FAIL keycloak: version 26.8.0 does not match the pin 26.8.1` before the upgrade and was green after it. |
