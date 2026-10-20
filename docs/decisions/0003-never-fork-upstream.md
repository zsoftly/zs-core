# 0003. Never fork Keycloak or Kill Bill; extend through supported extension points only

Status: accepted, 2026-10-20.

## Context

A fork is the fastest way to add a feature and the slowest way to take the next security release. Both upstreams publish extension points designed for exactly what Core needs: Keycloak has service provider interfaces, themes and the Admin API; Kill Bill has a plugin API, per-tenant configuration, catalogue upload and push notifications.

## Decision

- Core consumes the upstream release artifacts unchanged, pinned by version and image digest in `components.yaml`.
- Everything Core adds is a Keycloak SPI implementation, a Keycloak theme, a Kill Bill plugin, or configuration applied through the public APIs. Each lives under `extensions/` with its own build, tests and compatibility range.
- An upstream bug is reported upstream. Until it is fixed, Core works around it in an extension or in configuration, and the workaround is recorded in `docs/upgrades/` with the upstream issue link and the version where it can be removed.
- Each Core release names the component versions it was tested against. Changing a component version is a Core release, with the upgrade runbook run on the reference stack first.

## Consequences

- Core can take a Keycloak or Kill Bill security release within days, because nothing has to be re-merged.
- Some wants will be refused or deferred because no extension point exists. That is the price, and it is cheaper than a fork.
