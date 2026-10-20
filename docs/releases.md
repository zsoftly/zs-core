# Releases

## TL;DR

A Core release is a git tag `vX.Y.Z` on `main`. The `core_release` value in `components.yaml` at that tag equals the tag without the `v`. The release bundles the pinned component versions, the Core extensions and API built against them, and the upgrade notes. Products take a Core release and never bump a component on their own. Patch releases carry only security and bug fixes. Changing a component version is at least a minor release, and it needs a rehearsal row in the matching runbook under `docs/upgrades/`. There is no hosted CI, so the gates run locally and on the reference stack, and the person cutting the release records the results in the release notes. Releases are immutable on GitHub (a repository setting).

## What a release contains

- `components.yaml` with exact versions, image references and image digests.
- The Core service and operator commands, built from the tag.
- The reference stack in `deploy/compose`.
- The Keycloak and Kill Bill extensions under `extensions/`, once they exist, each with its compatibility range.
- The OpenAPI document `api/openapi.yaml` and the migrations in `migrations/`.
- The upgrade runbooks in `docs/upgrades/`, with the rehearsal tables.

## Version rules

| Change | Release type |
| --- | --- |
| Security or bug fix in Core, no component version change | Patch |
| New Core feature, backward-compatible API change | Minor |
| Any component version change (Keycloak, Kill Bill, KAUI, Postgres, a pinned plugin) | At least minor, with a rehearsal row for the step |
| Breaking API change, or a component major version change | Major |

A component security patch ships as a Core patch release when the rehearsal shows no schema or configuration change. Say so in the release notes.

## Process

1. Check what is new upstream: `zs-core upgrade check`. It reads `components.yaml` and the upstream release feeds and changes nothing.
2. For each component version that changes, follow its runbook on the reference stack and add the rehearsal row. Update the tag, the digest and the release date in `components.yaml` together, and the matching image default in `deploy/compose/compose.yaml`. A unit test fails if the two disagree.
3. Set `core_release` in `components.yaml` to the new version.
4. Run the gates:
   - `make check` on the development machine.
   - `make test-integration` where Docker is available.
   - On the build host: `make stack-reset`, then `make stack-up`, `make stack-bootstrap` and `make doctor`. `doctor` must print no `FAIL` line, and the stack must be healthy within five minutes of a clean start.
   - The same sequence once with a component tag changed in `compose.yaml` and not in `components.yaml`. `doctor` must fail with a version line for that component.
5. Write the release notes (below).
6. Merge to `main`. Tag `vX.Y.Z` on the merge commit. Push the tag.
7. Create the GitHub release from the tag with the notes. Do not edit a published release. If something is wrong, ship the next patch release.

## Release notes

The notes list, in this order:

- the component versions and image digests, copied from `components.yaml`;
- the extension versions and the component versions each one supports;
- API changes, with migration notes for callers;
- database migrations added since the last release, and whether `zs-core migrate` must run before the new version serves;
- the upgrade runbooks that apply, and the rehearsal rows added;
- known issues, including upstream faults Core works around, each with the upstream issue link and the version that lets you remove the workaround.

## Taking a release in a product

A product upgrades by moving to the new tag. It runs the runbooks for the components that changed, in the order the notes give, on a staging copy first. It never changes a component version on its own.
