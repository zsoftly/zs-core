# Notifications

Outbound webhooks deliver public Core outbox events to the endpoints under a brand manifest's `notifications.webhooks` list. An empty endpoint `events` filter matches every public event. `tenant.mirror`, `wallet.settlement.requested`, and `metering.usage.recorded` remain internal events with exclusive consumers.

For a queued, unprocessed event, Core uses the endpoints and filters in the current manifest when it first creates a receipt. Core writes a durable delivery row for each matching endpoint before it acknowledges the source event. Core acknowledges an event with no matching endpoint. After a receipt, including a zero-endpoint receipt, adding endpoints or widening filters does not replay the event.

Delivery is at least once. A receiver must deduplicate the stable `evt_` envelope ID in durable storage before applying its side effect. Core retries failed endpoints independently with exponential backoff. Core quarantines a delivery after ten failed attempts.

Each worker pass claims and starts at most eight deliveries. Its count is the deliveries started. Endpoint failures remain for retry and do not fail healthy peers in the same pass. Removing an endpoint, or removing an event from its filter, cancels pending rows. Re-adding it does not resurrect them.

Core signs the exact JSON envelope bytes with `X-ZS-Signature: t=<unix>,v1=<hex HMAC-SHA256(t.body)>`. `Sign` emits a canonical decimal Unix timestamp. `Verify` parses the received timestamp to check its age. It computes the HMAC over the exact received timestamp text. A signature for one spelling does not authenticate another spelling of the same instant. Verify the HMAC in constant time. Reject timestamps more than five minutes before or after the receiver clock. Timestamp validation limits replay age but does not replace durable event-ID deduplication. [examples/webhook-client](../../examples/webhook-client) shows the verifier. The example binds a local listener. Terminate TLS in a trusted proxy before forwarding requests to it.

Core accepts only absolute HTTPS URLs without credentials, query strings, or fragments. Core resolves every destination immediately before dialing. It rejects private, loopback, link-local, multicast, metadata, unspecified and every IANA special-purpose IPv4 and IPv6 range, including globally reachable protocol assignments. Core disables proxies and redirects.

Core normalizes IPv4-mapped addresses to IPv4 before it checks this policy. The policy follows the [IANA IPv4 special-purpose registry](https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xml) and [IANA IPv6 special-purpose registry](https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xml). The registry snapshots have retrieval timestamp 2026-10-22T10:42:14Z. The IPv4 snapshot SHA-256 is `37f334466c842387a8aee8d1ae7a6d5b2208ef27eb8964072538b73ee2f5b21d`. The IPv6 snapshot SHA-256 is `660acf0201b462b8d820605ba1cb20c4ed216acd883ee4a55a17453d73d4c3aa`.

Core stores endpoint URLs and secret references. It resolves secret values from the current brand manifest just before a request, so a secret rotation takes effect without rewriting pending rows. A cancellation made before the worker's final claim check prevents a request. It cannot recall a request already in flight.

Manifest validation and `zs-core brand apply` reject invalid endpoint URLs before storing the manifest. At startup, Core repeats validation of every stored endpoint as defence in depth before starting the relays. An invalid endpoint stops startup. The fixed diagnostic identifies only the affected brand. Repair the stored configuration with `zs-core brand apply`.
