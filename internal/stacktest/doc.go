// Package stacktest holds the integration tests that need the reference
// stack: a running Keycloak and Kill Bill, as started by make stack-up and
// make stack-bootstrap. They skip unless ZS_TEST_KEYCLOAK_URL is set. They
// change the stack (realms, clients, users, tenants), so run them on a build
// host stack that is reset afterwards, never on a shared environment. See
// docs/development.md.
package stacktest
