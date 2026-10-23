package ca.zsoftly.core.keycloakaudit;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNotNull;

import java.lang.reflect.Proxy;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.Test;
import org.keycloak.events.Event;
import org.keycloak.events.EventType;
import org.keycloak.events.admin.AdminEvent;
import org.keycloak.events.admin.AuthDetails;
import org.keycloak.events.admin.OperationType;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakTransaction;
import org.keycloak.models.KeycloakTransactionManager;
import org.keycloak.models.RealmModel;
import org.keycloak.models.RealmProvider;

public class ProviderCallbackTest {
    private static final byte[] SECRET = "01234567890123456789012345678901".getBytes(StandardCharsets.UTF_8);

    @Test public void callbacksAllowExactlySevenUserTypesAndSendOnlyAfterCommit() throws Exception {
        List<String> payloads = new ArrayList<>();
        CountDownLatch sent = new CountDownLatch(7);
        Delivery delivery = delivery((endpoint, realm, timestamp, signature, body, timeout) -> { payloads.add(new String(body, StandardCharsets.UTF_8)); sent.countDown(); return new Delivery.Response(204, -1); }, 16 * 1024);
        try {
            List<Fixture> fixtures = new ArrayList<>();
            for (EventType type : List.of(EventType.LOGIN, EventType.LOGIN_ERROR, EventType.LOGOUT, EventType.REGISTER, EventType.UPDATE_PASSWORD, EventType.UPDATE_EMAIL, EventType.RESET_PASSWORD)) {
                Fixture fixture = fixture(delivery); fixture.provider.onEvent(user(type)); fixtures.add(fixture);
            }
            assertEquals("no HTTP before commit", 7, sent.getCount());
            for (Fixture fixture : fixtures) { fixture.after.get().begin(); fixture.after.get().commit(); }
            assertEquals(true, sent.await(2, TimeUnit.SECONDS));
            Fixture excluded = fixture(delivery); excluded.provider.onEvent(user(EventType.CLIENT_LOGIN)); assertNotNull(excluded.after.get()); excluded.after.get().begin(); excluded.after.get().commit(); Thread.sleep(20);
            assertEquals(7, payloads.size());
        } finally { delivery.close(); }
    }

    @Test public void rollbackSendsNothingAndAdminCallbackExcludesPrivateFields() throws Exception {
        List<String> payloads = new ArrayList<>();
        CountDownLatch sent = new CountDownLatch(1);
        Delivery delivery = delivery((endpoint, realm, timestamp, signature, body, timeout) -> { payloads.add(new String(body, StandardCharsets.UTF_8)); sent.countDown(); return new Delivery.Response(204, -1); });
        try {
            Fixture rolledBack = fixture(delivery); rolledBack.provider.onEvent(user(EventType.LOGIN)); rolledBack.after.get().begin(); rolledBack.after.get().rollback(); Thread.sleep(20); assertEquals(0, payloads.size());
            Fixture committed = fixture(delivery); committed.provider.onEvent(admin(), false); committed.after.get().begin(); committed.after.get().commit(); assertEquals(true, sent.await(2, TimeUnit.SECONDS));
            String json = payloads.get(0);
            for (String privateValue : List.of("email@example.test", "203.0.113.11", "representation-secret", "resource-secret", "provider-error", "details-secret")) assertFalse(json, json.contains(privateValue));
            assertEquals(true, json.contains("admin_actor_user_id"));
        } finally { delivery.close(); }
    }

    @Test public void dropsPermanentClientFailureWithoutRetry() throws Exception {
        AtomicReference<Integer> calls = new AtomicReference<>(0);
        Delivery delivery = delivery((endpoint, realm, timestamp, signature, body, timeout) -> { calls.set(calls.get() + 1); return new Delivery.Response(400, -1); });
        try { delivery.enqueue(new Delivery.Item("realm", "{}".getBytes(StandardCharsets.UTF_8))); for (int i = 0; i < 100 && delivery.dropCount() == 0; i++) Thread.sleep(10); assertEquals(1, calls.get().intValue()); assertEquals(1, delivery.dropCount()); } finally { delivery.close(); }
    }

    @Test public void captureBoundsCountAndByteOverflowBeforeCommit() {
        Delivery delivery = delivery((endpoint, realm, timestamp, signature, body, timeout) -> new Delivery.Response(204, -1));
        try {
            AfterCompletion count = new AfterCompletion(delivery);
            for (int i = 0; i < 129; i++) count.add(new Delivery.Item("realm", "{}".getBytes(StandardCharsets.UTF_8)));
            assertEquals(1, delivery.dropCount());
            count.begin(); count.rollback();
            AfterCompletion bytes = new AfterCompletion(delivery);
            bytes.add(new Delivery.Item("realm", new byte[600])); bytes.add(new Delivery.Item("realm", new byte[600]));
            assertEquals(2, delivery.dropCount());
        } finally { delivery.close(); }
    }

    private static Delivery delivery(Delivery.Transport transport) {
        return delivery(transport, 1024);
    }
    private static Delivery delivery(Delivery.Transport transport, int bytes) {
        return new Delivery(URI.create("http://127.0.0.1/audit"), "keycloak", Map.of("realm", SECRET), 256, bytes, 3, Duration.ofSeconds(10), transport, System::nanoTime, System::currentTimeMillis, Thread::sleep);
    }
    private static Fixture fixture(Delivery delivery) {
        AtomicReference<KeycloakTransaction> after = new AtomicReference<>();
        KeycloakTransactionManager tx = (KeycloakTransactionManager) Proxy.newProxyInstance(ProviderCallbackTest.class.getClassLoader(), new Class<?>[]{KeycloakTransactionManager.class}, (p, method, args) -> { if (method.getName().equals("enlistAfterCompletion")) after.set((KeycloakTransaction) args[0]); return defaultValue(method.getReturnType()); });
        RealmModel realm = (RealmModel) Proxy.newProxyInstance(ProviderCallbackTest.class.getClassLoader(), new Class<?>[]{RealmModel.class}, (p, method, args) -> method.getName().equals("getName") ? "realm" : defaultValue(method.getReturnType()));
        RealmProvider realms = (RealmProvider) Proxy.newProxyInstance(ProviderCallbackTest.class.getClassLoader(), new Class<?>[]{RealmProvider.class}, (p, method, args) -> method.getName().equals("getRealm") ? realm : defaultValue(method.getReturnType()));
        KeycloakSession session = (KeycloakSession) Proxy.newProxyInstance(ProviderCallbackTest.class.getClassLoader(), new Class<?>[]{KeycloakSession.class}, (p, method, args) -> method.getName().equals("getTransactionManager") ? tx : method.getName().equals("realms") ? realms : defaultValue(method.getReturnType()));
        return new Fixture(new AuditEventListenerProvider(session, delivery), after);
    }
    private static Object defaultValue(Class<?> type) { if (!type.isPrimitive()) return null; if (type == boolean.class) return false; if (type == byte.class) return (byte) 0; if (type == short.class) return (short) 0; if (type == int.class) return 0; if (type == long.class) return 0L; if (type == float.class) return 0f; if (type == double.class) return 0d; if (type == char.class) return '\0'; return null; }
    private static Event user(EventType type) { Event e = new Event(); e.setId("event-id"); e.setTime(1); e.setType(type); e.setRealmId("realm-id"); e.setRealmName("realm"); e.setUserId("subject"); e.setClientId("client"); return e; }
    private static AdminEvent admin() { AdminEvent e = new AdminEvent(); e.setId("event-id"); e.setTime(1); e.setRealmId("realm-id"); e.setRealmName("realm"); e.setOperationType(OperationType.UPDATE); e.setResourceTypeAsString("USER"); e.setRepresentation("representation-secret"); e.setResourcePath("resource-secret"); e.setError("provider-error"); e.setDetails(Map.of("detail", "details-secret")); AuthDetails a = new AuthDetails(); a.setRealmId("actor-realm"); a.setUserId("actor-user"); a.setClientId("actor-client"); a.setIpAddress("203.0.113.11"); e.setAuthDetails(a); return e; }
    private record Fixture(AuditEventListenerProvider provider, AtomicReference<KeycloakTransaction> after) { }
}
