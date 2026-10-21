package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

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
func (s *Sweeper) SweepOnce(ctx context.Context) (SweepReport, error) {
	var report SweepReport
	if !s.Engines.Ready() {
		return report, fmt.Errorf("tenant sweep: the engines are not configured")
	}
	lock, err := s.DB.Begin(ctx)
	if err != nil {
		return report, unavailable(err)
	}
	defer func() { _ = lock.Rollback(context.WithoutCancel(ctx)) }()
	var locked bool
	if err := lock.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('zs-core.tenant.sweep:' || current_schema(), 0))`).Scan(&locked); err != nil {
		return report, unavailable(err)
	}
	if !locked {
		report.Skipped = true
		return report, nil
	}
	correlationID := idgen.MustNewV7().String()
	if err := s.completeCreations(ctx, correlationID, &report); err != nil {
		return report, err
	}
	brands, err := brand.List(ctx, s.DB, s.log())
	if err != nil {
		return report, err
	}
	for _, b := range brands {
		if b.Status != brand.StatusActive || b.AppliedAt.IsZero() {
			continue
		}
		if err := s.syncBrand(ctx, b.ID, correlationID, &report); err != nil {
			return report, err
		}
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
func (s *Sweeper) completeCreations(ctx context.Context, correlationID string, report *SweepReport) error {
	claim, err := newClaim()
	if err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `UPDATE tenant_provisioning SET claim_token = $1, claimed_until = now() + $2::interval, updated_at = now()
WHERE tenant_id IN (
    SELECT tenant_id FROM tenant_provisioning
    WHERE status = 'pending' AND next_attempt_at <= now() AND (claimed_until IS NULL OR claimed_until <= now())
    ORDER BY next_attempt_at LIMIT $3 FOR UPDATE SKIP LOCKED)
RETURNING `+provisioningColumns, claim, fmt.Sprintf("%d seconds", int(ProvisioningLease.Seconds())), s.batch())
	if err != nil {
		return unavailable(err)
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (provisioning, error) { return scanProvisioning(r) })
	if err != nil {
		return unavailable(err)
	}
	for _, p := range due {
		if _, err := provision(ctx, s.DB, s.Engines, p, SystemActor, correlationID); err != nil {
			report.Failed++
			s.log().Warn("tenant creation is still incomplete", "tenant_id", p.publicID(), "error", err.Error())
			continue
		}
		report.Completed++
	}
	return nil
}

func (s *Sweeper) syncBrand(ctx context.Context, brandID idgen.UUID, correlationID string, report *SweepReport) error {
	after := idgen.Nil
	for {
		rows, err := s.DB.Query(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE brand_id = $1 AND id > $2 ORDER BY id LIMIT $3`,
			brandID, after, s.batch())
		if err != nil {
			return unavailable(err)
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) { return scanTenant(r) })
		if err != nil {
			return unavailable(err)
		}
		for _, t := range list {
			r, err := SyncOrganization(ctx, s.DB, s.Engines, t, correlationID)
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
			recipient, recipientErr := ReconcileBillingRecipient(recipientCtx, s.DB, s.Engines, t.Scope(), correlationID)
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
		}
		if len(list) < s.batch() {
			return nil
		}
		after = list[len(list)-1].ID
	}
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
