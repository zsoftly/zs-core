//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// settlementAt is a closing tenant whose closure settlement sits at status,
// with the columns the earlier states fill in.
func settlementAt(t *testing.T, e *env, status string, drained, blocked bool) tenant.Tenant {
	t.Helper()
	ctx := context.Background()
	tn := e.mustOrg(e.person("owner@example.test"), "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements
    (brand_id, tenant_id, cutoff_at, status, cancellation_completed_at, drain_completed_at, finance_target_date, blocked_at)
VALUES ($1, $2, now(), $3, CASE WHEN $3 <> 'cancelling' THEN now() END, CASE WHEN $4 THEN now() END, CASE WHEN $3 <> 'cancelling' OR $4 THEN current_date END,
        CASE WHEN $5 THEN now() END)`, tn.BrandID, tn.ID, status, drained, blocked); err != nil {
		t.Fatal(err)
	}
	return tn
}

// settleInvoice gives the tenant a closure invoice attempt with the outcomes
// named, in order: no_invoice; started, invoice_id and evidenced; or started
// and post_unknown. It returns the invoice ID when the outcomes carry one.
func settleInvoice(t *testing.T, e *env, tn tenant.Tenant, kinds ...string) idgen.UUID {
	t.Helper()
	ctx := context.Background()
	attempt, invoice := idgen.MustNewV7(), idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $2, $4::uuid, current_date)`, attempt, tn.BrandID, tn.ID, tn.KillBillAccountID); err != nil {
		t.Fatal(err)
	}
	for _, kind := range kinds {
		var inv, currency any
		switch kind {
		case "started":
			currency = "CAD"
		case "invoice_id", "evidenced":
			inv = invoice
		}
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, invoice_id, expected_currency) VALUES ($1, $2, $3, $4, $5)`,
			idgen.MustNewV7(), attempt, kind, inv, currency); err != nil {
			t.Fatalf("outcome %s: %v", kind, err)
		}
	}
	return invoice
}

func notReady(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !zserrors.IsCode(err, zserrors.CodeNotReady) {
		t.Fatalf("%s = %v, want a not-ready refusal", what, err)
	}
}

// A settlement moves one state at a time, each step audited, and a step that is
// already done is a no-op.
func TestClosureSettlementMovesOneStateAtATimeAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := settlementAt(t, e, "cancelling", false, false)
	adv := func(to tenant.ClosureState) (tenant.ClosureAdvance, error) {
		return tenant.AdvanceClosureSettlement(ctx, e.db.Pool, tn.Scope(), reviewActor, to, "corr-state")
	}
	_, err := adv(tenant.ClosureCompleted)
	notReady(t, "cancelling to completed", err)
	_, err = adv(tenant.ClosureBlocking)
	notReady(t, "cancelling to blocking", err)
	_, err = adv(tenant.ClosureFinancePending)
	notReady(t, "finance before the usage is drained", err)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET drain_completed_at = now(), finance_target_date = current_date WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	got, err := adv(tenant.ClosureFinancePending)
	if err != nil || !got.Changed || got.From != tenant.ClosureCancelling || got.To != tenant.ClosureFinancePending {
		t.Fatalf("advance = %+v %v", got, err)
	}
	if again, err := adv(tenant.ClosureFinancePending); err != nil || again.Changed {
		t.Fatalf("repeat = %+v %v, want no change", again, err)
	}
	var cancelled bool
	var audits int
	if err := e.db.Pool.QueryRow(ctx, `SELECT cancellation_completed_at IS NOT NULL FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&cancelled); err != nil || !cancelled {
		t.Fatalf("cancellation_completed_at = %v, %v", cancelled, err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_state_changed'`, tn.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows = %d, %v", audits, err)
	}
}

// Blocking needs the invoice settled: evidenced or nothing to invoice. An
// unknown outcome, an unfinished attempt and no attempt all wait for a person.
func TestClosureSettlementNeedsASettledInvoiceBeforeBlocking(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	toBlocking := func(tn tenant.Tenant) (tenant.ClosureAdvance, error) {
		return tenant.AdvanceClosureSettlement(ctx, e.db.Pool, tn.Scope(), reviewActor, tenant.ClosureBlocking, "corr-state")
	}
	none := settlementAt(t, e, "finance_pending", true, false)
	_, err := toBlocking(none)
	notReady(t, "no attempt", err)

	unknown := settlementAt(t, e, "finance_pending", true, false)
	settleInvoice(t, e, unknown, "started", "post_unknown")
	_, err = toBlocking(unknown)
	notReady(t, "an unknown outcome", err)

	unfinished := settlementAt(t, e, "finance_pending", true, false)
	settleInvoice(t, e, unfinished, "started", "invoice_id")
	_, err = toBlocking(unfinished)
	notReady(t, "an invoice not yet evidenced", err)

	evidenced := settlementAt(t, e, "finance_pending", true, false)
	invoice := settleInvoice(t, e, evidenced, "started", "invoice_id", "evidenced")
	if got, err := toBlocking(evidenced); err != nil || !got.Changed {
		t.Fatalf("evidenced = %+v %v", got, err)
	}
	var recorded *string
	if err := e.db.Pool.QueryRow(ctx, `SELECT finance_invoice_id::text FROM tenant_closure_settlements WHERE tenant_id = $1`, evidenced.ID).Scan(&recorded); err != nil || recorded == nil || *recorded != invoice.String() {
		t.Fatalf("finance_invoice_id = %v, %v, want %s", recorded, err, invoice)
	}

	nothing := settlementAt(t, e, "finance_pending", true, false)
	settleInvoice(t, e, nothing, "no_invoice")
	if got, err := toBlocking(nothing); err != nil || !got.Changed {
		t.Fatalf("nothing to invoice = %+v %v", got, err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT finance_invoice_id::text FROM tenant_closure_settlements WHERE tenant_id = $1`, nothing.ID).Scan(&recorded); err != nil || recorded != nil {
		t.Fatalf("finance_invoice_id with nothing to invoice = %v, %v", recorded, err)
	}
}

// Completion needs the account blocked, a settled invoice, and the review
// guard of migration 26: unresolved legacy usage stops it until staff review it.
func TestClosureSettlementCompletesOnlyAfterBlockingAndReview(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := settlementAt(t, e, "blocking", true, false)
	settleInvoice(t, e, tn, "no_invoice")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET finance_completed_at = now() WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	complete := func() (tenant.ClosureAdvance, error) {
		return tenant.AdvanceClosureSettlement(ctx, e.db.Pool, tn.Scope(), reviewActor, tenant.ClosureCompleted, "corr-state")
	}
	_, err := complete()
	notReady(t, "completion before the account is blocked", err)
	if changed, err := tenant.MarkClosureBlocked(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-state"); err != nil || !changed {
		t.Fatalf("mark blocked = %v %v", changed, err)
	}
	if changed, err := tenant.MarkClosureBlocked(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-state"); err != nil || changed {
		t.Fatalf("a second mark blocked = %v %v, want no change", changed, err)
	}
	addLegacyEvent(t, e, tn)
	_, err = complete()
	notReady(t, "completion with unreviewed legacy usage", err)
	if _, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-state"); err != nil {
		t.Fatal(err)
	}
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[0].ID, reviewActor, "reviewed with finance", "corr-state"); err != nil {
		t.Fatal(err)
	}
	got, err := complete()
	if err != nil || !got.Changed || got.From != tenant.ClosureBlocking || got.To != tenant.ClosureCompleted {
		t.Fatalf("completion = %+v %v", got, err)
	}
	if again, err := complete(); err != nil || again.Changed {
		t.Fatalf("repeat = %+v %v, want no change", again, err)
	}
	var completed bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&completed); err != nil || !completed {
		t.Fatalf("completed_at = %v, %v", completed, err)
	}
}

// The database itself refuses to skip a state or to go back.
func TestClosureSettlementCannotSkipOrGoBackInTheDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, from, to string }{
		{"finance to completed", "finance_pending", "completed"},
		{"cancelling to blocking", "cancelling", "blocking"},
		{"blocking to finance", "blocking", "finance_pending"},
		{"completed to blocking", "completed", "blocking"},
	} {
		tn := settlementAt(t, e, tc.from, true, true)
		settleInvoice(t, e, tn, "no_invoice")
		_, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET status = $2 WHERE tenant_id = $1`, tn.ID, tc.to)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s = %v, want a check violation", tc.name, err)
		}
	}
}

// A settlement starts at cancelling, and nothing a session can set changes
// that. The test databases turn the rule off for fixtures, so the test turns it
// on in its own transaction, which needs ownership of the table.
func TestClosureSettlementStartsAtCancellingWhateverIsSet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := e.mustOrg(e.person("owner@example.test"), "Tenant A")
	insert := func(status string, settings ...string) error {
		tx, err := e.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `ALTER TABLE tenant_closure_settlements ENABLE TRIGGER tenant_closure_settlement_start`); err != nil {
			t.Fatal(err)
		}
		for _, setting := range settings {
			if _, err := tx.Exec(ctx, setting); err != nil {
				t.Fatalf("%s: %v", setting, err)
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status) VALUES ($1, $2, now(), $3)`, tn.BrandID, tn.ID, status)
		return err
	}
	var pgErr *pgconn.PgError
	for name, settings := range map[string][]string{
		"plain":                   nil,
		"the old fixture setting": {`SET LOCAL zs.test_settlement_fixture = 'on'`},
		"another custom setting":  {`SET LOCAL zs.anything = 'on'`, `SET LOCAL zs.test_fixture = 'true'`},
	} {
		if err := insert("blocking", settings...); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: insert at blocking = %v, want a check violation", name, err)
		}
	}
	if err := insert("cancelling"); err != nil {
		t.Fatalf("insert at cancelling = %v", err)
	}
}

// A recorded step is never changed or cleared, and a completed settlement is
// frozen whole.
func TestClosureSettlementFreezesRecordedStepsAndCompletedRows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := settlementAt(t, e, "blocking", true, true)
	settleInvoice(t, e, tn, "no_invoice")
	violation := func(what, statement string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if _, err := e.db.Pool.Exec(ctx, statement, tn.ID); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s = %v, want a check violation", what, err)
		}
	}
	violation("clearing blocked_at", `UPDATE tenant_closure_settlements SET blocked_at = NULL WHERE tenant_id = $1`)
	violation("rewriting cancellation_completed_at", `UPDATE tenant_closure_settlements SET cancellation_completed_at = now() + interval '1 day' WHERE tenant_id = $1`)
	violation("rewriting drain_completed_at", `UPDATE tenant_closure_settlements SET drain_completed_at = NULL WHERE tenant_id = $1`)
	done := settlementAt(t, e, "completed", true, true)
	_, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET last_error = 'x' WHERE tenant_id = $1`, done.ID)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != "a completed closure settlement is frozen" {
		t.Fatalf("changing a completed settlement = %v, want it frozen", err)
	}
}

// The start rule needs every evidence column empty, not only the status.
func TestClosureSettlementStartsEmpty(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	postgrestest.EnableSettlementStartRule(t, e.db)
	for name, column := range map[string]string{
		"cancellation_completed_at": "cancellation_completed_at = now()",
		"drain_completed_at":        "drain_completed_at = now()",
		"finance_target_date":       "finance_target_date = current_date",
		"finance_completed_at":      "finance_completed_at = now()",
		"blocked_at":                "blocked_at = now()",
		"completed_at":              "completed_at = now()",
		"last_error":                "last_error = 'x'",
		"attempts":                  "attempts = 3",
	} {
		tn := e.mustOrg(e.person(name+"@example.test"), "Tenant "+name)
		set := column
		col := set[:strings.Index(set, " =")]
		_, err := e.db.Pool.Exec(ctx, `WITH t AS (SELECT $1::uuid AS brand_id, $2::uuid AS tenant_id)
INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status, `+col+`)
SELECT brand_id, tenant_id, now(), 'cancelling', `+set[strings.Index(set, "= ")+2:]+` FROM t`, tn.BrandID, tn.ID)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("a settlement started with %s set = %v, want a check violation", name, err)
		}
	}
	tn := e.mustOrg(e.person("clean@example.test"), "Tenant clean")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, now())`, tn.BrandID, tn.ID); err != nil {
		t.Fatalf("an empty settlement at cancelling = %v", err)
	}
}
