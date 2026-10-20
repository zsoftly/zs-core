// Package billing is the billing surface of ZSoftly Core on Kill Bill.
//
// Kill Bill is the financial authority. It holds every balance, credit,
// invoice, payment and payment method, and this package stores none of them.
// It reads them from Kill Bill and writes them to Kill Bill, scoped to one
// brand, the Kill Bill tenant, and one Core tenant, the Kill Bill account
// whose external key is the tenant ID. Core keeps only what Kill Bill lacks:
// promo and voucher codes and their redemptions, the expiry of the credits
// Core issued, a cache of each tenant's billing standing, the receipts of
// Kill Bill push notifications, and audit.
//
// Every Kill Bill call carries the brand's own tenant API key and secret,
// resolved from the brand's secret reference at each call, and every write
// carries X-Killbill-CreatedBy (the Core principal), X-Killbill-Reason and
// X-Killbill-Comment (the correlation ID). No Kill Bill error text leaves
// the package: a Kill Bill 5xx or transport failure is billing_unavailable,
// a Kill Bill 4xx is billing_rejected with Kill Bill's numeric error code
// only. Card data never passes through Core, and payment method references
// and plugin properties are never logged.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// DB is the subset of pgxpool.Pool the service needs.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Service is the billing surface. DB, KillBill and Secrets are required.
// WebhookSecret is ZS_KILLBILL_WEBHOOK_SECRET: it verifies Kill Bill push
// notifications and, through a derived key, signs billing credentials.
// Without it those two features answer not_ready.
type Service struct {
	DB            DB
	KillBill      *killbill.Client
	Secrets       secrets.Resolver
	WebhookSecret []byte
	Log           *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Ready reports whether the service can call Kill Bill.
func (s *Service) Ready() bool {
	return s != nil && s.DB != nil && s.KillBill != nil && s.Secrets != nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// errNotReady is the answer when the service lacks its Kill Bill settings.
var errNotReady = zserrors.New(zserrors.CodeNotReady, "Billing needs the Kill Bill admin settings.")

// Call is who acts and why. Actor becomes X-Killbill-CreatedBy, Reason
// X-Killbill-Reason and CorrelationID X-Killbill-Comment, so Kill Bill's
// audit log matches Core's.
type Call struct {
	Actor         events.Actor
	Reason        string
	CorrelationID string
}

func (c Call) kb() killbill.CallContext {
	by := string(c.Actor.Type)
	if c.Actor.ID != "" {
		by += ":" + c.Actor.ID
	}
	if by == "" || by == string(events.ActorSystem) {
		by = "zs-core"
	}
	return killbill.CallContext{CreatedBy: by, Reason: c.Reason, Comment: c.CorrelationID}
}

func (c Call) validate() error {
	if !c.Actor.Type.Valid() || (c.Actor.Type.RequiresID() && c.Actor.ID == "") {
		return errors.New("billing: an actor is required")
	}
	if c.CorrelationID == "" {
		return errors.New("billing: a correlation ID is required")
	}
	return nil
}

// account is what a billing call needs for one tenant: the tenant, its
// brand and the Kill Bill tenant credentials of the brand.
type account struct {
	tenant tenant.Tenant
	brand  brand.Record
	creds  killbill.TenantCredentials
}

// id returns the Kill Bill account ID of the tenant.
func (a account) id() string { return a.tenant.KillBillAccountID }

// currency is the account currency: the brand's default currency, the one
// Core created the account with.
func (a account) currency() string { return a.brand.Manifest.Billing.Currencies[0] }

// pluginName is the Kill Bill payment plugin of the brand's payment methods.
func (a account) pluginName() string { return a.brand.Manifest.Billing.PaymentPluginName() }

// account loads the tenant of sc, its brand and the brand's Kill Bill
// credentials. A closed tenant is tenant_not_found: once closed, a tenant
// has no billing surface.
func (s *Service) account(ctx context.Context, sc scope.Scope) (account, error) {
	if !s.Ready() {
		return account{}, errNotReady
	}
	t, err := tenant.Get(ctx, s.DB, sc)
	if err != nil {
		return account{}, err
	}
	if t.Status == tenant.StatusClosed {
		return account{}, tenant.ErrNotFound
	}
	rec, creds, err := s.brandCreds(ctx, sc.Brand)
	if err != nil {
		return account{}, err
	}
	return account{tenant: t, brand: rec, creds: creds}, nil
}

// brandCreds loads an applied brand and resolves its Kill Bill tenant
// secret.
func (s *Service) brandCreds(ctx context.Context, id idgen.UUID) (brand.Record, killbill.TenantCredentials, error) {
	if !s.Ready() {
		return brand.Record{}, killbill.TenantCredentials{}, errNotReady
	}
	rec, err := brand.Get(ctx, s.DB, id)
	if err != nil {
		return brand.Record{}, killbill.TenantCredentials{}, err
	}
	if rec.Status != brand.StatusActive || rec.AppliedAt.IsZero() || rec.Manifest == nil || len(rec.Manifest.Billing.Currencies) == 0 {
		return brand.Record{}, killbill.TenantCredentials{}, zserrors.New(zserrors.CodeNotReady, "The brand is not applied.")
	}
	secret, err := s.Secrets.Resolve(ctx, rec.Slug, rec.KillBillAPISecretRef)
	if err != nil {
		return brand.Record{}, killbill.TenantCredentials{}, zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	}
	return rec, killbill.TenantCredentials{APIKey: rec.KillBillAPIKey, APISecret: secret}, nil
}

// kbError maps a Kill Bill failure to a typed error. Kill Bill's message
// stays in the cause, which is logged and never returned. A 4xx carries Kill
// Bill's numeric error code only.
func kbError(err error) error {
	var zsErr *zserrors.Error
	var kbErr *killbill.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &zsErr):
		return err
	case killbill.IsUnavailable(err), errors.Is(err, killbill.ErrCredentialsRejected):
		return zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	case errors.Is(err, killbill.ErrNotFound):
		return zserrors.Wrap(zserrors.CodeNotFound, "", err)
	case errors.As(err, &kbErr):
		if kbErr.Code != 0 {
			return zserrors.Wrap(zserrors.CodeBillingRejected, fmt.Sprintf("Kill Bill rejected the request with error code %d.", kbErr.Code), err)
		}
		return zserrors.Wrap(zserrors.CodeBillingRejected, fmt.Sprintf("Kill Bill rejected the request with HTTP %d.", kbErr.Status), err)
	case errors.Is(err, killbill.ErrConflict):
		return zserrors.Wrap(zserrors.CodeBillingRejected, "Kill Bill rejected the request because it conflicts with the current state.", err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	}
	return zserrors.Wrap(zserrors.CodeInternal, "", err)
}

// unavailable maps a database failure.
func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}

func validation(message string) error {
	return zserrors.New(zserrors.CodeValidation, message)
}

// errNotFound is the answer for a Kill Bill object that does not exist or
// belongs to another account. The two are never told apart.
var errNotFound = zserrors.New(zserrors.CodeNotFound, "")

// owned refuses an object of another Kill Bill account.
func owned(a account, accountID string) error {
	if !strings.EqualFold(accountID, a.id()) {
		return errNotFound
	}
	return nil
}

// checkKillBillID refuses a path value that is not a canonical UUID before
// it reaches a Kill Bill URL.
func checkKillBillID(id string) error {
	if _, err := idgen.ParseUUID(id); err != nil || len(id) != 36 {
		return errNotFound
	}
	return nil
}

// audit records a write Core made in Kill Bill, in its own transaction once
// Kill Bill has answered. details hold Kill Bill IDs, plan names and amounts,
// never plugin properties, payment method references or card data.
func (s *Service) audit(ctx context.Context, sc scope.Scope, call Call, action string, details map[string]any) error {
	ctx = context.WithoutCancel(ctx)
	return inTx(ctx, s.DB, func(tx pgx.Tx) error {
		_, err := audit.Write(ctx, tx, sc, audit.Entry{
			Actor: call.Actor, Action: action, Reason: strings.TrimSpace(call.Reason), Outcome: audit.OutcomeSuccess,
			CorrelationID: call.CorrelationID, Details: details,
		})
		return err
	})
}

// inTx runs fn in a transaction and commits it.
func inTx(ctx context.Context, db DB, fn func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

var (
	producerOnce sync.Once
	producer     events.Producer
)

func coreProducer() events.Producer {
	producerOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil || host == "" || len(host) > 160 {
			host = "zs-core"
		}
		producer = events.Producer{Service: "zs-core", Instance: host}
	})
	return producer
}
