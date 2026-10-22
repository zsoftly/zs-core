package ca.zsoftly.core.keycloakaudit;

import java.util.ArrayList;
import java.util.List;
import org.keycloak.models.AbstractKeycloakTransaction;

/** Holds immutable payload bytes until the Keycloak transaction commits. */
final class AfterCompletion extends AbstractKeycloakTransaction {
    private final Delivery delivery;
    private final List<Delivery.Item> items = new ArrayList<>();
    private final int maxItems;
    private int bytes;

    AfterCompletion(Delivery delivery) { this.delivery = delivery; this.maxItems = 128; }
    void add(Delivery.Item item) {
        if (item == null || items.size() >= maxItems || bytes + item.body().length > delivery.maxItemBytes()) {
            delivery.drop("capture");
            return;
        }
        items.add(item);
        bytes += item.body().length;
    }
    @Override protected void commitImpl() { items.forEach(delivery::enqueue); items.clear(); bytes = 0; }
    @Override protected void rollbackImpl() { items.clear(); bytes = 0; }
}
