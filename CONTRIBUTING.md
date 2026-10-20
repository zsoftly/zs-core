# Contributing

Thank you for your interest in ZSoftly Core.

## Contributions are not open yet

ZSoftly Core is public so that anyone can read the design, follow the work, and self-host it under Apache 2.0. While the platform is under active development, issues are closed and pull requests are limited to the maintainers. External contributions will open once the first module contracts are stable, and this file will say so when they do.

Until then:

- Security problems go to [security@zsoftly.ca](mailto:security@zsoftly.ca) or a [private advisory](https://github.com/zsoftly/zs-core/security/advisories/new). See [SECURITY.md](SECURITY.md).
- Questions and ideas go to [contact@zsoftly.ca](mailto:contact@zsoftly.ca).

The rules below apply to the maintainers now and to everyone once contributions open.

## Pull requests

1. Claim an issue first by commenting on it. Open a new issue to discuss any change that does not have one.
2. Fork the repository and create a branch from `main`.
3. Keep the pull request on one issue. Link it with `Part of #<number>`.
4. Ship every feature with the six things listed in the README: API and OpenAPI documentation, a configuration schema, extension hooks, tenant isolation and permission tests, examples, and a migration plan.
5. Never hardcode branding or business rules. Never modify upstream Keycloak or Kill Bill.
6. Sign off every commit (see below).

Pull requests are squash-merged. The review target is an initial response within 2 business days.

## Developer Certificate of Origin

Every commit must carry a `Signed-off-by` line that matches the author, as defined by the [Developer Certificate of Origin 1.1](https://developercertificate.org/). The sign-off certifies that you wrote the change, or have the right to submit it, under the Apache 2.0 license. Commits without it are not merged, and commits made in the GitHub web editor require it too.

```bash
git commit -s
```

Commits are authored by people. Do not add co-author or generated-by trailers for tools.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
