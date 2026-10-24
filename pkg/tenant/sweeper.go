package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// SystemActor is the audit actor of the sweeper.
var SystemActor = events.Actor{Type: events.ActorSystem}

// Sweeper completes tenant creations that an engine failure interrupted and
// corrects Keycloak Organizations towards Core: their name, their enabled
// state and their members. It also repairs account primary email towards
// current eligible ownership. One pass runs at a time across every Core
// instance, through a database advisory lock.
type Sweeper struct {
	DB      DB
	Engines Engines
	Log     *slog.Logger
	// Batch is how many creations and tenants one query reads. Zero means
	// 100.
	Batch int
	// Closure supplies the steps of a closure the sweep cannot do itself: the
	// cancellation and drain before finance, and the account block after it.
	// Nil leaves a closing tenant waiting at those steps.
	Closure ClosureSteps
	// ClosureWork is how many closing tenants one pass drives. Zero means 50.
	ClosureWork int
	// ClosureScan is how many closing tenants one pass looks at. Zero means 500.
	ClosureScan int
	// PassBudget is how long one closure pass may run. Zero means 30 seconds.
	PassBudget time.Duration
	// CallBudget is how long one closure step call may run. Zero means 5
	// seconds.
	CallBudget time.Duration
}

// SweepReport counts what one pass did.
type SweepReport struct {
	Skipped              bool
	Completed            int
	Failed               int
	OrganizationsChecked int
	OrganizationsUpdated int
	MembersAdded         int
	MembersRemoved       int
	RecipientsChecked    int
	RecipientsUpdated    int
	RecipientsDeferred   int
	Errors               int
	// ClosuresChecked counts the closing tenants the pass looked at, and the
	// rest count what became of them.
	ClosuresChecked      int
	ClosuresAdvanced     int
	ClosuresWaiting      int
	ClosuresManualReview int
	ClosuresBusy         int
	ClosuresFailed       int
}

func (s *Sweeper) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// Run sweeps every interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := s.SweepOnce(ctx)
		if err != nil {
			s.log().Error("tenant sweep failed", "error", err.Error())
		} else if !report.Skipped {
			s.log().Info("tenant sweep", "completed", report.Completed, "failed", report.Failed,
				"organizations_checked", report.OrganizationsChecked, "organizations_updated", report.OrganizationsUpdated,
				"members_added", report.MembersAdded, "members_removed", report.MembersRemoved,
				"recipients_checked", report.RecipientsChecked, "recipients_updated", report.RecipientsUpdated,
				"recipients_deferred", report.RecipientsDeferred, "errors", report.Errors)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SweepOnce runs one pass. It returns Skipped when another pass holds the
// lock.
func (s *Sweeper) SweepOnce(ctx context.Context) (report SweepReport, err error) {
	if !s.Engines.Ready() {
		return report, fmt.Errorf("tenant sweep: the engines are not configured")
	}
	pool, ok := s.DB.(*pgxpool.Pool)
	if !ok {
		return report, fmt.Errorf("tenant sweep: lifecycle pool is required")
	}
	guard, locked, err := AcquireLifecycleSweep(ctx, pool)
	if err != nil {
		return report, err
	}
	if !locked {
		report.Skipped = true
		return report, nil
	}
	budgetEnded := false
	defer func() {
		if releaseErr := guard.Release(); releaseErr != nil {
			// A pass whose budget ended in the middle of a statement has no
			// capability left to release. That is how it ended, not a fault.
			if budgetEnded && errors.Is(releaseErr, ErrLifecycleReleased) {
				return
			}
			err = errors.Join(err, releaseErr)
		}
	}()
	correlationID := idgen.MustNewV7().String()
	if err := s.completeCreations(ctx, guard, correlationID, &report); err != nil {
		return report, err
	}
	brands, err := brand.List(ctx, guard, s.log())
	if err != nil {
		return report, err
	}
	for _, b := range brands {
		if b.Status != brand.StatusActive || b.AppliedAt.IsZero() {
			continue
		}
		if err := s.syncBrand(ctx, guard, b.ID, correlationID, &report); err != nil {
			return report, err
		}
	}
	// The closures come last. Their pass can end by its budget in the middle
	// of a statement, which ends the whole sweep pass, and a long closure
	// backlog must not keep the Organizations and recipients from being synced.
	if err := s.completeClosures(ctx, guard, correlationID, &report); err != nil {
		if errors.Is(err, errClosurePassOver) {
			// The closure budget ended in the middle of a statement, so the
			// capability is spent. The pass ends here and the next one goes on.
			budgetEnded = true
			return report, nil
		}
		return report, err
	}
	return report, nil
}

func (s *Sweeper) batch() int {
	if s.Batch <= 0 {
		return 100
	}
	return s.Batch
}

// completeCreations claims due creations and runs each again.
func (s *Sweeper) completeCreations(ctx context.Context, guard *LifecycleSweep, correlationID string, report *SweepReport) error {
	passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	passCutoff := time.Now().UTC()
	eligibleAt, tenantID, err := s.creationCursor(passCtx, guard)
	if err != nil {
		return err
	}
	work, scanned := 0, 0
	for scanned < 500 && work < 50 {
		limit := s.batch()
		if remaining := 500 - scanned; limit > remaining {
			limit = remaining
		}
		due, err := s.creationPage(passCtx, guard, passCutoff, eligibleAt, tenantID, limit)
		if err != nil {
			return err
		}
		if len(due) == 0 {
			return s.advanceCreationCursor(passCtx, guard, provisioning{}, true)
		}
		for _, p := range due {
			if scanned >= 500 || work >= 50 {
				break
			}
			scanned++
			child, got, err := guard.TryTenant(passCtx, p.scope())
			if err != nil {
				return err
			}
			if !got {
				if err := s.advanceCreationCursor(passCtx, guard, p, false); err != nil {
					return err
				}
				at := p.EligibleAt
				eligibleAt, tenantID = &at, p.TenantID
				continue
			}
			claim, err := newClaim()
			if err == nil {
				args, bindErr := p.scope().Bind(claim, fmt.Sprintf("%d seconds", int(ProvisioningLease.Seconds())))
				if bindErr != nil {
					return errors.Join(bindErr, child.Release())
				}
				tx, beginErr := child.Begin(passCtx)
				if beginErr != nil {
					return errors.Join(beginErr, child.Release())
				}
				var current bool
				err = tx.QueryRow(passCtx, `SELECT true FROM tenant_provisioning
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'pending' AND (claimed_until IS NULL OR claimed_until <= now())
FOR UPDATE SKIP LOCKED`, args[:2]...).Scan(&current)
				if err == nil {
					var claimed bool
					err = tx.QueryRow(passCtx, `UPDATE tenant_provisioning SET claim_token = $3, claimed_until = now() + $4::interval, updated_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'pending' AND (claimed_until IS NULL OR claimed_until <= now()) RETURNING true`, args...).Scan(&claimed)
				}
				if err == nil {
					err = tx.Commit(passCtx)
				} else {
					_ = tx.Rollback(context.WithoutCancel(ctx))
				}
			}
			if err == nil {
				work++
				p.Claim = claim
				_, err = provision(passCtx, child, s.Engines, p, SystemActor, correlationID)
			}
			// A creation that failed and then failed to clean up reports both.
			err = errors.Join(err, child.Release(), s.advanceCreationCursor(passCtx, guard, p, false))
			at := p.EligibleAt
			eligibleAt, tenantID = &at, p.TenantID
			if err != nil {
				report.Failed++
				s.log().Warn("tenant creation is still incomplete", "tenant_id", p.publicID(), "error", err.Error())
				continue
			}
			report.Completed++
		}
	}
	return nil
}

func (s *Sweeper) creationCursor(ctx context.Context, guard *LifecycleSweep) (*time.Time, idgen.UUID, error) {
	var at *time.Time
	var id *idgen.UUID
	err := guard.QueryRow(ctx, `SELECT last_eligible_at, last_tenant_id FROM sweep_cursor WHERE kind = 'creation'`).Scan(&at, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, idgen.Nil, nil
	}
	if err != nil {
		return nil, idgen.Nil, err
	}
	if id == nil {
		return nil, idgen.Nil, nil
	}
	return at, *id, nil
}

func (s *Sweeper) creationPage(ctx context.Context, guard *LifecycleSweep, cutoff time.Time, afterAt *time.Time, afterID idgen.UUID, limit int) ([]provisioning, error) {
	tx, err := guard.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+provisioningColumns+` FROM tenant_provisioning
WHERE status = 'pending' AND next_attempt_at <= $1 AND (claimed_until IS NULL OR claimed_until <= $1)
  AND ($2::timestamptz IS NULL OR (next_attempt_at, tenant_id) > ($2, $3))
ORDER BY next_attempt_at, tenant_id LIMIT $4`, cutoff, afterAt, afterID, limit)
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, unavailable(err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (provisioning, error) { return scanProvisioning(r) })
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable(err)
	}
	return list, nil
}

func (s *Sweeper) advanceCreationCursor(ctx context.Context, guard *LifecycleSweep, p provisioning, wrap bool) error {
	tx, err := guard.Begin(ctx)
	if err != nil {
		return err
	}
	if wrap {
		_, err = tx.Exec(ctx, `INSERT INTO sweep_cursor (kind, last_eligible_at, last_tenant_id, updated_at)
VALUES ('creation', NULL, NULL, now()) ON CONFLICT (kind) DO UPDATE SET last_eligible_at = NULL, last_tenant_id = NULL, updated_at = now()`)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO sweep_cursor (kind, last_eligible_at, last_tenant_id, updated_at)
VALUES ('creation', $1, $2, now()) ON CONFLICT (kind) DO UPDATE SET last_eligible_at = EXCLUDED.last_eligible_at, last_tenant_id = EXCLUDED.last_tenant_id, updated_at = now()`, p.EligibleAt, p.TenantID)
	}
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}

func (s *Sweeper) syncBrand(ctx context.Context, guard *LifecycleSweep, brandID idgen.UUID, correlationID string, report *SweepReport) error {
	after := idgen.Nil
	for {
		rows, err := guard.Query(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE brand_id = $1 AND id > $2 ORDER BY id LIMIT $3`,
			brandID, after, s.batch())
		if err != nil {
			return unavailable(err)
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) { return scanTenant(r) })
		if err != nil {
			return unavailable(err)
		}
		for _, t := range list {
			child, got, err := guard.TryTenant(ctx, t.Scope())
			if err != nil {
				return err
			}
			if !got {
				continue
			}
			// The page was read before this capability was taken, so the
			// status on it can be out of date. The Organization is synced
			// with the status the tenant has now.
			t, err = currentTenant(ctx, child, t)
			if err != nil {
				return errors.Join(err, child.Release())
			}
			r, err := SyncOrganization(ctx, child, s.Engines, t, correlationID)
			report.OrganizationsChecked++
			report.MembersAdded += r.MembersAdded
			report.MembersRemoved += r.MembersRemoved
			if r.OrganizationUpdated {
				report.OrganizationsUpdated++
			}
			if err != nil {
				report.Errors++
				s.log().Warn("keycloak organization not in step with Core", "tenant_id", t.PublicID(), "error", err.Error())
			}
			// Recipient repair is independent of the Organization outcome. Ready
			// rows are checked too, including legacy accounts and email drift.
			recipientCtx, cancel := context.WithTimeout(ctx, mirrorTimeout)
			recipient, recipientErr := ReconcileBillingRecipient(recipientCtx, child, s.Engines, t.Scope(), correlationID)
			cancel()
			if recipient.Deferred {
				report.RecipientsDeferred++
			} else {
				report.RecipientsChecked++
			}
			if recipient.Changed {
				report.RecipientsUpdated++
			}
			if recipientErr != nil {
				report.Errors++
				s.log().Warn("billing recipient not in step with Core", "tenant_id", t.PublicID(), "error_code", codeName(recipientErr))
			}
			if releaseErr := child.Release(); releaseErr != nil {
				return errors.Join(err, recipientErr, releaseErr)
			}
		}
		if len(list) < s.batch() {
			return nil
		}
		after = list[len(list)-1].ID
	}
}

// currentTenant reads t again under the capability db holds.
func currentTenant(ctx context.Context, db DB, t Tenant) (Tenant, error) {
	args, err := t.Scope().Bind()
	if err != nil {
		return t, err
	}
	fresh, err := scanTenant(db.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE brand_id = $1 AND id = $2`, args...))
	if err != nil {
		return t, unavailable(err)
	}
	return fresh, nil
}

// SyncReport is what SyncOrganization changed.
type SyncReport struct {
	OrganizationUpdated bool
	MembersAdded        int
	MembersRemoved      int
}

// SyncOrganization corrects the Keycloak Organization of t towards Core. The
// organization is enabled only while the tenant is active and carries the
// tenant's name. Its members are the identities with an active membership,
// none once the tenant is closed. Every correction is audited as a system
// action.
func SyncOrganization(ctx context.Context, db DB, eng Engines, t Tenant, correlationID string) (SyncReport, error) {
	var report SyncReport
	be, err := eng.forBrand(ctx, db, t.BrandID)
	if err != nil {
		return report, err
	}
	kc := eng.Keycloak
	org, err := kc.GetOrganization(ctx, be.realm, t.KeycloakOrganizationID)
	if err != nil {
		return report, engineError(err)
	}
	wantEnabled := t.Status == StatusActive
	fallback := fallbackOrganizationName(t.Name, t.PublicID())
	if org.Enabled != wantEnabled || (org.Name != t.Name && org.Name != fallback) {
		before := map[string]any{"name": org.Name, "enabled": org.Enabled}
		update := org
		update.Enabled = wantEnabled
		if org.Name != fallback {
			update.Name = t.Name
		}
		err := kc.UpdateOrganization(ctx, be.realm, update)
		if errors.Is(err, keycloak.ErrConflict) {
			update.Name = fallback
			err = kc.UpdateOrganization(ctx, be.realm, update)
		}
		if err != nil {
			return report, engineError(err)
		}
		report.OrganizationUpdated = true
		if err := systemAudit(ctx, db, t.Scope(), "tenant.keycloak.organization.updated", correlationID, map[string]any{
			"before": before, "after": map[string]any{"name": update.Name, "enabled": update.Enabled},
		}); err != nil {
			return report, err
		}
	}

	want := map[string]string{}
	if t.Status != StatusClosed {
		args, err := t.Scope().Bind()
		if err != nil {
			return report, err
		}
		rows, err := db.Query(ctx, `SELECT m.id, coalesce(i.keycloak_user_id, '') FROM memberships m
JOIN identities i ON i.id = m.identity_id AND i.brand_id = m.brand_id
WHERE m.brand_id = $1 AND m.tenant_id = $2 AND m.status = 'active'`, args...)
		if err != nil {
			return report, unavailable(err)
		}
		for rows.Next() {
			var id idgen.UUID
			var user string
			if err := rows.Scan(&id, &user); err != nil {
				rows.Close()
				return report, unavailable(err)
			}
			if user != "" {
				want[user] = idgen.Format(idgen.Membership, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return report, unavailable(err)
		}
	}
	have, err := kc.OrganizationMembers(ctx, be.realm, org.ID)
	if err != nil {
		return report, engineError(err)
	}
	present := map[string]bool{}
	var firstErr error
	for _, m := range have {
		present[m.ID] = true
		if _, ok := want[m.ID]; ok {
			continue
		}
		if err := kc.RemoveOrganizationMember(ctx, be.realm, org.ID, m.ID); err != nil && !errors.Is(err, keycloak.ErrNotFound) {
			firstErr = errors.Join(firstErr, engineError(err))
			continue
		}
		report.MembersRemoved++
		if err := systemAudit(ctx, db, t.Scope(), "tenant.keycloak.member.removed", correlationID,
			map[string]any{"keycloak_user_id": m.ID}); err != nil {
			return report, err
		}
	}
	for user, membership := range want {
		if present[user] {
			continue
		}
		if err := kc.AddOrganizationMember(ctx, be.realm, org.ID, user); err != nil && !errors.Is(err, keycloak.ErrConflict) {
			firstErr = errors.Join(firstErr, engineError(err))
			continue
		}
		report.MembersAdded++
		if err := systemAudit(ctx, db, t.Scope(), "tenant.keycloak.member.added", correlationID,
			map[string]any{"keycloak_user_id": user, "membership_id": membership}); err != nil {
			return report, err
		}
	}
	return report, firstErr
}

func systemAudit(ctx context.Context, db DB, s scope.Scope, action, correlationID string, details map[string]any) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := audit.Write(ctx, tx, s, audit.Entry{
		Actor: SystemActor, Action: action, Outcome: audit.OutcomeSuccess, CorrelationID: correlationID, Details: details,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}
