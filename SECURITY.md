# Security policy

## Reporting a vulnerability

Do not open a public GitHub issue for a security problem.

Report it privately by email to [security@zsoftly.ca](mailto:security@zsoftly.ca) or through [GitHub Security Advisories](https://github.com/zsoftly/zs-core/security/advisories/new).

We acknowledge receipt within 3 business days and work with you on a fix and a coordinated disclosure.

## Scope

This policy covers the ZSoftly Core modules and the source code in this repository.

It does not cover Keycloak or Kill Bill themselves. Report those upstream. It does not cover ZSoftly hosted services. Report those to [security@zsoftly.ca](mailto:security@zsoftly.ca).

## Supported versions

| Version | Security fixes |
| --- | --- |
| `main` before the first stable release | Yes. Fixes land on `main` and there are no backports. |
| Latest stable minor release (for example 1.4.x) | Yes, as a patch release. |
| Older minor releases (for example 1.3.x) | No. Upgrade to the latest minor release. |
| Pre-release tags (`-alpha`, `-beta`, `-rc`) | No. They exist for testing and are not maintained after the stable release they precede. |

"Latest stable" means the highest released version that is not a pre-release. If more than one major line is ever maintained, this table will list each one.
