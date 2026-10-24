//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

type env struct {
	t     *testing.T
	db    *postgrestest.DB
	fake  *enginetest.Fake
	eng   tenant.Engines
	brand enginetest.Brand
}

type sweepUnlockCancelTracer struct {
	enabled atomic.Bool
	hits    atomic.Int64
	child   bool
}

func (t *sweepUnlockCancelTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t.enabled.Load() && strings.Contains(data.SQL, "pg_advisory_unlock") && len(data.Args) == 1 {
		if key, ok := data.Args[0].(int64); ok && (key < 0) == t.child {
			t.hits.Add(1)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}
	}
	return ctx
}

func (*sweepUnlockCancelTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func newEnv(t *testing.T, permissions ...string) *env {
	t.Helper()
	db := postgrestest.New(t)
	fake := enginetest.New(t)
	return &env{t: t, db: db, fake: fake, brand: fake.InsertBrand(t, db.Pool, "zs-acme", permissions...),
		eng: tenant.Engines{Keycloak: fake.Keycloak(t), KillBill: fake.KillBill(t), Secrets: fake.Secrets()}}
}

func (e *env) person(email string) enginetest.Person {
	return enginetest.InsertPerson(e.t, e.db.Pool, e.brand, email)
}

func userActor(p enginetest.Person) events.Actor {
	return events.Actor{Type: events.ActorUser, ID: p.Identity.PublicID()}
}

func (e *env) createOrg(owner enginetest.Person, name, key string) (tenant.Tenant, error) {
	return tenant.CreateOrganization(context.Background(), e.db.Pool, e.eng, tenant.CreateRequest{
		Brand: e.brand.ID, Creator: owner.Identity.ID, Name: name, IdempotencyKey: key, BillingCountry: "CA",
		Actor: userActor(owner), CorrelationID: "test",
	})
}

func (e *env) mustOrg(owner enginetest.Person, name string) tenant.Tenant {
	e.t.Helper()
	tn, err := e.createOrg(owner, name, "key-"+idgen.MustNewV7().String())
	if err != nil {
		e.t.Fatal(err)
	}
	return tn
}

// caller builds the caller scope resolution would build for a membership.
func (e *env) caller(sc scope.Scope, p enginetest.Person, mfa bool) tenant.Caller {
	e.t.Helper()
	m, err := tenant.MembershipOf(context.Background(), e.db.Pool, sc, p.Identity.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	perms, m, err := tenant.Effective(context.Background(), e.db.Pool, sc, m.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return tenant.Caller{Membership: m, Permissions: perms, RecentMFA: mfa, Actor: userActor(p), CorrelationID: "test"}
}

func (e *env) tx(fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(context.Background(), e.db.Pool, fn)
}

// join invites p with role and accepts the invitation.
func (e *env) join(sc scope.Scope, inviter tenant.Caller, p enginetest.Person, role tenant.Role) tenant.Membership {
	e.t.Helper()
	var m tenant.Membership
	err := e.tx(func(tx pgx.Tx) error {
		_, token, err := tenant.CreateInvitation(context.Background(), tx, sc, inviter, p.Email, role)
		if err != nil {
			return err
		}
		_, m, err = tenant.AcceptInvitation(context.Background(), tx, acceptor(p), token)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func acceptor(p enginetest.Person) tenant.Acceptor {
	return tenant.Acceptor{Brand: p.Brand.ID, IdentityID: p.Identity.ID, Email: p.Email, EmailVerified: true, RecentAuth: true, CorrelationID: "test"}
}

func code(err error) string {
	if err == nil {
		return "ok"
	}
	var e *zserrors.Error
	if !errors.As(err, &e) {
		return "untyped: " + err.Error()
	}
	return zserrors.Lookup(e.Code).Name
}

func actions(t *testing.T, db *postgrestest.DB, sc scope.Scope) []string {
	t.Helper()
	page, err := audit.Query(context.Background(), db.Pool, sc, audit.Filter{Limit: audit.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range page.Events {
		out = append(out, ev.Action+":"+string(ev.Outcome))
	}
	return out
}

// Creating an organization creates a Keycloak Organization and a Kill Bill
// account that carry the tenant ID, the owner membership and the three
// locked system teams. The same idempotency key and payload return the same
// tenant; another payload is idempotency_key_conflict.
func TestCreateOrganizationMapsTheEnginesAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	owner := e.person("owner@example.test")
	tn, err := e.createOrg(owner, "Acme Inc.", "create-key-1")
	if err != nil {
		t.Fatal(err)
	}
	if tn.Kind != tenant.KindOrganization || tn.Status != tenant.StatusActive || tn.Activation != tenant.ActivationPending ||
		!strings.HasPrefix(tn.Slug, "acme-inc-") || tn.BillingCountry != "CA" {
		t.Fatalf("tenant = %+v", tn)
	}
	org, members, ok := e.fake.Organization(e.brand.Realm, tn.PublicID())
	if !ok || org.Alias != tn.PublicID() || org.Name != "Acme Inc." || !org.Enabled || org.ID != tn.KeycloakOrganizationID ||
		len(members) != 1 || members[0] != owner.Subject {
		t.Fatalf("organization = %+v %v %v", org, members, ok)
	}
	account, ok := e.fake.Account(e.brand.Slug, tn.PublicID())
	if !ok || account.ID != tn.KillBillAccountID || account.Currency != "CAD" || account.Name != "Acme Inc." || account.Country != "CA" {
		t.Fatalf("account = %+v %v", account, ok)
	}

	again, err := e.createOrg(owner, "Acme Inc.", "create-key-1")
	if err != nil || again.ID != tn.ID {
		t.Fatalf("replay = %v %v", again.PublicID(), err)
	}
	if _, err := e.createOrg(owner, "Acme Other", "create-key-1"); code(err) != "idempotency_key_conflict" {
		t.Fatalf("other payload: %v", err)
	}
	// Another person may use the same key: keys are scoped to the creator.
	other := e.person("other@example.test")
	if second, err := e.createOrg(other, "Acme Inc.", "create-key-1"); err != nil || second.ID == tn.ID {
		t.Fatalf("other creator: %v %v", second.PublicID(), err)
	}
	if _, err := e.createOrg(owner, "Acme", "short"); code(err) != "idempotency_key_invalid" {
		t.Fatalf("short key: %v", err)
	}

	teams, _, err := tenant.ListTeams(context.Background(), e.db.Pool, tn.Scope(), tenant.Page{})
	if err != nil || len(teams) != 3 {
		t.Fatalf("teams = %+v %v", teams, err)
	}
	m, err := tenant.MembershipOf(context.Background(), e.db.Pool, tn.Scope(), owner.Identity.ID)
	if err != nil || m.Role != tenant.RoleOwner || m.Status != tenant.MembershipActive {
		t.Fatalf("owner membership = %+v %v", m, err)
	}
	for _, team := range teams {
		if !team.System || !team.Locked {
			t.Fatalf("team %s is not a locked system team", team.Name)
		}
		in := slices.Contains(team.MembershipIDs, m.ID)
		if (team.Name == tenant.TeamAdmins) == in {
			t.Fatalf("owner in team %s = %v", team.Name, in)
		}
	}
	if got := actions(t, e.db, tn.Scope()); !slices.Contains(got, "tenant.created:success") {
		t.Fatalf("audit = %v", got)
	}
	// Brand isolation: the tenant does not exist in another brand.
	otherBrand := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
	if _, err := tenant.Get(context.Background(), e.db.Pool, scope.Scope{Brand: otherBrand.ID, Tenant: tn.ID}); code(err) != "tenant_not_found" {
		t.Fatalf("tenant visible in another brand: %v", err)
	}
}

// When Kill Bill is unreachable, no tenant row is written, the failure is
// audited, and the sweeper completes the same tenant once Kill Bill is back.
// The Keycloak Organization created before the failure is reused.
func TestCreateRollsBackAndTheSweeperCompletesIt(t *testing.T) {
	e := newEnv(t)
	owner := e.person("owner@example.test")
	e.fake.SetDown(false, true)
	_, err := e.createOrg(owner, "Rollback Ltd", "rollback-key")
	if code(err) != "billing_unavailable" {
		t.Fatalf("create with Kill Bill down: %v", err)
	}
	var tenantID idgen.UUID
	var orgID string
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT tenant_id, keycloak_organization_id FROM tenant_provisioning
WHERE status = 'pending' AND last_error LIKE 'killbill.account:%'`).Scan(&tenantID, &orgID); err != nil {
		t.Fatalf("pending creation: %v", err)
	}
	sc := scope.Scope{Brand: e.brand.ID, Tenant: tenantID}
	if _, err := tenant.Get(context.Background(), e.db.Pool, sc); code(err) != "tenant_not_found" {
		t.Fatalf("tenant row exists after the failure: %v", err)
	}
	if got := actions(t, e.db, sc); len(got) != 1 || got[0] != "tenant.created:failure" {
		t.Fatalf("audit = %v", got)
	}
	if _, _, ok := e.fake.Organization(e.brand.Realm, idgen.Format(idgen.Tenant, tenantID)); !ok {
		t.Fatal("the organization created before the failure is missing")
	}

	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}
	// Not due yet: the sweeper waits for the retry delay.
	if report, err := sweeper.SweepOnce(context.Background()); err != nil || report.Completed != 0 {
		t.Fatalf("early sweep = %+v %v", report, err)
	}
	e.fake.SetDown(false, false)
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE tenant_provisioning SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	report, err := sweeper.SweepOnce(context.Background())
	if err != nil || report.Completed != 1 {
		t.Fatalf("sweep = %+v %v", report, err)
	}
	tn, err := tenant.Get(context.Background(), e.db.Pool, sc)
	if err != nil || tn.KeycloakOrganizationID != orgID {
		t.Fatalf("tenant after sweep = %+v %v", tn, err)
	}
	if a, ok := e.fake.Account(e.brand.Slug, tn.PublicID()); !ok || a.ID != tn.KillBillAccountID {
		t.Fatalf("account = %+v %v", a, ok)
	}
	wantAudit := []string{"tenant.billing_recipient.reconciled:success", "tenant.created:success", "tenant.created:failure"}
	if got := actions(t, e.db, sc); !slices.Equal(got, wantAudit) {
		t.Fatalf("audit after sweep = %v, want %v", got, wantAudit)
	}
	// The client's retry with the same key now returns the tenant.
	if again, err := e.createOrg(owner, "Rollback Ltd", "rollback-key"); err != nil || again.ID != tenantID {
		t.Fatalf("retry = %v %v", again.PublicID(), err)
	}
}

func TestSweeperPersistsCreationCursorAfterExaminedWork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	first := e.person("cursor-first@example.test")
	second := e.person("cursor-second@example.test")
	if _, err := e.createOrg(first, "Cursor First", "cursor-first-key"); code(err) != "billing_unavailable" {
		t.Fatalf("first creation = %v", err)
	}
	if _, err := e.createOrg(second, "Cursor Second", "cursor-second-key"); code(err) != "billing_unavailable" {
		t.Fatalf("second creation = %v", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = date_trunc('minute', now())
        WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDown(false, false)
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Batch: 1}).SweepOnce(ctx)
	if err != nil || report.Completed != 2 {
		t.Fatalf("sweep = %+v %v", report, err)
	}
	var wrapped bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id IS NULL FROM sweep_cursor WHERE kind = 'creation'`).Scan(&wrapped); err != nil {
		t.Fatalf("creation cursor = %v", err)
	}
	if !wrapped {
		t.Fatal("creation cursor did not wrap after the end of eligible work")
	}
}

func TestSweeperCompletesCreationWithOneConnection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	owner := e.person("one-connection@example.test")
	if _, err := e.createOrg(owner, "One Connection", "one-connection-key"); code(err) != "billing_unavailable" {
		t.Fatalf("creation = %v", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = now() WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e.fake.SetDown(false, false)
	sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	report, err := (&tenant.Sweeper{DB: pool, Engines: e.eng, Batch: 1}).SweepOnce(sweepCtx)
	if err != nil || report.Completed != 1 {
		t.Fatalf("one-connection sweep = %+v %v", report, err)
	}
}

func TestSweeperCancellationDoesNotRetainOneConnection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	owner := e.person("cancelled-sweep@example.test")
	if _, err := e.createOrg(owner, "Cancelled Sweep", "cancelled-sweep-key"); code(err) != "billing_unavailable" {
		t.Fatalf("creation = %v", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = now() WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (&tenant.Sweeper{DB: pool, Engines: e.eng}).SweepOnce(cancelled); err == nil {
		t.Fatal("cancelled sweep succeeded")
	}
	e.fake.SetDown(false, false)
	report, err := (&tenant.Sweeper{DB: pool, Engines: e.eng}).SweepOnce(ctx)
	if err != nil || report.Completed != 1 {
		t.Fatalf("sweep after cancellation = %+v %v", report, err)
	}
}

func TestSweeperReturnsVerifiedGlobalReleaseFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tracer := &sweepUnlockCancelTracer{}
	cfg, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tracer.enabled.Store(true)
	if _, err := (&tenant.Sweeper{DB: pool, Engines: e.eng}).SweepOnce(ctx); !errors.Is(err, context.Canceled) || tracer.hits.Load() == 0 {
		t.Fatalf("global release failure = %v hits=%d", err, tracer.hits.Load())
	}
	tracer.enabled.Store(false)
	if report, err := (&tenant.Sweeper{DB: pool, Engines: e.eng}).SweepOnce(ctx); err != nil || report.Skipped {
		t.Fatalf("sweep after global release failure = %+v %v", report, err)
	}
}

func TestSweeperReturnsVerifiedChildReleaseFailure(t *testing.T) {
	e := newEnv(t)
	owner := e.person("child-release@example.test")
	e.mustOrg(owner, "Child Release")
	ctx := context.Background()
	tracer := &sweepUnlockCancelTracer{child: true}
	cfg, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e.fake.SetDown(true, false)
	tracer.enabled.Store(true)
	report, err := (&tenant.Sweeper{DB: pool, Engines: e.eng}).SweepOnce(ctx)
	if !errors.Is(err, context.Canceled) || tracer.hits.Load() == 0 || report.Errors == 0 {
		t.Fatalf("child release failure = %+v %v hits=%d", report, err, tracer.hits.Load())
	}
}

func TestSweeperMidFlightCancellationLeavesCreationRetryable(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fake.SetDown(false, true)
	owner := e.person("midflight-cancel@example.test")
	if _, err := e.createOrg(owner, "Midflight Cancel", "midflight-cancel-key"); code(err) != "billing_unavailable" {
		t.Fatalf("creation = %v", err)
	}
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE tenant_provisioning SET next_attempt_at = now() WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var fired atomic.Bool
	e.fake.AfterWrite(func(_, _ string) {
		if fired.CompareAndSwap(false, true) {
			cancel()
		}
	})
	t.Cleanup(func() { e.fake.AfterWrite(nil) })
	e.fake.SetDown(false, false)
	if _, err := (&tenant.Sweeper{DB: pool, Engines: e.eng}).SweepOnce(ctx); err == nil || !fired.Load() {
		t.Fatalf("midflight cancellation = %v fired=%v", err, fired.Load())
	}
	e.fake.AfterWrite(nil)
	var one int
	if err := pool.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("pool after midflight cancellation = %d %v", one, err)
	}
	report, err := (&tenant.Sweeper{DB: pool, Engines: e.eng}).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweep after midflight cancellation = %+v %v", report, err)
	}
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM tenant_provisioning`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" && status != "completed" {
		t.Fatalf("midflight creation status = %q", status)
	}
	if status == "pending" && report.Completed != 0 {
		t.Fatalf("pending midflight creation completed count = %d", report.Completed)
	}
}

func TestSweeperCreationCursorAdvancesPastLockedPrefix(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	first := e.person("locked-prefix-first@example.test")
	second := e.person("locked-prefix-second@example.test")
	if _, err := e.createOrg(first, "Locked Prefix First", "locked-prefix-first-key"); code(err) != "billing_unavailable" {
		t.Fatalf("first creation = %v", err)
	}
	if _, err := e.createOrg(second, "Locked Prefix Second", "locked-prefix-second-key"); code(err) != "billing_unavailable" {
		t.Fatalf("second creation = %v", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = date_trunc('minute', now())
        WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	var lockedID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT tenant_id FROM tenant_provisioning WHERE status = 'pending'
        ORDER BY next_attempt_at, tenant_id LIMIT 1`).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	lock, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Rollback(context.Background()) })
	if _, err := lock.Exec(ctx, `SELECT tenant_id FROM tenant_provisioning WHERE tenant_id = $1 FOR UPDATE`, lockedID); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDown(false, false)
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Batch: 1}).SweepOnce(ctx)
	if err != nil || report.Completed != 1 {
		t.Fatalf("locked-prefix sweep = %+v %v", report, err)
	}
	var exists bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sweep_cursor WHERE kind = 'creation')`).Scan(&exists); err != nil {
		t.Fatalf("creation cursor = %v", err)
	}
	if !exists {
		t.Fatal("creation cursor was not persisted")
	}
	var status string
	if err := e.db.Pool.QueryRow(ctx, `SELECT status FROM tenant_provisioning WHERE tenant_id = $1`, lockedID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("locked prefix status = %q, want pending", status)
	}
}

func TestSweeperCreationCursorCapsWorkAndResumes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	for i := range 51 {
		owner := e.person(fmt.Sprintf("cursor-cap-%02d@example.test", i))
		if _, err := e.createOrg(owner, fmt.Sprintf("Cursor Cap %02d", i), fmt.Sprintf("cursor-cap-key-%02d", i)); code(err) != "billing_unavailable" {
			t.Fatalf("creation %d = %v", i, err)
		}
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = date_trunc('minute', now())
        WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	var fiftieth idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT tenant_id FROM tenant_provisioning WHERE status = 'pending'
        ORDER BY next_attempt_at, tenant_id OFFSET 49 LIMIT 1`).Scan(&fiftieth); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDown(false, false)
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}
	first, err := sweeper.SweepOnce(ctx)
	if err != nil || first.Completed != 50 {
		t.Fatalf("first capped sweep = %+v %v", first, err)
	}
	var cursor idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id FROM sweep_cursor WHERE kind = 'creation'`).Scan(&cursor); err != nil {
		t.Fatalf("first creation cursor = %v", err)
	}
	if cursor != fiftieth {
		t.Fatalf("first cursor = %s, want %s", cursor, fiftieth)
	}
	second, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(ctx)
	if err != nil || second.Completed != 1 {
		t.Fatalf("resume sweep = %+v %v", second, err)
	}
	var wrapped bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id IS NULL FROM sweep_cursor WHERE kind = 'creation'`).Scan(&wrapped); err != nil {
		t.Fatalf("wrapped creation cursor = %v", err)
	}
	if !wrapped {
		t.Fatal("creation cursor did not wrap after resumed final work")
	}
}

func TestSweeperCreationCursorScansBusyPrefixBeforeResuming(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	for i := range 501 {
		owner := e.person(fmt.Sprintf("cursor-scan-%03d@example.test", i))
		if _, err := e.createOrg(owner, fmt.Sprintf("Cursor Scan %03d", i), fmt.Sprintf("cursor-scan-key-%03d", i)); code(err) != "billing_unavailable" {
			t.Fatalf("creation %d = %v", i, err)
		}
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = date_trunc('minute', now()) WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	var fiveHundredth idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT tenant_id FROM tenant_provisioning WHERE status = 'pending'
        ORDER BY next_attempt_at, tenant_id OFFSET 499 LIMIT 1`).Scan(&fiveHundredth); err != nil {
		t.Fatal(err)
	}
	locker, err := e.db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = locker.Exec(context.Background(), `SELECT pg_advisory_unlock_all()`)
		locker.Release()
	})
	rows, err := locker.Query(ctx, `SELECT pg_advisory_lock(
    hashtextextended('zs-core.tenant.closure:' || current_schema() || ':' || brand_id::text || ':' || tenant_id::text, 0)
    | '-9223372036854775808'::bigint
) FROM (
    SELECT brand_id, tenant_id FROM tenant_provisioning WHERE status = 'pending'
    ORDER BY next_attempt_at, tenant_id LIMIT 500
) locked`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	e.fake.SetDown(false, false)
	first, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(ctx)
	if err != nil || first.Completed != 0 {
		t.Fatalf("busy-prefix sweep = %+v %v", first, err)
	}
	var cursor idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id FROM sweep_cursor WHERE kind = 'creation'`).Scan(&cursor); err != nil {
		t.Fatalf("busy-prefix cursor = %v", err)
	}
	if cursor != fiveHundredth {
		t.Fatalf("busy-prefix cursor = %s, want %s", cursor, fiveHundredth)
	}
	second, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(ctx)
	if err != nil || second.Completed != 1 {
		t.Fatalf("busy-prefix resume = %+v %v", second, err)
	}
	var pending int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_provisioning WHERE status = 'pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 500 {
		t.Fatalf("busy pending = %d, want 500", pending)
	}
	var wrapped bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id IS NULL FROM sweep_cursor WHERE kind = 'creation'`).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	if !wrapped {
		t.Fatal("busy-prefix cursor did not wrap")
	}
}

func TestSweeperCreationCursorResumesAfterStoredRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	first := e.person("cursor-resume-first@example.test")
	second := e.person("cursor-resume-second@example.test")
	if _, err := e.createOrg(first, "Cursor Resume First", "cursor-resume-first-key"); code(err) != "billing_unavailable" {
		t.Fatalf("first creation = %v", err)
	}
	if _, err := e.createOrg(second, "Cursor Resume Second", "cursor-resume-second-key"); code(err) != "billing_unavailable" {
		t.Fatalf("second creation = %v", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = date_trunc('minute', now()) WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	var firstID idgen.UUID
	var eligibleAt time.Time
	if err := e.db.Pool.QueryRow(ctx, `SELECT tenant_id, next_attempt_at FROM tenant_provisioning WHERE status = 'pending' ORDER BY next_attempt_at, tenant_id LIMIT 1`).Scan(&firstID, &eligibleAt); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO sweep_cursor (kind, last_eligible_at, last_tenant_id) VALUES ('creation', $1, $2)`, eligibleAt, firstID); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDown(false, false)
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Batch: 1}).SweepOnce(ctx)
	if err != nil || report.Completed != 1 {
		t.Fatalf("resumed sweep = %+v %v", report, err)
	}
	var firstStatus string
	if err := e.db.Pool.QueryRow(ctx, `SELECT status FROM tenant_provisioning WHERE tenant_id = $1`, firstID).Scan(&firstStatus); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "pending" {
		t.Fatalf("stored cursor predecessor = %q, want pending", firstStatus)
	}
	var wrapped bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id IS NULL FROM sweep_cursor WHERE kind = 'creation'`).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	if !wrapped {
		t.Fatal("creation cursor did not wrap after resumed tail")
	}
}

func TestPersonalTenantIsOnePerIdentity(t *testing.T) {
	e := newEnv(t)
	p := e.person("solo@example.test")
	req := tenant.CreateRequest{Brand: e.brand.ID, Creator: p.Identity.ID, Name: "Solo", Actor: userActor(p), CorrelationID: "test"}
	first, created, err := tenant.EnsurePersonal(context.Background(), e.db.Pool, e.eng, req)
	if err != nil || !created || first.Kind != tenant.KindPersonal {
		t.Fatalf("first = %+v %v %v", first, created, err)
	}
	second, created, err := tenant.EnsurePersonal(context.Background(), e.db.Pool, e.eng, req)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("second = %+v %v %v", second, created, err)
	}
	owner := e.caller(first.Scope(), p, true)
	err = e.tx(func(tx pgx.Tx) error {
		_, _, err := tenant.CreateInvitation(context.Background(), tx, first.Scope(), owner, "friend@example.test", tenant.RoleMember)
		return err
	})
	if code(err) != "tenant_kind_unsupported" {
		t.Fatalf("invitation to a personal tenant: %v", err)
	}
}

// The last active owner cannot be removed, suspended or demoted, and an
// ownership transfer always leaves an owner.
func TestLastOwnerGuard(t *testing.T) {
	e := newEnv(t)
	ownerP, adminP := e.person("owner@example.test"), e.person("admin@example.test")
	tn := e.mustOrg(ownerP, "Guarded")
	sc := tn.Scope()
	owner := e.caller(sc, ownerP, true)
	adminM := e.join(sc, owner, adminP, tenant.RoleAdmin)
	ctx := context.Background()
	member := tenant.RoleMember
	suspended := tenant.MembershipSuspended

	for name, op := range map[string]func(tx pgx.Tx) error{
		"remove": func(tx pgx.Tx) error {
			_, err := tenant.RemoveMember(ctx, tx, sc, owner, owner.Membership.ID)
			return err
		},
		"suspend": func(tx pgx.Tx) error {
			_, err := tenant.UpdateMember(ctx, tx, sc, owner, owner.Membership.ID, tenant.MemberChange{Status: &suspended})
			return err
		},
		"demote": func(tx pgx.Tx) error {
			_, err := tenant.UpdateMember(ctx, tx, sc, owner, owner.Membership.ID, tenant.MemberChange{Role: &member})
			return err
		},
	} {
		if err := e.tx(op); code(err) != "last_owner" {
			t.Errorf("%s the last owner: %v", name, err)
		}
	}
	// Changes need recent MFA.
	noMFA := owner
	noMFA.RecentMFA = false
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, sc, noMFA, adminM.ID)
		return err
	}); code(err) != "recent_mfa_required" {
		t.Fatalf("remove without MFA: %v", err)
	}
	// An admin cannot touch the owner.
	admin := e.caller(sc, adminP, true)
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, admin, owner.Membership.ID, tenant.MemberChange{Role: &member})
		return err
	}); code(err) != "permission_denied" {
		t.Fatalf("admin demoting the owner: %v", err)
	}
	// Transfer needs an active admin target and the owner role.
	if err := e.tx(func(tx pgx.Tx) error {
		_, _, err := tenant.TransferOwnership(ctx, tx, sc, admin, owner.Membership.ID)
		return err
	}); code(err) != "permission_denied" {
		t.Fatalf("admin transferring: %v", err)
	}
	if err := e.tx(func(tx pgx.Tx) error {
		_, _, err := tenant.TransferOwnership(ctx, tx, sc, noMFA, adminM.ID)
		return err
	}); code(err) != "recent_mfa_required" {
		t.Fatalf("transfer without MFA: %v", err)
	}
	var from, to tenant.Membership
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		from, to, err = tenant.TransferOwnership(ctx, tx, sc, owner, adminM.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if from.Role != tenant.RoleAdmin || to.Role != tenant.RoleOwner {
		t.Fatalf("transfer = %s -> %s", from.Role, to.Role)
	}
	// The new owner is now the last owner and is guarded in turn.
	newOwner := e.caller(sc, adminP, true)
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, sc, newOwner, newOwner.Membership.ID)
		return err
	}); code(err) != "last_owner" {
		t.Fatalf("remove the new last owner: %v", err)
	}
	// With a second owner, the first may step down.
	ownerRole := tenant.RoleOwner
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, newOwner, from.ID, tenant.MemberChange{Role: &ownerRole})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, newOwner, newOwner.Membership.ID, tenant.MemberChange{Role: &member})
		return err
	}); err != nil {
		t.Fatalf("demote one of two owners: %v", err)
	}
	if got := actions(t, e.db, sc); !slices.Contains(got, "tenant.ownership.transferred:success") {
		t.Fatalf("audit = %v", got)
	}
}

// Nobody gives out more than they hold: roles, team grants and team
// membership all check the caller's own permissions.
func TestPermissionsCannotEscalate(t *testing.T) {
	e := newEnv(t, "vm:create", "billing:refund")
	ownerP, adminP, memberP := e.person("owner@example.test"), e.person("admin@example.test"), e.person("member@example.test")
	tn := e.mustOrg(ownerP, "Escalation")
	sc := tn.Scope()
	ctx := context.Background()
	owner := e.caller(sc, ownerP, true)
	e.join(sc, owner, adminP, tenant.RoleAdmin)
	memberM := e.join(sc, owner, memberP, tenant.RoleMember)
	admin := e.caller(sc, adminP, true)
	member := e.caller(sc, memberP, true)

	if slices.Contains(admin.Permissions, "billing:read") || !slices.Contains(admin.Permissions, "vm:create") {
		t.Fatalf("admin permissions = %v", admin.Permissions)
	}
	if strings.Join(member.Permissions, ",") != "member:read,team:read,tenant:read" {
		t.Fatalf("member permissions = %v", member.Permissions)
	}
	if !slices.Contains(owner.Permissions, "billing:refund") {
		t.Fatalf("owner lacks a registered permission: %v", owner.Permissions)
	}
	if err := e.tx(func(tx pgx.Tx) error {
		_, _, err := tenant.CreateInvitation(ctx, tx, sc, admin, "b@example.test", tenant.RoleBillingAdmin)
		return err
	}); code(err) != "permission_denied" {
		t.Fatalf("admin inviting a billing admin: %v", err)
	}
	if err := e.tx(func(tx pgx.Tx) error {
		_, _, err := tenant.CreateInvitation(ctx, tx, sc, owner, "o@example.test", tenant.RoleOwner)
		return err
	}); code(err) != "validation_failed" {
		t.Fatalf("inviting an owner: %v", err)
	}

	var team tenant.Team
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		team, err = tenant.CreateTeam(ctx, tx, sc, admin, "Operators")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		caller tenant.Caller
		perms  []string
		want   string
	}{
		"unregistered":           {owner, []string{"vm:delete"}, "permission_unknown"},
		"admin grants billing":   {admin, []string{"billing:read"}, "permission_denied"},
		"member lacks manage":    {member, []string{"tenant:read"}, "permission_denied"},
		"admin grants vm:create": {admin, []string{"vm:create", "tenant:read"}, "ok"},
	} {
		err := e.tx(func(tx pgx.Tx) error {
			_, err := tenant.SetTeamGrants(ctx, tx, sc, c.caller, team.ID, c.perms)
			return err
		})
		if code(err) != c.want {
			t.Errorf("%s: %v, want %s", name, err, c.want)
		}
	}
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.SetTeamMembers(ctx, tx, sc, admin, team.ID, []idgen.UUID{memberM.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	member = e.caller(sc, memberP, true)
	if !slices.Contains(member.Permissions, "vm:create") {
		t.Fatalf("member permissions after the team grant = %v", member.Permissions)
	}
	// The owner grants billing to the team; the admin can no longer manage it.
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.SetTeamGrants(ctx, tx, sc, owner, team.ID, []string{"vm:create", "billing:read"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.tx(func(tx pgx.Tx) error {
		return tenant.DeleteTeam(ctx, tx, sc, admin, team.ID)
	}); code(err) != "permission_denied" {
		t.Fatalf("admin deleting a team that grants billing: %v", err)
	}
	// System teams are locked.
	teams, _, _ := tenant.ListTeams(ctx, e.db.Pool, sc, tenant.Page{})
	for _, st := range teams {
		if !st.System {
			continue
		}
		if err := e.tx(func(tx pgx.Tx) error {
			_, err := tenant.SetTeamMembers(ctx, tx, sc, owner, st.ID, nil)
			return err
		}); code(err) != "team_locked" {
			t.Fatalf("set members of %s: %v", st.Name, err)
		}
		if err := e.tx(func(tx pgx.Tx) error { return tenant.DeleteTeam(ctx, tx, sc, owner, st.ID) }); code(err) != "team_locked" {
			t.Fatalf("delete %s: %v", st.Name, err)
		}
	}
	// A role change moves the member between system teams in the same
	// transaction.
	adminRole := tenant.RoleAdmin
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, owner, memberM.ID, tenant.MemberChange{Role: &adminRole})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	teams, _, _ = tenant.ListTeams(ctx, e.db.Pool, sc, tenant.Page{})
	for _, st := range teams {
		if st.Name == tenant.TeamAdmins && !slices.Contains(st.MembershipIDs, memberM.ID) {
			t.Fatal("the promoted member is not in the admins team")
		}
	}
}

func TestInvitationAcceptance(t *testing.T) {
	e := newEnv(t)
	ownerP := e.person("owner@example.test")
	tn := e.mustOrg(ownerP, "Invites")
	sc := tn.Scope()
	ctx := context.Background()
	owner := e.caller(sc, ownerP, true)
	invite := func(email string) (tenant.Invitation, string) {
		t.Helper()
		var inv tenant.Invitation
		var token string
		if err := e.tx(func(tx pgx.Tx) error {
			var err error
			inv, token, err = tenant.CreateInvitation(ctx, tx, sc, owner, email, tenant.RoleMember)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return inv, token
	}
	accept := func(a tenant.Acceptor, token string) error {
		return e.tx(func(tx pgx.Tx) error {
			_, _, err := tenant.AcceptInvitation(ctx, tx, a, token)
			return err
		})
	}
	var stored []byte
	_, token := invite("Invitee@Example.TEST")
	if err := e.db.Pool.QueryRow(ctx, `SELECT token_digest FROM invitations`).Scan(&stored); err != nil || string(stored) == token || len(stored) != 32 {
		t.Fatalf("stored token = %d bytes, %v", len(stored), err)
	}
	other := e.person("someone.else@example.test")
	if err := accept(acceptor(other), token); code(err) != "invitation_email_mismatch" {
		t.Fatalf("different verified email: %v", err)
	}
	invitee := e.person("invitee@example.test")
	unverified := acceptor(invitee)
	unverified.EmailVerified = false
	if err := accept(unverified, token); code(err) != "email_not_verified" {
		t.Fatalf("unverified email: %v", err)
	}
	stale := acceptor(invitee)
	stale.RecentAuth = false
	if err := accept(stale, token); code(err) != "recent_auth_required" {
		t.Fatalf("old authentication: %v", err)
	}
	if err := accept(acceptor(invitee), token); err != nil {
		t.Fatalf("same email in another case: %v", err)
	}
	if err := accept(acceptor(invitee), token); code(err) != "invitation_not_found" {
		t.Fatalf("accepted twice: %v", err)
	}

	revoked, revokedToken := invite("r@example.test")
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.RevokeInvitation(ctx, tx, sc, owner, revoked.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accept(acceptor(e.person("r@example.test")), revokedToken); code(err) != "invitation_not_found" {
		t.Fatalf("revoked: %v", err)
	}
	expired, expiredToken := invite("x@example.test")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE invitations SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	if err := accept(acceptor(e.person("x@example.test")), expiredToken); code(err) != "invitation_not_found" {
		t.Fatalf("expired: %v", err)
	}
	if err := accept(acceptor(other), "not-a-token"); code(err) != "invitation_not_found" {
		t.Fatalf("unknown token: %v", err)
	}
	// A token never reaches another brand's tenant.
	_, crossToken := invite("cross@example.test")
	otherBrand := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
	if err := accept(acceptor(enginetest.InsertPerson(t, e.db.Pool, otherBrand, "cross@example.test")), crossToken); code(err) != "invitation_not_found" {
		t.Fatalf("cross-brand acceptance: %v", err)
	}
	list, _, err := tenant.ListInvitations(ctx, e.db.Pool, sc, owner, tenant.Page{})
	if err != nil || len(list) != 4 {
		t.Fatalf("list = %d %v", len(list), err)
	}
	statuses := map[string]int{}
	for _, inv := range list {
		statuses[inv.Status]++
	}
	if statuses["accepted"] != 1 || statuses["revoked"] != 1 || statuses["expired"] != 1 || statuses["pending"] != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
}

func TestActivationDecisionsAreIdempotentAndAppendOnly(t *testing.T) {
	e := newEnv(t)
	tn := e.mustOrg(e.person("owner@example.test"), "Activate")
	sc := tn.Scope()
	ctx := context.Background()
	staff := events.Actor{Type: events.ActorStaff, ID: "sub:staff-1"}
	decide := func(d tenant.Decision) (tenant.Activation, bool, error) {
		var a tenant.Activation
		var replayed bool
		err := e.tx(func(tx pgx.Tx) error {
			var err error
			a, replayed, err = tenant.Decide(ctx, tx, sc, d, staff, "test")
			return err
		})
		return a, replayed, err
	}
	d := tenant.Decision{State: tenant.ActivationApproved, Reason: "KYC passed", RequestID: "request-0001"}
	first, replayed, err := decide(d)
	if err != nil || replayed {
		t.Fatalf("first = %+v %v %v", first, replayed, err)
	}
	again, replayed, err := decide(d)
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay = %+v %v %v", again, replayed, err)
	}
	if _, _, err := decide(tenant.Decision{State: tenant.ActivationRejected, Reason: "KYC passed", RequestID: "request-0001"}); code(err) != "idempotency_key_conflict" {
		t.Fatalf("other payload: %v", err)
	}
	if _, _, err := decide(tenant.Decision{State: tenant.ActivationApproved, RequestID: "request-0002"}); code(err) != "reason_required" {
		t.Fatalf("no reason: %v", err)
	}
	state, history, err := tenant.ActivationOf(ctx, e.db.Pool, sc)
	if err != nil || state != tenant.ActivationApproved || len(history) != 1 {
		t.Fatalf("activation = %s %d %v", state, len(history), err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_activations SET reason = 'changed'`); err == nil {
		t.Fatal("tenant_activations accepted an update")
	}
	if _, err := e.db.Pool.Exec(ctx, `DELETE FROM tenant_activations`); err == nil {
		t.Fatal("tenant_activations accepted a delete")
	}
	otherBrand := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
	if err := e.tx(func(tx pgx.Tx) error {
		_, _, err := tenant.Decide(ctx, tx, scope.Scope{Brand: otherBrand.ID, Tenant: tn.ID}, d, staff, "test")
		return err
	}); code(err) != "tenant_not_found" {
		t.Fatalf("decision through another brand: %v", err)
	}
}

// Suspension blocks the Kill Bill account and every write in Core; reads
// continue. Closing closes the Kill Bill account and is terminal.
func TestSuspensionAndClosure(t *testing.T) {
	e := newEnv(t)
	ownerP := e.person("owner@example.test")
	tn := e.mustOrg(ownerP, "Lifecycle")
	sc := tn.Scope()
	ctx := context.Background()
	operator := events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, sc, operator, "", "test"); code(err) != "reason_required" {
		t.Fatalf("suspend without a reason: %v", err)
	}
	e.fake.SetDown(false, true)
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, sc, operator, "overdue", "test"); code(err) != "billing_unavailable" {
		t.Fatalf("suspend with Kill Bill down: %v", err)
	}
	if got, _ := tenant.Get(ctx, e.db.Pool, sc); got.Status != tenant.StatusActive {
		t.Fatalf("status after a failed suspension = %s", got.Status)
	}
	e.fake.SetDown(false, false)
	suspended, err := tenant.Suspend(ctx, e.db.Pool, e.eng, sc, operator, "overdue", "test")
	if err != nil || suspended.Status != tenant.StatusSuspended {
		t.Fatalf("suspend = %+v %v", suspended, err)
	}
	account, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	if len(account.Blocking) != 1 || !account.Blocking[0].IsBlockBilling || !account.Blocking[0].IsBlockEntitlement ||
		account.Blocking[0].Service != tenant.BlockingService {
		t.Fatalf("blocking = %+v", account.Blocking)
	}
	if org, _, _ := e.fake.Organization(e.brand.Realm, tn.PublicID()); org.Enabled {
		t.Fatal("organization still enabled")
	}
	owner := e.caller(sc, ownerP, true)
	name := "Renamed"
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.Update(ctx, tx, sc, owner, tenant.Change{Name: &name})
		return err
	}); code(err) != "tenant_not_found" {
		t.Fatalf("write to a suspended tenant: %v", err)
	}
	if _, err := tenant.Get(ctx, e.db.Pool, sc); err != nil {
		t.Fatalf("read of a suspended tenant: %v", err)
	}
	if _, err := tenant.Reactivate(ctx, e.db.Pool, e.eng, sc, operator, "paid", "test"); err != nil {
		t.Fatal(err)
	}
	account, _ = e.fake.Account(e.brand.Slug, tn.PublicID())
	if len(account.Blocking) != 2 || account.Blocking[1].IsBlockBilling {
		t.Fatalf("blocking after reactivation = %+v", account.Blocking)
	}
	closed, err := tenant.Close(ctx, e.db.Pool, e.eng, sc, operator, "customer left", "test")
	if err != nil || closed.Status != tenant.StatusClosed {
		t.Fatalf("close = %+v %v", closed, err)
	}
	if account, _ = e.fake.Account(e.brand.Slug, tn.PublicID()); !account.Closed {
		t.Fatal("Kill Bill account not closed")
	}
	if _, err := tenant.Reactivate(ctx, e.db.Pool, e.eng, sc, operator, "undo", "test"); code(err) != "tenant_closed" {
		t.Fatalf("reactivate a closed tenant: %v", err)
	}
	if got := actions(t, e.db, sc); !slices.Contains(got, "tenant.closed:success") || !slices.Contains(got, "tenant.suspended:success") {
		t.Fatalf("audit = %v", got)
	}
}

// The sweeper corrects the Keycloak Organization towards Core: a member
// added behind Core's back leaves it, a removed member leaves it, and the
// name follows the tenant.
func TestSweeperCorrectsOrganizationMembers(t *testing.T) {
	e := newEnv(t)
	ownerP, memberP := e.person("owner@example.test"), e.person("member@example.test")
	tn := e.mustOrg(ownerP, "Mirror")
	sc := tn.Scope()
	ctx := context.Background()
	owner := e.caller(sc, ownerP, true)
	m := e.join(sc, owner, memberP, tenant.RoleMember)
	e.fake.AddOrganizationMember(tn.KeycloakOrganizationID, "intruder")
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}
	report, err := sweeper.SweepOnce(ctx)
	if err != nil || report.MembersAdded != 1 || report.MembersRemoved != 1 {
		t.Fatalf("sweep = %+v %v", report, err)
	}
	_, members, _ := e.fake.Organization(e.brand.Realm, tn.PublicID())
	slices.Sort(members)
	want := []string{memberP.Subject, ownerP.Subject}
	slices.Sort(want)
	if strings.Join(members, ",") != strings.Join(want, ",") {
		t.Fatalf("members = %v, want %v", members, want)
	}
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, sc, owner, m.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	name := "Mirror Renamed"
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.Update(ctx, tx, sc, owner, tenant.Change{Name: &name})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if report, err = sweeper.SweepOnce(ctx); err != nil || report.MembersRemoved != 1 || report.OrganizationsUpdated != 1 {
		t.Fatalf("second sweep = %+v %v", report, err)
	}
	org, members, _ := e.fake.Organization(e.brand.Realm, tn.PublicID())
	if len(members) != 1 || members[0] != ownerP.Subject || org.Name != name {
		t.Fatalf("after removal: %+v %v", org, members)
	}
	if report, err = sweeper.SweepOnce(ctx); err != nil || report.MembersAdded+report.MembersRemoved+report.OrganizationsUpdated != 0 {
		t.Fatalf("third sweep changed something: %+v %v", report, err)
	}
	got := actions(t, e.db, sc)
	if !slices.Contains(got, "tenant.keycloak.member.removed:success") || !slices.Contains(got, "tenant.keycloak.organization.updated:success") {
		t.Fatalf("audit = %v", got)
	}
}

// The sweeper reads a page of tenants and then takes each one under its own
// capability. A tenant that was suspended in between must be synced with the
// status it has now, not the status the page showed, or the sweep would keep
// its Keycloak Organization enabled.
func TestSweeperSyncsTheOrganizationWithTheStatusInsideTheCapability(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ownerA, ownerB := e.person("fresh-a@example.test"), e.person("fresh-b@example.test")
	a, b := e.mustOrg(ownerA, "Fresh status A"), e.mustOrg(ownerB, "Fresh status B")
	if a.ID.String() > b.ID.String() {
		a, b, ownerA, ownerB = b, a, ownerB, ownerA
	}
	_ = ownerB
	// A's recipient has drifted, so the sweep writes to Kill Bill while it is
	// at A, after the page that holds B was read.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email = 'fresh-a-updated@example.test' WHERE id = $1`, ownerA.Identity.ID); err != nil {
		t.Fatal(err)
	}
	var suspended atomic.Bool
	e.fake.AfterWrite(func(method, path string) {
		if method == http.MethodPut && path == "/1.0/kb/accounts/"+a.KillBillAccountID && suspended.CompareAndSwap(false, true) {
			if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, b.ID); err != nil {
				t.Error(err)
			}
		}
	})
	t.Cleanup(func() { e.fake.AfterWrite(nil) })
	if _, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !suspended.Load() {
		t.Fatal("the sweep never wrote A's billing account, so B was not changed in the middle of the pass")
	}
	if org, _, _ := e.fake.Organization(e.brand.Realm, b.PublicID()); org.Enabled {
		t.Fatal("the Organization of a tenant suspended during the pass stayed enabled")
	}
	if org, _, _ := e.fake.Organization(e.brand.Realm, a.PublicID()); !org.Enabled {
		t.Fatal("the Organization of the active tenant was disabled")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// When a creation fails and the cleanup after it fails too, the sweep reports
// both, not only the first.
func TestSweeperCreationReportsTheCleanupFailureWithTheCreationFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.fake.SetDown(false, true)
	owner := e.person("cleanup-both@example.test")
	if _, err := e.createOrg(owner, "Cleanup both", "cleanup-both-key"); code(err) != "billing_unavailable" {
		t.Fatalf("creation = %v", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = now() WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	tracer := &sweepUnlockCancelTracer{child: true}
	cfg, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tracer.enabled.Store(true)
	logs := &lockedBuffer{}
	sweeper := &tenant.Sweeper{DB: pool, Engines: e.eng, Log: slog.New(slog.NewTextHandler(logs, nil))}
	_, _ = sweeper.SweepOnce(ctx)
	if tracer.hits.Load() == 0 {
		t.Fatal("the child release was never cancelled")
	}
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "tenant creation is still incomplete") {
			line = l
		}
	}
	if line == "" || !strings.Contains(strings.ToLower(line), "billing") || !strings.Contains(line, "canceled") {
		t.Fatalf("the creation failure was logged as %q, want both the billing failure and the release failure", line)
	}
}
