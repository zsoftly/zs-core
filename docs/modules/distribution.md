# Distribution, upgrades and releases

## TL;DR

A Core release is a tested bundle: pinned Keycloak, Kill Bill, KAUI and Postgres images, the Core extensions built against them, the Core API, and the upgrade notes. `components.yaml` is the single place versions live. `deploy/compose` brings the reference stack up from one command. `zs-core doctor` proves a running stack matches the release. `docs/upgrades/` holds one runbook per component, with the rehearsal record for each version step. Products upgrade by taking a Core release, never by bumping a component on their own. This is the Phase 2 contract.

## Components

See `components.yaml`. Each entry pins a version, an image reference, and the upstream source. Images carry a tag and a digest in `components.yaml`. The compose file uses the tag.

## Reference stack

`deploy/compose/compose.yaml` runs: Postgres 18 (one instance, separate databases for Keycloak, Kill Bill, KAUI and Core), Keycloak 26.8.1 in production mode behind its own hostname settings, Kill Bill 0.24.22 with KPM and the bundled plugins, KAUI 4.0.31, and Core. Mailpit catches email. Everything binds to loopback. Run `make stack-up` to start it and `make stack-down` to stop it. `make stack-reset` wipes data.

Keycloak: the stack imports a `zs-reference` realm from `deploy/compose/keycloak/`. It has the customer and staff clients and the `en` and `fr` locales, so the token tests have a real issuer. Kill Bill: the stack creates a `zs-reference` tenant with the test catalogue and the Stripe plugin in test mode when `STRIPE_TEST_SECRET_KEY` is present, otherwise with the built-in `__EXTERNAL_PAYMENT__` plugin.

## Doctor

`zs-core doctor` connects to each component and checks each of these against `components.yaml` where it applies:

- Database: reachable, with the Postgres major version.
- Migrations: applied (Core).
- Keycloak: version and realm present.
- Kill Bill: version, plugins installed and their versions.
- KAUI: reachable. It does not expose its version.
- Clock skew.
- Version match for Keycloak and Kill Bill.

Any mismatch is a non-zero exit with one line per finding. `doctor --json` is for automation.

## Upgrades

`docs/upgrades/keycloak.md` and `docs/upgrades/killbill.md` each hold: the supported upgrade path (which versions can be stepped over), pre-checks, backup, the upgrade steps on the reference stack, post-checks (doctor plus the Phase 3 and Phase 4 gate tests), rollback, and a rehearsal table with one row per version step the Core team has run (date, from, to, outcome, notes). A component version changes in `components.yaml` only with a rehearsal row for that step.

`zs-core upgrade check` compares `components.yaml` with upstream release feeds and prints newer versions with their release dates and whether a security advisory references them. It never changes anything.

## Releases

A Core release is a git tag `vX.Y.Z` on main whose `components.yaml` has `core_release` equal to the tag. The release notes list component versions, extension versions, API changes with migration notes, and the upgrade runbooks that apply. Releases are immutable on GitHub (repository setting). Patch releases carry only security and bug fixes; a component version change is at least a minor release.

## Tests that gate Phase 2

- `make stack-up` on a clean machine produces a stack where `zs-core doctor` is green within five minutes.
- `doctor` fails with a clear line when a component tag is changed in the compose file without changing `components.yaml`.
- The Keycloak and Kill Bill upgrade runbooks have each been rehearsed once on the reference stack for the current pinned versions, from the previous patch release, with rows in their rehearsal tables.
