package ca.zsoftly.core.keycloakaudit;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.LongSupplier;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import org.jboss.logging.Logger;

/** Bounded best-effort delivery. A process crash can lose memory-resident items. */
final class Delivery implements AutoCloseable {
    private static final Logger LOG = Logger.getLogger(Delivery.class);
    private static final Duration REQUEST_TIMEOUT = Duration.ofSeconds(5);
    private final URI endpoint;
    private final String sourceId;
    private final Map<String, byte[]> secrets;
    private final ArrayBlockingQueue<Item> queue;
    private final ExecutorService worker;
    private final int maxAttempts;
    private final long deadlineNanos;
    private final long maxBytes;
    private final Transport transport;
    private final LongSupplier nanoClock;
    private final LongSupplier epochMillis;
    private final Sleeper sleeper;
    private final AtomicLong queuedBytes = new AtomicLong();
    private final AtomicLong drops = new AtomicLong();
    private final AtomicBoolean closed = new AtomicBoolean();
    private final AtomicReference<Item> active = new AtomicReference<>();

    Delivery(URI endpoint, String sourceId, Map<String, byte[]> secrets, int capacity, int bytes, int maxAttempts, Duration deadline) {
        this(endpoint, sourceId, secrets, capacity, bytes, maxAttempts, deadline, new HttpTransport(), System::nanoTime, System::currentTimeMillis, Thread::sleep);
    }

    Delivery(URI endpoint, String sourceId, Map<String, byte[]> secrets, int capacity, int bytes, int maxAttempts, Duration deadline, Transport transport, LongSupplier nanoClock, LongSupplier epochMillis, Sleeper sleeper) {
        if (!"https".equals(endpoint.getScheme()) && !"http".equals(endpoint.getScheme())) throw new IllegalArgumentException("zs-audit endpoint must be http or https");
        if (capacity < 1 || bytes < 1024 || maxAttempts < 1 || deadline.isNegative() || deadline.isZero()) throw new IllegalArgumentException("invalid zs-audit limits");
        this.endpoint = endpoint;
        this.sourceId = sourceId;
        this.secrets = secrets;
        this.queue = new ArrayBlockingQueue<>(capacity);
        this.maxBytes = bytes;
        this.maxAttempts = maxAttempts;
        this.deadlineNanos = deadline.toNanos();
        this.transport = transport;
        this.nanoClock = nanoClock;
        this.epochMillis = epochMillis;
        this.sleeper = sleeper;
        this.worker = Executors.newSingleThreadExecutor(r -> { Thread t = new Thread(r, "zs-audit-delivery"); t.setDaemon(true); return t; });
        this.worker.execute(this::run);
    }

    String sourceId() { return sourceId; }
    int maxItemBytes() { return (int) Math.min(maxBytes, 16 * 1024); }
    long dropCount() { return drops.get(); }

    void enqueue(Item item) {
        if (closed.get()) { drop("late_enqueue"); return; }
        if (item == null || !secrets.containsKey(item.realm()) || item.body().length > maxItemBytes()) { drop("queue"); return; }
        synchronized (queue) {
            if (closed.get()) { drop("late_enqueue"); return; }
            if (queuedBytes.get() + item.body().length > maxBytes || !queue.offer(item)) { drop("queue"); return; }
            queuedBytes.addAndGet(item.body().length);
        }
    }

    private void run() {
        while (!Thread.currentThread().isInterrupted()) {
            try {
                Item item = queue.take();
                queuedBytes.addAndGet(-item.body().length);
                active.set(item);
                deliver(item);
                active.compareAndSet(item, null);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            } catch (RuntimeException e) {
                active.set(null);
                drop("worker");
            }
        }
    }

    private void deliver(Item item) {
        for (int attempt = 1; attempt <= maxAttempts; attempt++) {
            if (closed.get()) { drop("shutdown_active"); return; }
            long remaining = remainingNanos(item);
            if (remaining <= 0) { drop("deadline"); return; }
            try {
                String timestamp = Long.toString(epochMillis.getAsLong() / 1000);
                Response response = transport.send(endpoint, item.realm(), timestamp, signature(secrets.get(item.realm()), timestamp, item.body()), item.body(), min(REQUEST_TIMEOUT, remaining));
                if (response.status() == 204) return;
                if (response.status() >= 400 && response.status() < 500 && response.status() != 429) { drop("rejected"); return; }
                if (!sleep(item, retryDelay(attempt, response.retryAfterMillis()))) return;
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                if (closed.get()) drop("shutdown_active"); else drop("interrupted");
                return;
            } catch (Exception ignored) {
                if (!sleep(item, retryDelay(attempt, -1))) return;
            }
        }
        drop("retry_exhausted");
    }

    private boolean sleep(Item item, long requestedMillis) {
        if (closed.get()) { drop("shutdown_active"); return false; }
        long remaining = remainingNanos(item);
        if (remaining <= 0) { drop("deadline"); return false; }
        if (requestedMillis == 0) return true;
        long millis = Math.min(requestedMillis, TimeUnit.NANOSECONDS.toMillis(remaining));
        if (millis <= 0) { drop("deadline"); return false; }
        try {
            sleeper.sleep(millis);
            return true;
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            if (closed.get()) drop("shutdown_active"); else drop("interrupted");
            return false;
        }
    }

    private long remainingNanos(Item item) { return deadlineNanos - Math.max(0, nanoClock.getAsLong() - item.acceptedAtNanos()); }
    private static Duration min(Duration requested, long remainingNanos) { return requested.compareTo(Duration.ofNanos(remainingNanos)) < 0 ? requested : Duration.ofNanos(remainingNanos); }
    private static long retryDelay(int attempt, long retryAfterMillis) { long backoff = Math.min(1000L * attempt, 5000L); return retryAfterMillis >= 0 ? Math.min(retryAfterMillis, 5000L) : backoff; }

    static String signature(byte[] secret, String timestamp, byte[] body) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(secret, "HmacSHA256"));
            mac.update(timestamp.getBytes(StandardCharsets.US_ASCII));
            mac.update((byte) '\n');
            return java.util.HexFormat.of().formatHex(mac.doFinal(body));
        } catch (Exception e) { throw new IllegalStateException(e); }
    }

    static Map<String, byte[]> parseRealmSecrets(String value) {
        Map<String, byte[]> out = new HashMap<>();
        for (String pair : value.split(";")) {
            String[] p = pair.split("=", 2);
            if (p.length != 2 || p[0].isBlank() || p[1].length() < 32 || out.put(p[0], p[1].getBytes(StandardCharsets.UTF_8)) != null) throw new IllegalArgumentException("invalid zs-audit realm-secrets");
        }
        return Map.copyOf(out);
    }

    void drop(String reason) {
        long count = drops.incrementAndGet();
        LOG.warnf("zs-audit event dropped reason=%s count=%d", reason, count);
    }

    @Override public void close() {
        if (!closed.compareAndSet(false, true)) return;
        synchronized (queue) {
            Item item;
            while ((item = queue.poll()) != null) { queuedBytes.addAndGet(-item.body().length); drop("shutdown_queue"); }
        }
        worker.shutdownNow();
        try {
            if (!worker.awaitTermination(5, TimeUnit.SECONDS)) LOG.warn("zs-audit delivery worker did not terminate before timeout");
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            LOG.warn("zs-audit delivery worker shutdown interrupted");
        }
    }

    interface Sleeper { void sleep(long millis) throws InterruptedException; }
    interface Transport { Response send(URI endpoint, String realm, String timestamp, String signature, byte[] body, Duration timeout) throws Exception; }
    record Response(int status, long retryAfterMillis) { }
    record Item(String realm, byte[] body, long acceptedAtNanos) {
        Item(String realm, byte[] body) { this(realm, body, System.nanoTime()); }
        Item { body = body.clone(); }
        @Override public byte[] body() { return body.clone(); }
    }

    private static final class HttpTransport implements Transport {
        private final HttpClient client = HttpClient.newBuilder().build();
        @Override public Response send(URI endpoint, String realm, String timestamp, String signature, byte[] body, Duration timeout) throws Exception {
            HttpRequest request = HttpRequest.newBuilder(endpoint).timeout(timeout).header("Content-Type", "application/json").header("X-ZS-Keycloak-Realm", realm).header("X-ZS-Keycloak-Timestamp", timestamp).header("X-ZS-Keycloak-Signature", signature).POST(HttpRequest.BodyPublishers.ofByteArray(body)).build();
            HttpResponse<Void> response = client.send(request, HttpResponse.BodyHandlers.discarding());
            return new Response(response.statusCode(), retryAfter(response));
        }
        private static long retryAfter(HttpResponse<?> response) {
            String value = response.headers().firstValue("Retry-After").orElse("");
            try { long seconds = Long.parseLong(value); return seconds < 0 || seconds > 5 ? -1 : seconds * 1000; } catch (NumberFormatException e) { return -1; }
        }
    }
}
