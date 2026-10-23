package ca.zsoftly.core.keycloakaudit;

import java.nio.charset.StandardCharsets;
import org.keycloak.events.Event;
import org.keycloak.events.admin.AdminEvent;

/** Builds the fixed, scalar-only v1 payload. */
final class Wire {
    private Wire() { }
    static Delivery.Item user(String source, String realmName, Event event) {
        return item(source, event.getId(), event.getRealmId(), realmName, "user", event.getTime(), event.getError() != null,
            "event_type", event.getType().name(), "subject_id", event.getUserId(), "client_id", event.getClientId());
    }
    static Delivery.Item admin(String source, String realmName, AdminEvent event) {
        return item(source, event.getId(), event.getRealmId(), realmName, "admin", event.getTime(), event.getError() != null,
            "operation", event.getOperationType().name(), "resource_type", event.getResourceTypeAsString(),
            "admin_actor_realm_id", event.getAuthDetails() == null ? null : event.getAuthDetails().getRealmId(),
            "admin_actor_user_id", event.getAuthDetails() == null ? null : event.getAuthDetails().getUserId(),
            "admin_actor_client_id", event.getAuthDetails() == null ? null : event.getAuthDetails().getClientId());
    }
    private static Delivery.Item item(String source, String id, String realmID, String realmName, String kind, long at, boolean failed, String... pairs) {
        if (!identifier(source, 128) || !valid(id, 128) || !valid(realmID, 255) || !valid(realmName, 63)) return null;
        StringBuilder json = new StringBuilder("{\"version\":1");
        field(json, "source_id", source); field(json, "event_id", id); field(json, "realm_id", realmID); field(json, "realm_name", realmName);
        field(json, "kind", kind); json.append(",\"occurred_at_ms\":").append(at).append(",\"failed\":").append(failed);
        for (int i = 0; i < pairs.length; i += 2) {
            if (pairs[i + 1] == null) continue;
            if (!valid(pairs[i + 1], limit(pairs[i]))) return null;
            field(json, pairs[i], pairs[i + 1]);
        }
        json.append('}'); byte[] body = json.toString().getBytes(StandardCharsets.UTF_8); return body.length <= 16 * 1024 ? new Delivery.Item(realmName, body) : null;
    }
    private static void field(StringBuilder b, String key, String value) { b.append(",\"").append(key).append("\":\""); for (int i = 0; i < value.length(); i++) { char c = value.charAt(i); if (c == '"' || c == '\\') b.append('\\'); b.append(c); } b.append('"'); }
    private static int limit(String key) { return switch (key) { case "event_type" -> 100; case "operation" -> 32; case "resource_type" -> 128; default -> 255; }; }
    private static boolean identifier(String value, int limit) { if (!valid(value, limit)) return false; for (int i = 0; i < value.length(); i++) { char c = value.charAt(i); if (!(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' && c != '-') return false; } return true; }
    private static boolean valid(String value, int limit) { if (value == null || value.isEmpty() || !wellFormed(value) || value.getBytes(StandardCharsets.UTF_8).length > limit) return false; for (int i = 0; i < value.length(); i++) if (value.charAt(i) < 0x20 || value.charAt(i) == 0x7f) return false; return true; }
    private static boolean wellFormed(String value) { for (int i = 0; i < value.length(); i++) { char c = value.charAt(i); if (Character.isHighSurrogate(c)) { if (i + 1 == value.length() || !Character.isLowSurrogate(value.charAt(++i))) return false; } else if (Character.isLowSurrogate(c)) return false; } return true; }
}
