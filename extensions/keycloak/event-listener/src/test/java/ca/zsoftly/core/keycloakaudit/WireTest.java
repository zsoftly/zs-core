package ca.zsoftly.core.keycloakaudit;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;

import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.Map;
import org.junit.Test;
import org.keycloak.events.Event;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.events.admin.AuthDetails;
import org.keycloak.events.admin.OperationType;

public class WireTest {
    @Test public void emitsTheSevenAllowedUserEventShapesWithoutPrivateFields() {
        for (EventType type : List.of(EventType.LOGIN, EventType.LOGIN_ERROR, EventType.LOGOUT, EventType.REGISTER, EventType.UPDATE_PASSWORD, EventType.UPDATE_EMAIL, EventType.RESET_PASSWORD)) {
            Event event = user(type);
            String json = json(Wire.user("keycloak", "realm", event));
            assertFalse(json, json.contains("email@example.test"));
            assertFalse(json, json.contains("203.0.113.11"));
            assertFalse(json, json.contains("session-secret"));
            assertFalse(json, json.contains("provider-error"));
            assertFalse(json, json.contains("details-secret"));
            assertFalse(json, json.contains("representation"));
            assertFalse(json, json.contains("resource_path"));
        }
    }

    @Test public void emitsAdminActorIdsButExcludesAdminPrivateFields() {
        AdminEvent event = new AdminEvent();
        event.setId("event-id"); event.setTime(1); event.setRealmId("realm-id"); event.setRealmName("realm"); event.setOperationType(OperationType.UPDATE); event.setResourceTypeAsString("USER");
        event.setRepresentation("representation-secret"); event.setResourcePath("realms/realm/users/resource-secret"); event.setError("provider-error"); event.setDetails(Map.of("email", "email@example.test"));
        AuthDetails actor = new AuthDetails(); actor.setRealmId("actor-realm"); actor.setUserId("actor-user"); actor.setClientId("actor-client"); actor.setIpAddress("203.0.113.11"); event.setAuthDetails(actor);
        String json = json(Wire.admin("keycloak", "realm", event));
        assertFalse(json, json.contains("representation-secret"));
        assertFalse(json, json.contains("resource-secret"));
        assertFalse(json, json.contains("provider-error"));
        assertFalse(json, json.contains("email@example.test"));
        assertFalse(json, json.contains("203.0.113.11"));
        assertFalse(json, json.contains("details"));
    }

    @Test public void refusesMalformedNonemptyOptionalProvenance() {
        Event event = user(EventType.LOGIN); event.setClientId("bad\u0000client");
        assertFalse(Wire.user("keycloak", "realm", event) != null);
    }

    private static Event user(EventType type) {
        Event event = new Event();
        event.setId("event-id"); event.setTime(1); event.setType(type); event.setRealmId("realm-id"); event.setRealmName("realm"); event.setUserId("subject-id"); event.setClientId("client-id");
        event.setSessionId("session-secret"); event.setIpAddress("203.0.113.11"); event.setError("provider-error"); event.setDetails(Map.of("email", "email@example.test", "detail", "details-secret"));
        return event;
    }
    private static String json(Delivery.Item item) { assertNotNull(item); return new String(item.body(), StandardCharsets.UTF_8); }
}
