# Keycloak audit listener

Build this JDK 21 provider before the reference-stack image:

```sh
cd extensions/keycloak/event-listener
mvn -B verify
```

The compose Keycloak Dockerfile copies
`target/keycloak-audit-listener-1.0.0.jar` before running `kc.sh build` on the
pinned Keycloak 26.8.1 image. Its service-provider descriptor names the
`zs-audit` event listener.

The provider reads deployment settings only: `endpoint`, `source-id`, and
`realm-secrets`. `realm-secrets` is `realm=secret;realm=secret`. No secret is
stored in a realm attribute, manifest, image layer or log. Retried events keep
their original native ID and body bytes while each HTTP attempt signs a new
delivery timestamp.
