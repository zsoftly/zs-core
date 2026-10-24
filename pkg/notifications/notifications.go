package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/outbox"
)

const (
	DefaultBatchSize        = 100
	DefaultMaxAttempts      = 10
	MaxConcurrentDeliveries = 8
	MaxEnvelopeBytes        = 1 << 20
	claimLease              = 30 * time.Second
)

type Service struct {
	DB      *pgxpool.Pool
	Secrets secrets.Resolver
	HTTP    *http.Client
	Log     *slog.Logger
	Now     func() time.Time
}

var (
	defaultClientOnce sync.Once
	defaultClient     *http.Client
)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	defaultClientOnce.Do(func() { defaultClient = HTTPClient(nil) })
	return defaultClient
}

// ValidateEndpoints checks every stored webhook configuration before the
// generic outbox relay starts claiming source events. It never resolves a
// secret or changes database state. Errors deliberately name only the brand
// slug so malformed endpoint URLs and resolver detail do not escape.
func (s *Service) ValidateEndpoints(ctx context.Context) error {
	if s.DB == nil {
		return errors.New("webhook endpoint validation unavailable")
	}
	rows, err := s.DB.Query(ctx, `SELECT id, slug, manifest FROM brands ORDER BY slug`)
	if err != nil {
		return errors.New("webhook endpoint validation unavailable")
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id       idgen.UUID
			slug     string
			manifest []byte
		)
		if err := rows.Scan(&id, &slug, &manifest); err != nil {
			return errors.New("webhook endpoint validation unavailable")
		}
		var configured brand.Manifest
		if err := json.Unmarshal(manifest, &configured); err != nil {
			return endpointValidationError(slug)
		}
		if configured.Notifications == nil {
			continue
		}
		canonicalURLs := make(map[string]struct{}, len(configured.Notifications.Webhooks))
		for _, endpoint := range configured.Notifications.Webhooks {
			canonical, err := CanonicalURL(endpoint.URL)
			if err != nil {
				return endpointValidationError(slug)
			}
			if _, exists := canonicalURLs[canonical]; exists {
				return endpointValidationError(slug)
			}
			canonicalURLs[canonical] = struct{}{}
		}
		for _, endpoint := range configured.Notifications.Webhooks {
			if s.Secrets == nil || s.Secrets.Check(slug, endpoint.SecretRef) != nil {
				return endpointValidationError(slug)
			}
		}
	}
	if rows.Err() != nil {
		return errors.New("webhook endpoint validation unavailable")
	}
	return nil
}

func endpointValidationError(slug string) error {
	return fmt.Errorf("webhook endpoint configuration invalid for brand %s", slug)
}

// FanoutHandler creates durable endpoint snapshots in a transaction. The
// surrounding outbox relay acknowledges the source event only after this
// method returns successfully. Internal events remain excluded by the normal
// unfiltered outbox reader.
func (s *Service) FanoutHandler() outbox.Handler {
	return outbox.HandlerFunc(func(ctx context.Context, event events.Envelope) error {
		if s.DB == nil {
			return outbox.ErrDeliveryDeferred
		}
		return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error { return s.Fanout(ctx, tx, event) })
	})
}

// Fanout snapshots every matching current endpoint when it first writes the
// durable receipt. A queued source without a receipt uses the endpoint set
// current at that first fanout. A zero-endpoint receipt is also final, so
// later endpoint additions and filter widening never replay history.
func (s *Service) Fanout(ctx context.Context, tx pgx.Tx, event events.Envelope) error {
	if tx == nil {
		return errors.New("webhook fanout requires a transaction")
	}
	if err := event.Validate(); err != nil {
		return err
	}
	// Product webhooks are the generic outbox consumer. Internal events have
	// named consumers and must never escape through a direct Fanout call.
	// Record a zero-endpoint receipt so a retry cannot become deliverable if
	// the manifest changes later.
	if outbox.IsInternal(event.Type) {
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_fanouts (brand_id,event_id,endpoint_count) VALUES ($1,$2,0) ON CONFLICT DO NOTHING`, event.Brand(), event.StorageID()); err != nil {
			return fmt.Errorf("record internal webhook fanout receipt: %w", err)
		}
		return nil
	}
	// Lock the same row that brand apply updates before it records the new
	// manifest and reconciles cancellations. This serializes fanout snapshots
	// with endpoint removal so neither transaction can miss the other.
	if err := tx.QueryRow(ctx, `SELECT id FROM brands WHERE id=$1 FOR SHARE`, event.Brand()).Scan(new(idgen.UUID)); err != nil {
		return fmt.Errorf("lock webhook brand: %w", err)
	}
	record, err := brand.Get(ctx, tx, event.Brand())
	if err != nil {
		return fmt.Errorf("load webhook brand: %w", err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode webhook envelope: %w", err)
	}
	if len(payload) > MaxEnvelopeBytes {
		return fmt.Errorf("webhook envelope exceeds %d bytes", MaxEnvelopeBytes)
	}
	var webhooks []brand.Webhook
	if record.Manifest != nil && record.Manifest.Notifications != nil {
		webhooks = record.Manifest.Notifications.Webhooks
	}
	matching := make([]brand.Webhook, 0, len(webhooks))
	for _, endpoint := range webhooks {
		if matches(endpoint, event.Type) {
			matching = append(matching, endpoint)
		}
	}
	// The receipt and all endpoint rows commit together. A retry after a crash
	// before the source outbox acknowledgement sees the receipt and never adds
	// an endpoint introduced after the original fanout, including when the
	// original fanout had no matching endpoints.
	tag, err := tx.Exec(ctx, `INSERT INTO webhook_fanouts (brand_id,event_id,endpoint_count) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, event.Brand(), event.StorageID(), len(matching))
	if err != nil {
		return fmt.Errorf("insert webhook fanout receipt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if len(matching) != 0 && s.Secrets == nil {
		return errors.New("webhook fanout requires a secret resolver")
	}
	for _, endpoint := range matching {
		canonical, err := CanonicalURL(endpoint.URL)
		if err != nil {
			return fmt.Errorf("configured webhook URL: %w", err)
		}
		if err := s.Secrets.Check(record.Slug, endpoint.SecretRef); err != nil {
			return fmt.Errorf("configured webhook secret reference: %w", err)
		}
		deliveryID, err := idgen.NewV7()
		if err != nil {
			return fmt.Errorf("create webhook delivery ID: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO webhook_deliveries (id, brand_id, event_id, event_type, endpoint_url, secret_ref, envelope, next_attempt_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,now()) ON CONFLICT (brand_id,event_id,endpoint_url) DO NOTHING`,
			deliveryID, event.Brand(), event.StorageID(), event.Type, canonical, endpoint.SecretRef, payload)
		if err != nil {
			return fmt.Errorf("insert webhook delivery: %w", err)
		}
	}
	return nil
}

func matches(endpoint brand.Webhook, eventType string) bool {
	if len(endpoint.Events) == 0 {
		return true
	}
	for _, typ := range endpoint.Events {
		if typ == eventType {
			return true
		}
	}
	return false
}

// Reconcile cancels pending deliveries that the current manifest no longer
// permits. It is deliberately cancellation-only: a newly added endpoint does
// not revive rows made while it was absent. Call it in the brand-apply
// transaction after the manifest has become current.
func (s *Service) Reconcile(ctx context.Context, tx pgx.Tx, record brand.Record, entry audit.Entry) (int, error) {
	if tx == nil {
		return 0, errors.New("webhook reconciliation requires a transaction")
	}
	endpoints := make([]struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}, 0)
	if record.Manifest != nil && record.Manifest.Notifications != nil {
		for _, endpoint := range record.Manifest.Notifications.Webhooks {
			canonical, err := CanonicalURL(endpoint.URL)
			if err != nil {
				// Match currentEndpoint's behavior: an invalid stored endpoint does
				// not match a pending delivery and is therefore cancelled.
				continue
			}
			events := endpoint.Events
			if events == nil {
				events = []string{}
			}
			endpoints = append(endpoints, struct {
				URL    string   `json:"url"`
				Events []string `json:"events"`
			}{URL: canonical, Events: events})
		}
	}
	allowed, err := json.Marshal(endpoints)
	if err != nil {
		return 0, fmt.Errorf("encode current webhook endpoints: %w", err)
	}
	tag, err := tx.Exec(ctx, `
UPDATE webhook_deliveries AS delivery
SET canceled_at=now(), last_error='endpoint_removed_or_filtered', claim_token=NULL, claimed_until=NULL
WHERE delivery.brand_id=$1
  AND delivery.delivered_at IS NULL
  AND delivery.canceled_at IS NULL
  AND delivery.quarantined_at IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM jsonb_to_recordset($2::jsonb) AS endpoint(url text, events jsonb)
      WHERE endpoint.url=delivery.endpoint_url
        AND (jsonb_array_length(endpoint.events)=0 OR endpoint.events ? delivery.event_type)
  )`, record.ID, string(allowed))
	if err != nil {
		return 0, fmt.Errorf("cancel webhook deliveries: %w", err)
	}
	canceled := int(tag.RowsAffected())
	if canceled != 0 {
		entry.Action = "notification.webhook_deliveries.cancelled"
		entry.Details = map[string]any{"count": canceled}
		if _, err := audit.WriteBrand(ctx, tx, record.ID, entry); err != nil {
			return 0, fmt.Errorf("audit webhook reconciliation: %w", err)
		}
	}
	return canceled, nil
}

// Run drains due delivery rows until ctx ends. Each row is independently
// claimed and completed, so a failing endpoint never blocks another endpoint.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := s.RunOnce(ctx, DefaultBatchSize); err != nil && s.Log != nil {
			s.Log.Error("webhook delivery pass failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) RunOnce(ctx context.Context, limit int) (int, error) {
	if s.DB == nil {
		return 0, errors.New("webhook delivery requires a database")
	}
	if limit <= 0 {
		limit = DefaultBatchSize
	}
	if limit > MaxConcurrentDeliveries {
		limit = MaxConcurrentDeliveries
	}
	token, err := idgen.NewV7()
	if err != nil {
		return 0, fmt.Errorf("create webhook claim token: %w", err)
	}
	rows, err := s.DB.Query(ctx, `WITH due AS (SELECT id FROM webhook_deliveries WHERE delivered_at IS NULL AND canceled_at IS NULL AND quarantined_at IS NULL AND next_attempt_at <= now() AND (claimed_until IS NULL OR claimed_until < now()) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT $1) UPDATE webhook_deliveries d SET claim_token=$2, claimed_until=now()+$3 * interval '1 second' FROM due WHERE d.id=due.id RETURNING d.id,d.brand_id,d.event_id,d.event_type,d.endpoint_url,d.envelope,d.claim_token,d.claimed_until`, limit, token, int64(claimLease/time.Second))
	if err != nil {
		return 0, fmt.Errorf("claim webhook deliveries: %w", err)
	}
	var deliveries []delivery
	for rows.Next() {
		var d delivery
		if err := rows.Scan(&d.ID, &d.BrandID, &d.EventID, &d.EventType, &d.EndpointURL, &d.Envelope, &d.Token, &d.ClaimedUntil); err != nil {
			rows.Close()
			return 0, err
		}
		deliveries = append(deliveries, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	var wg sync.WaitGroup
	for _, delivery := range deliveries {
		d := delivery
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.deliver(ctx, d); err != nil && s.Log != nil {
				// Transport and resolver errors can include the complete endpoint URL.
				// URLs are configuration, not safe log fields: their paths may carry a
				// vendor capability token. Keep the operational signal bounded.
				s.Log.Warn("webhook delivery failed", "delivery", d.ID.String(), "error_class", "delivery_failed")
			}
		}()
	}
	wg.Wait()
	// Count is the number of rows whose delivery was started in this pass.
	// Per-endpoint failures are durably retried or quarantined and therefore
	// do not abort healthy endpoints in the same claimed group.
	return len(deliveries), nil
}

type delivery struct {
	ID, BrandID, EventID   idgen.UUID
	EventType, EndpointURL string
	Envelope               []byte
	Token                  idgen.UUID
	ClaimedUntil           time.Time
}

func (s *Service) deliver(ctx context.Context, d delivery) error {
	if !s.now().Before(d.ClaimedUntil) {
		return errors.New("webhook delivery claim expired before send")
	}
	deadlineCtx, cancel := context.WithDeadline(ctx, d.ClaimedUntil)
	defer cancel()
	ctx = deadlineCtx
	record, err := brand.Get(ctx, s.DB, d.BrandID)
	if err != nil {
		return s.failed(ctx, d, "brand_unavailable", err)
	}
	endpoint, ok := currentEndpoint(record.Manifest, d.EndpointURL, d.EventType)
	if !ok {
		return s.cancel(ctx, d, "endpoint_removed_or_filtered")
	}
	if s.Secrets == nil {
		return s.failed(ctx, d, "secret_unavailable", errors.New("webhook secrets are not configured"))
	}
	secret, err := s.Secrets.Resolve(ctx, record.Slug, endpoint.SecretRef)
	if err != nil {
		return s.failed(ctx, d, "secret_unavailable", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.EndpointURL, bytes.NewReader(d.Envelope))
	if err != nil {
		return s.failed(ctx, d, "request_invalid", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, Sign([]byte(secret), d.Envelope, s.now()))
	// This is the final eligibility boundary. Reconciliation that completes
	// before it prevents a request, including if secret resolution blocked;
	// once Do begins, an in-flight request cannot be recalled.
	if err := s.eligible(ctx, d); err != nil {
		return err
	}
	resp, err := s.client().Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, DefaultMaxResponseBytes))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return s.complete(ctx, d)
		}
		err = fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return s.failed(ctx, d, "delivery_failure", err)
}

func (s *Service) eligible(ctx context.Context, d delivery) error {
	var eligible bool
	if err := s.DB.QueryRow(ctx, `SELECT true FROM webhook_deliveries WHERE id=$1 AND claim_token=$2 AND claimed_until > now() AND delivered_at IS NULL AND canceled_at IS NULL AND quarantined_at IS NULL`, d.ID, d.Token).Scan(&eligible); err != nil || !eligible {
		return errors.New("webhook delivery claim lost")
	}
	return nil
}

func currentEndpoint(manifest *brand.Manifest, canonical, eventType string) (brand.Webhook, bool) {
	if manifest == nil || manifest.Notifications == nil {
		return brand.Webhook{}, false
	}
	for _, w := range manifest.Notifications.Webhooks {
		c, err := CanonicalURL(w.URL)
		if err == nil && c == canonical && matches(w, eventType) {
			return w, true
		}
	}
	return brand.Webhook{}, false
}

func (s *Service) complete(ctx context.Context, d delivery) error {
	tag, err := s.DB.Exec(ctx, `UPDATE webhook_deliveries SET delivered_at=now(), claim_token=NULL, claimed_until=NULL, last_error=NULL WHERE id=$1 AND claim_token=$2 AND claimed_until > now() AND delivered_at IS NULL`, d.ID, d.Token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("webhook delivery claim lost")
	}
	return nil
}
func (s *Service) cancel(ctx context.Context, d delivery, reason string) error {
	_, err := s.DB.Exec(ctx, `UPDATE webhook_deliveries SET canceled_at=now(), last_error=$3, claim_token=NULL, claimed_until=NULL WHERE id=$1 AND claim_token=$2 AND claimed_until > now() AND delivered_at IS NULL`, d.ID, d.Token, reason)
	return err
}
func (s *Service) failed(ctx context.Context, d delivery, reason string, original error) error {
	tag, err := s.DB.Exec(ctx, `UPDATE webhook_deliveries SET attempts=attempts+1,last_error=$3,next_attempt_at=now()+(LEAST(3600,POWER(2,attempts)::int)*interval '1 second'),quarantined_at=CASE WHEN attempts+1 >= $4 THEN now() ELSE NULL END,claim_token=NULL,claimed_until=NULL WHERE id=$1 AND claim_token=$2 AND claimed_until > now() AND delivered_at IS NULL`, d.ID, d.Token, reason, DefaultMaxAttempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("webhook delivery claim lost")
	}
	return original
}
