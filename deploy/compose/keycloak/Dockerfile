# Runs the build step Keycloak documents for production mode, so the server
# starts with --optimized. The upstream image is used unchanged: this adds no
# files other than the build output kc.sh writes itself.
ARG KEYCLOAK_IMAGE
FROM ${KEYCLOAK_IMAGE} AS builder
ENV KC_DB=postgres \
    KC_HEALTH_ENABLED=true \
    KC_METRICS_ENABLED=true
RUN /opt/keycloak/bin/kc.sh build

FROM ${KEYCLOAK_IMAGE}
COPY --from=builder /opt/keycloak/ /opt/keycloak/
ENTRYPOINT ["/opt/keycloak/bin/kc.sh"]
