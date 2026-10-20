package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// MaxNotificationBytes caps a push notification body.
const MaxNotificationBytes = 64 << 10

// Core events made from Kill Bill push notifications.
const (
	EventInvoiceCreated      = "billing.invoice.created"
	EventPaymentSucceeded    = "billing.payment.succeeded"
	EventPaymentFailed       = "billing.payment.failed"
	EventOverdueChanged      = "billing.overdue.changed"
	EventSubscriptionChanged = "billing.subscription.changed"
)

// coreEvents maps Kill Bill event types to Core events. Other Kill Bill
// events refresh the standing cache and make no Core event. Kill Bill sends
// PAYMENT_SUCCESS or PAYMENT_FAILED for every payment transaction, and also
// INVOICE_PAYMENT_* when the payment pays an invoice, so only the PAYMENT_*
// events make Core events.
var coreEvents = map[string]string{
	"INVOICE_CREATION":        EventInvoiceCreated,
	"PAYMENT_SUCCESS":         EventPaymentSucceeded,
	"PAYMENT_FAILED":          EventPaymentFailed,
	"OVERDUE_CHANGE":          EventOverdueChanged,
	"SUBSCRIPTION_CREATION":   EventSubscriptionChanged,
	"SUBSCRIPTION_PHASE":      EventSubscriptionChanged,
	"SUBSCRIPTION_CHANGE":     EventSubscriptionChanged,
	"SUBSCRIPTION_CANCEL":     EventSubscriptionChanged,
	"SUBSCRIPTION_UNCANCEL":   EventSubscriptionChanged,
	"SUBSCRIPTION_BCD_CHANGE": EventSubscriptionChanged,
}

// Notification is a Kill Bill push notification as Kill Bill 0.24.22 sends
// it. It carries no event ID.
type Notification struct {
	EventType  string  `json:"eventType"`
	AccountID  *string `json:"accountId"`
	ObjectType string  `json:"objectType"`
	ObjectID   string  `json:"objectId"`
	MetaData   *string `json:"metaData"`
}

// NotificationResult says what HandleNotification did.
type NotificationResult struct {
	// Duplicate is true for a notification Core already handled.
	Duplicate bool
	// Ignored is true for a notification that names no account of the
	// brand, or no account at all.
	Ignored bool
	// Event is the Core event written, or "".
	Event string
}

// VerifyWebhookToken checks the token of a brand's callback path in
// constant time.
func (s *Service) VerifyWebhookToken(brandID idgen.UUID, token string) bool {
	if len(s.WebhookSecret) == 0 || brandID.IsNil() {
		return false
	}
	want := brand.WebhookToken(s.WebhookSecret, brandID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

// NotificationRetention is how long Core keeps the receipt of a push
// notification. The credit sweeper deletes older receipts.
const NotificationRetention = 7 * 24 * time.Hour

// HandleNotification turns a Kill Bill push notification for a brand into
// Core state, once. The caller has verified the brand's callback token. The
// notification must name an account of a tenant of that brand. Any other is
// ignored. Kill Bill sends no event ID and sends the same body again on a
// retry. So Core reads the state of the object from Kill Bill when the
// notification arrives, the subscription for a subscription event and the
// overdue state otherwise, and deduplicates on a digest of the body and that
// state. A notification whose state differs from the last state recorded for
// its object is never dropped, so a state that changes and changes back
// still makes an event. For a known account the standing cache is refreshed
// and the mapped Core event is written to the outbox, both in one
// transaction with the receipt.
func (s *Service) HandleNotification(ctx context.Context, brandID idgen.UUID, body []byte) (NotificationResult, error) {
	if !s.Ready() {
		return NotificationResult{}, errNotReady
	}
	if len(body) > MaxNotificationBytes {
		return NotificationResult{}, zserrors.New(zserrors.CodePayloadTooLarge, "")
	}
	var n Notification
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&n); err != nil || n.EventType == "" || len(n.EventType) > 100 || !upperSnake(n.EventType) ||
		len(n.ObjectType) > 100 || len(n.ObjectID) > 190 || (n.AccountID != nil && len(*n.AccountID) > 190) {
		return NotificationResult{}, validation("The body is not a Kill Bill push notification.")
	}

	var t *tenant.Tenant
	if n.AccountID != nil && *n.AccountID != "" {
		found, err := tenantByAccount(ctx, s.DB, brandID, *n.AccountID)
		if err != nil {
			return NotificationResult{}, err
		}
		t = found
	}
	result := NotificationResult{Ignored: t == nil}
	var od *Overdue
	observed := ""
	if t != nil && t.Status != tenant.StatusClosed {
		a, err := s.account(ctx, t.Scope())
		if err != nil {
			return NotificationResult{}, err
		}
		got, err := s.KillBill.AccountOverdue(ctx, a.creds, a.id())
		if err != nil {
			return NotificationResult{}, kbError(err)
		}
		od = &Overdue{State: got.Name, Clear: got.IsClearState, BlocksChanges: got.IsBlockChanges,
			DisablesEntitlement: got.IsDisableEntitlementAndChangesBlocked}
		observed = "overdue:" + got.Name
		if n.ObjectType == "SUBSCRIPTION" && checkKillBillID(n.ObjectID) == nil {
			if observed, err = s.subscriptionState(ctx, a, n.ObjectID); err != nil {
				return NotificationResult{}, err
			}
		}
	}
	if len(observed) > 300 {
		sum := sha256.Sum256([]byte(observed))
		observed = "sha256:" + hex.EncodeToString(sum[:])
	}
	key := notificationKey(body, observed)
	objectKey := n.ObjectType + ":" + n.ObjectID
	if n.ObjectID == "" && n.AccountID != nil {
		objectKey = n.ObjectType + ":" + *n.AccountID
	}
	err := inTx(ctx, s.DB, func(tx pgx.Tx) error {
		// One notification of an object at a time, so the last recorded
		// state is the one the next notification compares with.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "zs-core.killbill-notification:"+brandID.String()+":"+objectKey); err != nil {
			return unavailable(err)
		}
		var last *string
		if err := tx.QueryRow(ctx, `SELECT observed_state FROM killbill_notifications WHERE brand_id = $1 AND object_key = $2
ORDER BY seq DESC LIMIT 1`, brandID, objectKey).Scan(&last); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return unavailable(err)
		}
		var tenantID any
		if t != nil {
			tenantID = t.ID
		}
		// A receipt with the same key is a retry, unless the object's state
		// changed since: then the receipt becomes the latest again.
		tag, err := tx.Exec(ctx, `INSERT INTO killbill_notifications (brand_id, dedupe_key, event_type, tenant_id, object_key, observed_state, received_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (brand_id, dedupe_key) DO UPDATE SET received_at = EXCLUDED.received_at, seq = EXCLUDED.seq WHERE $8::boolean`,
			brandID, key, n.EventType, tenantID, objectKey, observed, s.now(), last != nil && *last != observed)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			result = NotificationResult{Duplicate: true}
			return nil
		}
		if t == nil {
			return nil
		}
		if od != nil {
			if err := s.saveStanding(ctx, tx, t.Scope(), *od, n.EventType); err != nil {
				return err
			}
		}
		eventType, ok := coreEvents[n.EventType]
		if !ok {
			return nil
		}
		payload := map[string]any{"killbill_event_type": n.EventType, "killbill_object_type": n.ObjectType,
			"killbill_object_id": n.ObjectID, "killbill_account_id": *n.AccountID}
		if eventType == EventOverdueChanged && od != nil {
			payload["overdue"] = od
		}
		if eventType == EventPaymentSucceeded || eventType == EventPaymentFailed {
			for k, v := range paymentMeta(n.MetaData) {
				payload[k] = v
			}
		}
		env, err := events.New(ctx, events.Params{
			Type: eventType, SchemaVersion: 1, Producer: coreProducer(),
			Actor: events.Actor{Type: events.ActorProvider, ID: "killbill"},
			Brand: brandID, Tenant: t.ID,
			Aggregate:     events.Aggregate{Type: "killbill." + strings.ToLower(n.ObjectType), ID: aggregateID(n)},
			CorrelationID: key[:32], Payload: payload,
		})
		if err != nil {
			return err
		}
		if err := outbox.Write(ctx, tx, env); err != nil {
			return unavailable(err)
		}
		result.Event = eventType
		return nil
	})
	if err != nil {
		return NotificationResult{}, err
	}
	return result, nil
}

// notificationKey is the digest of a notification body and the state Core
// observed in Kill Bill when it arrived.
func notificationKey(body []byte, observed string) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte{0})
	h.Write([]byte(observed))
	return hex.EncodeToString(h.Sum(nil))
}

// subscriptionState is the observed state of a subscription of the account:
// its state, plan and phase in Kill Bill. A subscription Kill Bill does not
// know, or one of another account, is "subscription:unknown".
func (s *Service) subscriptionState(ctx context.Context, a account, id string) (string, error) {
	sub, err := s.KillBill.Subscription(ctx, a.creds, id)
	switch {
	case errors.Is(err, killbill.ErrNotFound):
		return "subscription:unknown", nil
	case err != nil:
		return "", kbError(err)
	case owned(a, sub.AccountID) != nil:
		return "subscription:unknown", nil
	}
	return "subscription:" + sub.State + ":" + sub.PlanName + ":" + sub.PhaseType + ":" + sub.CancelledDate, nil
}

// paymentMeta reads the transaction type and ID that Kill Bill 0.24.22 puts
// in the metadata of a payment notification, so a purchase and a refund of
// one payment are told apart. Amounts stay in Kill Bill.
func paymentMeta(meta *string) map[string]string {
	out := map[string]string{}
	if meta == nil {
		return out
	}
	var m struct {
		PaymentTransactionID string `json:"paymentTransactionId"`
		TransactionType      string `json:"transactionType"`
		Status               string `json:"status"`
	}
	if json.Unmarshal([]byte(*meta), &m) != nil {
		return out
	}
	for k, v := range map[string]string{"killbill_transaction_id": m.PaymentTransactionID, "transaction_type": m.TransactionType, "transaction_status": m.Status} {
		if v != "" && len(v) <= 64 && upperSnake(strings.ReplaceAll(strings.ToUpper(v), "-", "_")) {
			out[k] = v
		}
	}
	return out
}

func aggregateID(n Notification) string {
	if n.ObjectID != "" && len(n.ObjectID) <= 255 {
		return n.ObjectID
	}
	return *n.AccountID
}

func upperSnake(s string) bool {
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// tenantByAccount returns the tenant of a brand whose Kill Bill account is
// accountID, or nil.
func tenantByAccount(ctx context.Context, q DB, brandID idgen.UUID, accountID string) (*tenant.Tenant, error) {
	if _, err := idgen.ParseUUID(accountID); err != nil {
		return nil, nil
	}
	var id idgen.UUID
	err := q.QueryRow(ctx, `SELECT id FROM tenants WHERE brand_id = $1 AND killbill_account_id = $2::uuid`, brandID, accountID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable(err)
	}
	t, err := tenant.Get(ctx, q, scope.Scope{Brand: brandID, Tenant: id})
	if err != nil {
		return nil, err
	}
	return &t, nil
}
