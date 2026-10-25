//go:build integration

package brand

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/internal/secrets"
)

// sharedApplyDB counts planning reads separately from the transaction that
// acquires the locks. Its second transaction is the first planned mutation.
type sharedApplyDB struct {
	DB
	queries           atomic.Int32
	begins            atomic.Int32
	beforeFirstCommit func(context.Context) error
}

func (db *sharedApplyDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.queries.Add(1)
	return db.DB.Query(ctx, sql, args...)
}

func (db *sharedApplyDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.queries.Add(1)
	return db.DB.QueryRow(ctx, sql, args...)
}

func (db *sharedApplyDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if db.begins.Add(1) == 2 && db.beforeFirstCommit != nil {
		return &sharedApplyCommitTx{Tx: tx, beforeCommit: db.beforeFirstCommit}, nil
	}
	return tx, nil
}

type sharedApplyCommitTx struct {
	pgx.Tx
	beforeCommit func(context.Context) error
}

func (tx *sharedApplyCommitTx) Commit(ctx context.Context) error {
	if err := tx.beforeCommit(ctx); err != nil {
		return err
	}
	return tx.Tx.Commit(ctx)
}

// The different schemas and slugs prevent the existing per-brand lock from
// accidentally protecting the shared plugin state under test. Channels prove
// overlap; elapsed time is only a deadline, never the overlap assertion.
func TestApplySerializesSharedKillBillAcrossSchemasAndBrands(t *testing.T) {
	for _, phase := range []string{"planning", "execution"} {
		t.Run(phase, func(t *testing.T) {
			// The lock under test is shared by the schemas of one database, so the
			// two fixtures must be two schemas of the same database.
			dbA, dbB := postgrestest.NewSchema(t), postgrestest.NewSchema(t)
			if dbA.Schema == dbB.Schema {
				t.Fatal("fixture must use distinct schemas")
			}
			manifest := func(slug string) *Manifest {
				m := brandManifest(t, slug, "env://"+secrets.EnvPrefix(slug)+"STAFF")
				m.Billing.Overdue = ""
				m.Billing.InvoiceTemplates = nil
				m.Billing.Email = nil
				m.Billing.Meters = nil
				m.Entitlements = nil
				if err := m.Validate(); err != nil {
					t.Fatal(err)
				}
				return m
			}
			a, b := manifest("shared-alpha"), manifest("shared-beta")
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			pause := func(ctx context.Context) error {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			var engineAReads, engineAWrites, engineBRequests, unexpectedA atomic.Int32
			serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/realms/master/protocol/openid-connect/token" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"test-token","expires_in":300}`))
					return
				}
				if r.Method == http.MethodGet {
					engineAReads.Add(1)
					if r.URL.Path == "/admin/realms/"+a.Identity.Realm {
						if phase == "planning" {
							_ = pause(r.Context())
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						http.NotFound(w, r)
						return
					}
					if r.URL.Path == "/1.0/kb/tenants" && r.URL.Query().Get("apiKey") == a.Billing.KillBillTenantKey {
						http.NotFound(w, r)
						return
					}
				} else {
					engineAWrites.Add(1)
				}
				unexpectedA.Add(1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			t.Cleanup(serverA.Close)
			serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				engineBRequests.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(serverB.Close)
			makeDeps := func(db DB, url string) Deps {
				kc, err := keycloak.New(keycloak.Config{URL: url, AdminUser: "admin", AdminPassword: "unused"})
				if err != nil {
					t.Fatal(err)
				}
				kb, err := killbill.New(killbill.Config{URL: url, AdminUser: "admin", AdminPassword: "unused"})
				if err != nil {
					t.Fatal(err)
				}
				return Deps{DB: db, Keycloak: kc, KillBill: kb, Secrets: secrets.Default{LookupEnv: func(string) (string, bool) { return "synthetic-secret", true }}}
			}
			wrappedA, wrappedB := &sharedApplyDB{DB: dbA.Pool}, &sharedApplyDB{DB: dbB.Pool}
			commitFailure := errors.New("injected first mutation commit failure")
			if phase == "execution" {
				wrappedA.beforeFirstCommit = func(ctx context.Context) error {
					if err := pause(ctx); err != nil {
						return err
					}
					return commitFailure
				}
			}
			depsA, depsB := makeDeps(wrappedA, serverA.URL), makeDeps(wrappedB, serverB.URL)
			ctxA, cancelA := context.WithTimeout(context.Background(), 20*time.Second)
			finished := make(chan struct{})
			var applyErr error
			go func() {
				defer close(finished)
				_, applyErr = Apply(ctxA, depsA, a, integrationActor)
			}()
			t.Cleanup(func() {
				unblock()
				cancelA()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("first Apply did not terminate before schema cleanup")
				}
			})
			select {
			case <-entered:
			case <-finished:
				t.Fatalf("first Apply ended before %s barrier: %v", phase, applyErr)
			case <-ctxA.Done():
				t.Fatalf("first Apply did not reach %s barrier", phase)
			}

			// Probe the exact fixed database-wide key from the other schema.
			// Including current_schema() or either brand in this key is a defect.
			probe, err := dbB.Pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var free bool
			probeErr := probe.QueryRow(context.Background(), `SELECT pg_try_advisory_xact_lock(hashtextextended('zs-core.brand.apply.shared-killbill', 0))`).Scan(&free)
			rollbackErr := probe.Rollback(context.Background())
			if probeErr != nil || rollbackErr != nil || free {
				t.Fatalf("fixed shared lock not held at %s: free=%v query=%v rollback=%v", phase, free, probeErr, rollbackErr)
			}
			ctxB, cancelB := context.WithTimeout(context.Background(), 2*time.Second)
			_, err = Apply(ctxB, depsB, b, integrationActor)
			cancelB()
			if codeOf(err) != "brand_apply_in_progress" {
				t.Fatalf("different brand/schema must be busy during %s: %v", phase, err)
			}
			if wrappedB.queries.Load() != 0 || engineBRequests.Load() != 0 {
				t.Fatalf("competing Apply reached planning: queries=%d engine requests=%d", wrappedB.queries.Load(), engineBRequests.Load())
			}
			if engineAReads.Load() == 0 || unexpectedA.Load() != 0 || engineAWrites.Load() != 0 {
				t.Fatalf("unexpected first Apply fixture activity: reads=%d writes=%d unexpected=%d", engineAReads.Load(), engineAWrites.Load(), unexpectedA.Load())
			}
			unblock()
			select {
			case <-finished:
			case <-ctxA.Done():
				t.Fatal("first Apply did not return after barrier release")
			}
			wantCode := "identity_provider_unavailable"
			if phase == "execution" {
				wantCode = "database_unavailable"
				if !errors.Is(applyErr, commitFailure) {
					t.Fatalf("first Apply did not preserve commit failure: %v", applyErr)
				}
			}
			if codeOf(applyErr) != wantCode {
				t.Fatalf("first Apply error = %v, want %s", applyErr, wantCode)
			}
			var brands int
			if err := dbA.Pool.QueryRow(context.Background(), `SELECT count(*) FROM brands`).Scan(&brands); err != nil || brands != 0 {
				t.Fatalf("failed first Apply retained brand mutation: count=%d err=%v", brands, err)
			}
			// Failure must release the shared lock. The retry now reaches the
			// deliberate engine failure, rather than another busy refusal.
			ctxRetry, cancelRetry := context.WithTimeout(context.Background(), 2*time.Second)
			_, err = Apply(ctxRetry, depsB, b, integrationActor)
			cancelRetry()
			if codeOf(err) != "identity_provider_unavailable" || wrappedB.queries.Load() == 0 || engineBRequests.Load() == 0 {
				t.Fatalf("retry did not reach planning after lock release: err=%v queries=%d engine requests=%d", err, wrappedB.queries.Load(), engineBRequests.Load())
			}
		})
	}
}
