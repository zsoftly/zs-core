package ca.zsoftly.core.keycloakaudit;

import org.keycloak.events.Event;
import org.keycloak.events.EventListenerProvider;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;

/** Copies an allowlisted envelope and enqueues it only after source commit. */
final class AuditEventListenerProvider implements EventListenerProvider {
    private final KeycloakSession session;
    private final Delivery delivery;
    private final AfterCompletion tx;
    AuditEventListenerProvider(KeycloakSession session, Delivery delivery) {
        this.session = session; this.delivery = delivery;
        this.tx = new AfterCompletion(delivery);
        session.getTransactionManager().enlistAfterCompletion(tx);
    }
    @Override public void onEvent(Event event) { if (allowed(event.getType())) tx.add(Wire.user(delivery.sourceId(), realmName(event.getRealmId(), event.getRealmName()), event)); }
    @Override public void onEvent(AdminEvent event, boolean ignored) { tx.add(Wire.admin(delivery.sourceId(), realmName(event.getRealmId(), event.getRealmName()), event)); }
    @Override public void close() { }
    private String realmName(String realmId, String fallback) {
        RealmModel realm = realmId == null ? null : session.realms().getRealm(realmId);
        return realm == null ? fallback : realm.getName();
    }
    private static boolean allowed(EventType type) {
        return type == EventType.LOGIN || type == EventType.LOGIN_ERROR || type == EventType.LOGOUT || type == EventType.REGISTER || type == EventType.UPDATE_PASSWORD || type == EventType.UPDATE_EMAIL || type == EventType.RESET_PASSWORD;
    }
}
