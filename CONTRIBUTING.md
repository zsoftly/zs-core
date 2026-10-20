# Contributing

Thank you for your interest in ZSoftly Core.

## Reporting issues

Use [GitHub Issues](https://github.com/zsoftly/zs-core/issues) to report bugs or propose changes. The issue templates ask for the details a maintainer needs.

## Pull requests

1. Claim an issue first by commenting on it. Open a new issue to discuss any change that does not have one.
2. Fork the repository and create a branch from `main`.
3. Keep the pull request on one issue. Link it with `Part of #<number>`.
4. Ship every feature with the six things listed in the README: API and OpenAPI documentation, a configuration schema, extension hooks, tenant isolation and permission tests, examples, and a migration plan.
5. Never hardcode branding or business rules. Never modify upstream Keycloak or Kill Bill.
6. Sign off every commit (see below).

Pull requests are squash-merged. The review target is an initial response within 2 business days.

## Developer Certificate of Origin

Contributions are accepted under the [Developer Certificate of Origin 1.1](https://developercertificate.org/). Sign off each commit to certify that you wrote the change or have the right to submit it under the Apache 2.0 license:

```bash
git commit -s
```

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
