// Package outbox is the transactional outbox for ZSoftly Core events.
//
// Write inserts an event in the caller's transaction, so the event commits or
// rolls back with the state change that produced it. A Relay later reads
// unpublished events, hands each to a Handler and marks it published only
// after the handler succeeds. A failed delivery is retried with exponential
// backoff and quarantined after a bounded number of attempts. Delivery is at
// least once, so handlers must be idempotent; the evt_ ID is the
// deduplication key.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/events"
)

const insertSQL = `
INSERT INTO outbox (id, event_type, brand_id, tenant_id, occurred_at, envelope)
VALUES ($1, $2, $3, $4, $5, $6)`

// Write validates envelope and inserts it in tx.
func Write(ctx context.Context, tx pgx.Tx, envelope events.Envelope) error {
	if tx == nil {
		return fmt.Errorf("outbox write requires a transaction")
	}
	if err := envelope.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode outbox envelope: %w", err)
	}
	var tenant any
	if t := envelope.Tenant(); !t.IsNil() {
		tenant = t
	}
	if _, err := tx.Exec(ctx, insertSQL,
		envelope.StorageID(), envelope.Type, envelope.Brand(), tenant, envelope.OccurredAt, encoded,
	); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}
