package ca.zsoftly.core.keycloakaudit;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotEquals;
import static org.junit.Assert.assertThrows;

import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import org.junit.Test;

public class DeliveryTest {
    private static final byte[] SECRET = "01234567890123456789012345678901".getBytes(StandardCharsets.UTF_8);
    private static final URI ENDPOINT = URI.create("http://127.0.0.1/audit");

    @Test public void signsExactTimestampNewlineAndBodyBytes() {
        byte[] body = "{\"event_id\":\"e1\"}".getBytes(StandardCharsets.UTF_8);
        String signature = Delivery.signature(SECRET, "1760000000", body);
        assertEquals("18e3a09b4c64c64c810cb1f0ba20bb13bf936c3415c3f6c9994d1b0ab555b76a", signature);
        assertNotEquals(signature, Delivery.signature("01234567890123456789012345678901x".getBytes(StandardCharsets.UTF_8), "1760000000", body));
        assertNotEquals(signature, Delivery.signature(SECRET, "1760000000", "{\"event_id\":\"e2\"}".getBytes(StandardCharsets.UTF_8)));
        assertNotEquals(signature, Delivery.signature(SECRET, "1760000001", body));
    }

    @Test public void rejectsDuplicateOrShortRealmSecrets() {
        assertThrows(IllegalArgumentException.class, () -> Delivery.parseRealmSecrets("realm=short"));
        assertThrows(IllegalArgumentException.class, () -> Delivery.parseRealmSecrets("realm=01234567890123456789012345678901;realm=01234567890123456789012345678901"));
        assertEquals(32, Delivery.parseRealmSecrets("realm=01234567890123456789012345678901").get("realm").length);
    }

    @Test public void retries429AndServerFailureButNotOtherClientFailure() throws Exception {
        AtomicInteger calls = new AtomicInteger();
        CountDownLatch done = new CountDownLatch(1);
        Delivery d = delivery((endpoint, realm, timestamp, signature, body, timeout) -> { int call = calls.incrementAndGet(); if (call == 1) return new Delivery.Response(429, 1); if (call == 2) return new Delivery.Response(503, -1); done.countDown(); return new Delivery.Response(204, -1); }, new AtomicLong(0));
        try { d.enqueue(item(0)); assertEquals("retry sequence", true, done.await(2, TimeUnit.SECONDS)); assertEquals(3, calls.get()); } finally { d.close(); }
    }

    @Test public void retriesImmediatelyForZeroRetryAfterWhileAgeRemains() throws Exception {
        AtomicInteger calls = new AtomicInteger();
        CountDownLatch done = new CountDownLatch(1);
        Delivery d = delivery((endpoint, realm, timestamp, signature, body, timeout) -> { if (calls.incrementAndGet() == 1) return new Delivery.Response(429, 0); done.countDown(); return new Delivery.Response(204, -1); }, new AtomicLong(0));
        try { d.enqueue(item(0)); assertEquals(true, done.await(2, TimeUnit.SECONDS)); assertEquals(2, calls.get()); assertEquals(0, d.dropCount()); } finally { d.close(); }
    }

    @Test public void dropsExpiredItemBeforeTransport() throws Exception {
        AtomicInteger calls = new AtomicInteger();
        AtomicLong clock = new AtomicLong(Duration.ofSeconds(2).toNanos());
        Delivery d = delivery((endpoint, realm, timestamp, signature, body, timeout) -> { calls.incrementAndGet(); return new Delivery.Response(204, -1); }, clock, Duration.ofSeconds(1));
        try { d.enqueue(item(0)); awaitDrops(d, 1); assertEquals(0, calls.get()); assertEquals(1, d.dropCount()); } finally { d.close(); }
    }

    @Test public void boundsQueueAndRejectsLateEnqueue() throws Exception {
        CountDownLatch entered = new CountDownLatch(1);
        CountDownLatch release = new CountDownLatch(1);
        Delivery d = delivery((endpoint, realm, timestamp, signature, body, timeout) -> { entered.countDown(); release.await(); return new Delivery.Response(204, -1); }, new AtomicLong(0));
        d.enqueue(item(0)); assertEquals(true, entered.await(2, TimeUnit.SECONDS));
        d.enqueue(item(0));
        d.close();
        d.enqueue(item(0));
        release.countDown();
        assertEquals(3, d.dropCount());
    }

    private static Delivery delivery(Delivery.Transport transport, AtomicLong clock) {
        return delivery(transport, clock, Duration.ofSeconds(10));
    }
    private static Delivery delivery(Delivery.Transport transport, AtomicLong clock, Duration deadline) {
        return new Delivery(ENDPOINT, "keycloak", Map.of("realm", SECRET), 1, 1024, 3, deadline, transport, clock::get, () -> 1760000000000L, millis -> clock.addAndGet(Duration.ofMillis(millis).toNanos()));
    }
    private static Delivery.Item item(long accepted) { return new Delivery.Item("realm", "{}".getBytes(StandardCharsets.UTF_8), accepted); }
    private static void awaitDrops(Delivery d, long expected) throws Exception { for (int i = 0; i < 100 && d.dropCount() < expected; i++) Thread.sleep(10); assertEquals(expected, d.dropCount()); }
}
