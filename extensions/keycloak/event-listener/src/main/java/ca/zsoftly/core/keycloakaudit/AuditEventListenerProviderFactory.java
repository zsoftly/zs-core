package ca.zsoftly.core.keycloakaudit;

import java.net.URI;
import java.time.Duration;
import java.util.Map;
import org.keycloak.Config;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventListenerProviderFactory;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;

/** Keycloak 26.8.1 listener factory. Configuration is deployment-owned. */
public final class AuditEventListenerProviderFactory implements EventListenerProviderFactory {
    private Delivery delivery;

    @Override public EventListenerProvider create(KeycloakSession session) { return new AuditEventListenerProvider(session, delivery); }
    @Override public void init(Config.Scope config) {
        URI endpoint = URI.create(required(config, "endpoint"));
        String sourceId = required(config, "source-id");
        Map<String, byte[]> secrets = Delivery.parseRealmSecrets(required(config, "realm-secrets"));
        delivery = new Delivery(endpoint, sourceId, secrets,
                config.getInt("queue-capacity", 1024), config.getInt("queue-bytes", 1 << 20),
                config.getInt("max-attempts", 5), Duration.ofSeconds(config.getInt("delivery-deadline-seconds", 60)));
    }
    @Override public void postInit(KeycloakSessionFactory factory) { }
    @Override public void close() { if (delivery != null) delivery.close(); }
    @Override public String getId() { return "zs-audit"; }
    private static String required(Config.Scope c, String name) { String value = c.get(name); if (value == null || value.isBlank()) throw new IllegalArgumentException("zs-audit " + name + " is required"); return value; }
}
