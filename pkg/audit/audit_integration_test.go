//go:build integration

package audit_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

func write(t *testing.T, db *postgrestest.DB, s scope.Scope, entry audit.Entry) audit.Event {
	t.Helper()
	var ev audit.Event
	err := pgx.BeginFunc(context.Background(), db.Pool, func(tx pgx.Tx) error {
		var err error
		ev, err = audit.Write(context.Background(), tx, s, entry)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func entry(actor, action string, at time.Time) audit.Entry {
	return audit.Entry{
		Actor:         events.Actor{Type: events.ActorUser, ID: actor},
		Action:        action,
		Reason:        "test",
		Outcome:       audit.OutcomeSuccess,
		CorrelationID: "corr-" + actor,
		OccurredAt:    at,
	}
}

func TestWriteAndQueryRoundTrip(t *testing.T) {
	db := postgrestest.New(t)
	s := postgrestest.NewScope(t)
	written := write(t, db, s, entry("alice", "tenant.member.added", time.Time{}))

	page, err := audit.Query(context.Background(), db.Pool, s, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.NextCursor != "" {
		t.Fatalf("page = %+v", page)
	}
	got := page.Events[0]
	if !reflect.DeepEqual(got, written) {
		t.Fatalf("read %+v, wrote %+v", got, written)
	}
}

func TestWriteRollsBackWithCaller(t *testing.T) {
	db := postgrestest.New(t)
	s := postgrestest.NewScope(t)
	ctx := context.Background()
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Write(ctx, tx, s, entry("alice", "tenant.updated", time.Time{})); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	page, err := audit.Query(ctx, db.Pool, s, audit.Filter{})
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("rolled-back audit event visible: %+v %v", page, err)
	}
}

func TestAuditTableIsAppendOnly(t *testing.T) {
	db := postgrestest.New(t)
	s := postgrestest.NewScope(t)
	write(t, db, s, entry("alice", "tenant.updated", time.Time{}))
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE audit_events SET outcome = 'failure'`,
		`DELETE FROM audit_events`,
		`TRUNCATE audit_events`,
	} {
		if _, err := db.Pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s succeeded on an append-only table", stmt)
		}
	}
}

func TestQueryFiltersAndPaginates(t *testing.T) {
	db := postgrestest.New(t)
	s := postgrestest.NewScope(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var all []audit.Event
	for i := 0; i < 7; i++ {
		actor := "alice"
		action := "tenant.member.added"
		if i%2 == 1 {
			actor = "bob"
			action = "tenant.member.removed"
		}
		all = append(all, write(t, db, s, entry(actor, action, base.Add(time.Duration(i)*time.Minute))))
	}
	// Two events at the same instant exercise the ID tiebreak.
	all = append(all, write(t, db, s, entry("carol", "tenant.updated", base.Add(10*time.Minute))))
	all = append(all, write(t, db, s, entry("carol", "tenant.updated", base.Add(10*time.Minute))))

	var seen []string
	cursor := ""
	pages := 0
	for {
		page, err := audit.Query(ctx, db.Pool, s, audit.Filter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range page.Events {
			seen = append(seen, e.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 5 || len(seen) != len(all) {
		t.Fatalf("pages = %d, events = %d", pages, len(seen))
	}
	for i := range all {
		if seen[i] != all[len(all)-1-i].ID {
			t.Fatalf("order mismatch at %d: newest-first expected", i)
		}
	}

	byActor, err := audit.Query(ctx, db.Pool, s, audit.Filter{ActorID: "bob"})
	if err != nil || len(byActor.Events) != 3 {
		t.Fatalf("actor filter = %d, %v", len(byActor.Events), err)
	}
	byAction, err := audit.Query(ctx, db.Pool, s, audit.Filter{Action: "tenant.member.added"})
	if err != nil || len(byAction.Events) != 4 {
		t.Fatalf("action filter = %d, %v", len(byAction.Events), err)
	}
	byTime, err := audit.Query(ctx, db.Pool, s, audit.Filter{OccurredAfter: base.Add(2 * time.Minute), OccurredBefore: base.Add(5 * time.Minute)})
	if err != nil || len(byTime.Events) != 3 {
		t.Fatalf("time filter = %d, %v", len(byTime.Events), err)
	}
	combined, err := audit.Query(ctx, db.Pool, s, audit.Filter{ActorID: "alice", OccurredAfter: base.Add(time.Minute)})
	if err != nil || len(combined.Events) != 3 {
		t.Fatalf("combined filter = %d, %v", len(combined.Events), err)
	}

	if _, err := audit.Query(ctx, db.Pool, s, audit.Filter{Cursor: "garbage"}); !zserrors.IsCode(err, zserrors.CodeInvalidCursor) {
		t.Fatalf("bad cursor err = %v", err)
	}
}

// TestIsolation proves the brand and tenant boundary of audit queries: a
// query scoped to brand A never returns brand B's events, and a query scoped
// to one tenant never returns another tenant's events in the same brand, even
// when tenant IDs, actors, actions and times are identical.
func TestIsolation(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

	brandA := postgrestest.NewScope(t)
	brandB := scope.Scope{Brand: postgrestest.NewScope(t).Brand, Tenant: brandA.Tenant}
	tenantA2 := scope.Scope{Brand: brandA.Brand, Tenant: postgrestest.NewScope(t).Tenant}

	for i := 0; i < 3; i++ {
		write(t, db, brandA, entry("shared-actor", "tenant.updated", at))
		write(t, db, brandB, entry("shared-actor", "tenant.updated", at))
		write(t, db, tenantA2, entry("shared-actor", "tenant.updated", at))
	}

	cases := []struct {
		name  string
		scope scope.Scope
	}{
		{"brand A tenant 1", brandA},
		{"brand B same tenant UUID", brandB},
		{"brand A tenant 2", tenantA2},
	}
	for _, c := range cases {
		page, err := audit.Query(ctx, db.Pool, c.scope, audit.Filter{ActorID: "shared-actor", Action: "tenant.updated", Limit: audit.MaxLimit})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 3 {
			t.Errorf("%s: got %d events, want 3", c.name, len(page.Events))
		}
		for _, e := range page.Events {
			if e.BrandID != c.scope.BrandID() || e.TenantID != c.scope.TenantID() {
				t.Errorf("%s: returned an event outside its scope: %+v", c.name, e)
			}
		}
	}

	// A cursor taken from one scope cannot widen another scope's query.
	pageA, err := audit.Query(ctx, db.Pool, brandA, audit.Filter{Limit: 1})
	if err != nil || pageA.NextCursor == "" {
		t.Fatalf("pageA = %+v, %v", pageA, err)
	}
	pageB, err := audit.Query(ctx, db.Pool, brandB, audit.Filter{Cursor: pageA.NextCursor, Limit: audit.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pageB.Events {
		if e.BrandID != brandB.BrandID() {
			t.Fatalf("cursor reuse crossed brands: %+v", e)
		}
	}

	// A scope missing the tenant or the brand is refused before any query.
	for _, partial := range []scope.Scope{{Brand: brandA.Brand}, {Tenant: brandA.Tenant}, {}} {
		if _, err := audit.Query(ctx, db.Pool, partial, audit.Filter{}); !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
			t.Errorf("partial scope %+v: err = %v", partial, err)
		}
		err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
			_, err := audit.Write(ctx, tx, partial, entry("x", "tenant.updated", at))
			return err
		})
		if !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
			t.Errorf("partial scope write %+v: err = %v", partial, err)
		}
	}
}

func TestBrandEventsAreSeparateFromTenantEvents(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	s := postgrestest.NewScope(t)
	otherBrand := postgrestest.NewScope(t).Brand
	write := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, db.Pool, fn); err != nil {
			t.Fatal(err)
		}
	}
	entry := audit.Entry{Actor: events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}, Action: "brand.keycloak.realm.updated",
		Outcome: audit.OutcomeSuccess, CorrelationID: "c", Details: map[string]any{"change": map[string]any{"field": "identity.login_theme"}}}
	write(func(tx pgx.Tx) error { _, err := audit.WriteBrand(ctx, tx, s.Brand, entry); return err })
	write(func(tx pgx.Tx) error { _, err := audit.WriteBrand(ctx, tx, otherBrand, entry); return err })
	write(func(tx pgx.Tx) error { _, err := audit.Write(ctx, tx, s, entry); return err })

	page, err := audit.QueryBrand(ctx, db.Pool, s.Brand, audit.Filter{})
	if err != nil || len(page.Events) != 1 || page.Events[0].TenantID != "" || page.Events[0].BrandID != s.BrandID() {
		t.Fatalf("brand events = %+v, %v", page.Events, err)
	}
	if got := page.Events[0].Details["change"].(map[string]any)["field"]; got != "identity.login_theme" {
		t.Fatalf("details = %+v", page.Events[0].Details)
	}
	page, err = audit.Query(ctx, db.Pool, s, audit.Filter{})
	if err != nil || len(page.Events) != 1 || page.Events[0].TenantID != s.TenantID() {
		t.Fatalf("tenant events = %+v, %v", page.Events, err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := audit.WriteBrand(ctx, tx, idgen.Nil, entry)
		return err
	}); err == nil {
		t.Fatal("brand event without a brand was written")
	}
}
