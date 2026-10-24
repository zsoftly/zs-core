//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

type queryCounter struct{ starts atomic.Int64 }

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	q.starts.Add(1)
	return ctx
}

func (*queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func lifecycleScope() scope.Scope {
	return scope.Scope{Brand: idgen.MustNewV7(), Tenant: idgen.MustNewV7()}
}
func TestLifecycleGuardSameTenantIsBusyUntilVerifiedRelease(t *testing.T) {
	db := postgrestest.New(t)
	sc := lifecycleScope()
	first, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc)
	if err != nil || !got {
		t.Fatalf("first guard = %v %v", got, err)
	}
	if second, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc); err != nil || got || second != nil {
		t.Fatalf("busy guard = %v %v %v", second, got, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc)
	if err != nil || !got {
		t.Fatalf("released guard = %v %v", got, err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleSweepUsesOneSessionForSequentialTenantChildren(t *testing.T) {
	db := postgrestest.New(t)
	parent, got, err := tenant.AcquireLifecycleSweep(context.Background(), db.Pool)
	if err != nil || !got {
		t.Fatalf("sweep guard = %v %v", got, err)
	}
	defer parent.Release()
	var parentPID uint32
	if err := parent.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&parentPID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		child, got, err := parent.TryTenant(context.Background(), lifecycleScope())
		if err != nil || !got {
			t.Fatalf("child guard = %v %v", got, err)
		}
		if err := parent.QueryRow(context.Background(), `SELECT 1`).Scan(new(int)); err == nil {
			t.Fatal("parent query succeeded while child was live")
		}
		var childPID uint32
		if err := child.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&childPID); err != nil || childPID != parentPID {
			t.Fatalf("child session = %d %d %v", childPID, parentPID, err)
		}
		tx, err := child.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(context.Background(), `SELECT 1`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := child.Release(); err != nil {
			t.Fatal(err)
		}
		if err := child.QueryRow(context.Background(), `SELECT 1`).Scan(new(int)); err == nil {
			t.Fatal("released child query succeeded")
		}
	}
	if contender, got, err := tenant.AcquireLifecycleSweep(context.Background(), db.Pool); err != nil || got || contender != nil {
		t.Fatalf("global contender = %v %v %v", contender, got, err)
	}
}

func TestLifecycleGuardUsesConfiguredSingleConnectionPool(t *testing.T) {
	db := postgrestest.New(t)
	config := db.Pool.Config().Copy()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), pool, lifecycleScope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	defer guard.Release()
	var pid uint32
	if err := guard.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil || pid == 0 {
		t.Fatalf("guard pid = %d %v", pid, err)
	}
	tx, err := guard.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithCancel(context.Background())
	cancel()
	if second, got, err := tenant.AcquireLifecycleGuard(blocked, pool, lifecycleScope()); err == nil || got || second != nil {
		t.Fatalf("cancelled acquire = %v %v %v", second, got, err)
	}
}

func TestLifecycleGuardDeferredReleaseClosesUnreadRowsAndRollsBack(t *testing.T) {
	db := postgrestest.New(t)
	sc := lifecycleScope()
	func() {
		guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc)
		if err != nil || !got {
			t.Fatalf("guard = %v %v", got, err)
		}
		defer func() {
			if err := guard.Release(); err != nil {
				t.Fatal(err)
			}
			if recover() == nil {
				t.Fatal("panic was not preserved")
			}
		}()
		if _, err := guard.Query(context.Background(), `SELECT 1`); err != nil {
			t.Fatal(err)
		}
		if _, err := guard.Begin(context.Background()); err == nil {
			t.Fatal("transaction began with unread rows")
		}
		panic("release lifecycle guard")
	}()
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc)
	if err != nil || !got {
		t.Fatalf("guard after deferred cleanup = %v %v", got, err)
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleGuardStaleQueryRowRefusesAfterRelease(t *testing.T) {
	db := postgrestest.New(t)
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, lifecycleScope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	row := guard.QueryRow(context.Background(), `SELECT 1`)
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	if err := row.Scan(new(int)); !errors.Is(err, tenant.ErrLifecycleReleased) {
		t.Fatalf("stale row error = %v", err)
	}
}

func TestLifecycleGuardRowsTypeMap(t *testing.T) {
	db := postgrestest.New(t)
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, lifecycleScope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	rows, err := guard.Query(context.Background(), `SELECT 1::int4`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rows.Close)
	typeMap := rows.TypeMap()
	if typeMap == nil {
		t.Fatal("live rows have no type map")
	}
	if _, ok := typeMap.TypeForName("int4"); !ok {
		t.Fatal("live rows type map cannot resolve int4")
	}
	if !rows.Next() {
		t.Fatalf("live rows next = false: %v", rows.Err())
	}
	var value int32
	if err := rows.Scan(&value); err != nil || value != 1 {
		t.Fatalf("live rows scan = %d %v", value, err)
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	if rows.TypeMap() != nil {
		t.Fatal("stale rows retained a type map")
	}
}

func TestLifecycleGuardBeginFuncPreservesTransactionSemantics(t *testing.T) {
	db := postgrestest.New(t)
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, lifecycleScope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	if err := pgx.BeginFunc(context.Background(), guard, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `SELECT 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("callback failure")
	if err := pgx.BeginFunc(context.Background(), guard, func(pgx.Tx) error { return want }); !errors.Is(err, want) {
		t.Fatalf("callback error = %v", err)
	}
	tx, err := guard.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	})
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(context.Background()); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("second rollback = %v", err)
	}
}

func TestLifecycleGuardRejectsTxOperationsWithOpenRows(t *testing.T) {
	for _, operation := range []string{"exec", "prepare", "commit"} {
		t.Run(operation, func(t *testing.T) {
			db := postgrestest.New(t)
			guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, lifecycleScope())
			if err != nil || !got {
				t.Fatalf("guard = %v %v", got, err)
			}
			t.Cleanup(func() {
				if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
					t.Error(err)
				}
			})
			tx, err := guard.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) && !errors.Is(err, pgx.ErrTxClosed) {
					t.Error(err)
				}
			})
			rows, err := tx.Query(context.Background(), `SELECT 1`)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(rows.Close)
			switch operation {
			case "exec":
				_, err = tx.Exec(context.Background(), `SELECT 2`)
			case "prepare":
				_, err = tx.Prepare(context.Background(), "guard-open-rows", `SELECT 2`)
			case "commit":
				err = tx.Commit(context.Background())
			}
			if err == nil || !strings.Contains(err.Error(), "tenant lifecycle operation already active") {
				t.Fatalf("%s with open rows = %v", operation, err)
			}
			if !rows.Next() {
				t.Fatalf("rows next = false: %v", rows.Err())
			}
			var value int
			if err := rows.Scan(&value); err != nil || value != 1 {
				t.Fatalf("rows scan = %d %v", value, err)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatalf("rows error after close = %v", err)
			}
			if _, err := tx.Exec(context.Background(), `SELECT 2`); err != nil {
				t.Fatalf("exec after rows close = %v", err)
			}
			if err := tx.Rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLifecycleGuardCancelsBlockedQueryAndInvalidatesCapability(t *testing.T) {
	db := postgrestest.New(t)
	lockKey := int64(81234567)
	locker, err := db.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { locker.Release() })
	if _, err := locker.Exec(context.Background(), `SELECT pg_advisory_lock($1::bigint)`, lockKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = locker.Exec(context.Background(), `SELECT pg_advisory_unlock($1::bigint)`, lockKey) })
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, lifecycleScope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	var pid uint32
	if err := guard.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	queryCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queryDone := make(chan error, 1)
	go func() {
		rows, err := guard.Query(queryCtx, `SELECT pg_advisory_lock($1::bigint)`, lockKey)
		if rows != nil {
			rows.Close()
			if err == nil {
				err = rows.Err()
			}
		}
		queryDone <- err
	}()
	observer, err := db.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Release() })
	waited := false
	for range 1000 {
		if err := observer.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, pid).Scan(&waited); err != nil {
			t.Fatal(err)
		}
		if waited {
			break
		}
	}
	if !waited {
		t.Fatal("guard query did not enter an advisory-lock wait")
	}
	cancel()
	if err := <-queryDone; err == nil {
		t.Fatal("cancelled blocked query succeeded")
	}
	if _, err := guard.Query(context.Background(), `SELECT 1`); !errors.Is(err, tenant.ErrLifecycleReleased) {
		t.Fatalf("query after cancelled result = %v", err)
	}
}

func TestLifecycleGuardSameTenantContentionReleasesForReacquire(t *testing.T) {
	db := postgrestest.New(t)
	sc := lifecycleScope()
	type result struct {
		guard *tenant.LifecycleGuard
		got   bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc)
			results <- result{guard: guard, got: got, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	for _, result := range []result{first, second} {
		if result.guard != nil {
			guard := result.guard
			t.Cleanup(func() {
				if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
					t.Error(err)
				}
			})
		}
	}
	if first.err != nil || second.err != nil || first.got == second.got {
		t.Fatalf("contention = %+v %+v", first, second)
	}
	winner := first
	if !winner.got {
		winner = second
	}
	if err := winner.guard.Release(); err != nil {
		t.Fatal(err)
	}
	again, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc)
	if again != nil {
		t.Cleanup(func() {
			if err := again.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
				t.Error(err)
			}
		})
	}
	if err != nil || !got {
		t.Fatalf("reacquire = %v %v", got, err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleGuardRejectsWrongScopeBeforeSQL(t *testing.T) {
	db := postgrestest.New(t)
	tracer := &queryCounter{}
	config := db.Pool.Config().Copy()
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sc := lifecycleScope()
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), pool, sc)
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	for _, wrong := range []scope.Scope{
		{Brand: sc.Brand, Tenant: idgen.MustNewV7()},
		{Brand: idgen.MustNewV7(), Tenant: sc.Tenant},
	} {
		tracer.starts.Store(0)
		if _, err := tenant.Get(context.Background(), guard, wrong); !errors.Is(err, tenant.ErrWrongScope) {
			t.Fatalf("wrong scope %v error = %v", wrong, err)
		}
		if got := tracer.starts.Load(); got != 0 {
			t.Fatalf("wrong scope %v issued %d queries", wrong, got)
		}
	}
	tracer.starts.Store(0)
	if _, err := tenant.Get(context.Background(), guard, sc); err == nil {
		t.Fatal("same scope unexpectedly had a tenant row")
	}
	if got := tracer.starts.Load(); got == 0 {
		t.Fatal("same scope issued no SQL")
	}
}

func TestLifecycleGuardInvalidatesAfterDriverErrors(t *testing.T) {
	for _, operation := range []string{"begin", "exec", "query", "commit"} {
		t.Run(operation, func(t *testing.T) {
			db := postgrestest.New(t)
			guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, lifecycleScope())
			if err != nil || !got {
				t.Fatalf("guard = %v %v", got, err)
			}
			t.Cleanup(func() {
				if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
					t.Error(err)
				}
			})
			switch operation {
			case "begin":
				cancelled, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := guard.Begin(cancelled); err == nil {
					t.Fatal("cancelled begin succeeded")
				}
			case "exec":
				tx, err := guard.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) && !errors.Is(err, pgx.ErrTxClosed) {
						t.Error(err)
					}
				})
				if _, err := tx.Exec(context.Background(), `SELECT FROM`); err == nil {
					t.Fatal("invalid exec succeeded")
				}
			case "query":
				tx, err := guard.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) && !errors.Is(err, pgx.ErrTxClosed) {
						t.Error(err)
					}
				})
				cancelled, cancel := context.WithCancel(context.Background())
				cancel()
				if rows, err := tx.Query(cancelled, `SELECT 1`); err == nil {
					rows.Close()
					t.Fatal("cancelled query succeeded")
				}
			case "commit":
				setup, err := guard.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := setup.Exec(context.Background(), `CREATE TEMP TABLE lifecycle_guard_driver_commit (id integer UNIQUE DEFERRABLE INITIALLY DEFERRED)`); err != nil {
					t.Fatal(err)
				}
				if err := setup.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
				tx, err := guard.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) && !errors.Is(err, pgx.ErrTxClosed) {
						t.Error(err)
					}
				})
				if _, err := tx.Exec(context.Background(), `INSERT INTO lifecycle_guard_driver_commit (id) VALUES (1), (1)`); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(context.Background()); err == nil {
					t.Fatal("deferred commit succeeded")
				}
			}
			if _, err := guard.Query(context.Background(), `SELECT 1`); !errors.Is(err, tenant.ErrLifecycleReleased) {
				t.Fatalf("query after %s error = %v", operation, err)
			}
		})
	}
}

func TestLifecycleSweepTryTenantSerializesSameParentChildren(t *testing.T) {
	db := postgrestest.New(t)
	parent, got, err := tenant.AcquireLifecycleSweep(context.Background(), db.Pool)
	if err != nil || !got {
		t.Fatalf("parent = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := parent.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	sc := lifecycleScope()
	type result struct {
		child *tenant.LifecycleGuard
		got   bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			child, got, err := parent.TryTenant(context.Background(), sc)
			results <- result{child: child, got: got, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	for _, result := range []result{first, second} {
		if result.child != nil {
			child := result.child
			t.Cleanup(func() {
				if err := child.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
					t.Error(err)
				}
			})
		}
	}
	if first.err != nil || second.err != nil || first.got == second.got {
		t.Fatalf("children = %+v %+v", first, second)
	}
	winner := first
	if !winner.got {
		winner = second
	}
	if contender, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc); err != nil || got || contender != nil {
		t.Fatalf("independent contender = %v %v %v", contender, got, err)
	}
	tx, err := winner.child.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := winner.child.Release(); err != nil {
		t.Fatal(err)
	}
	again, got, err := parent.TryTenant(context.Background(), sc)
	if err != nil || !got {
		t.Fatalf("child reacquire = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := again.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	if independent, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, sc); err != nil || got || independent != nil {
		t.Fatalf("independent child contender = %v %v %v", independent, got, err)
	}
}

func TestLifecycleSweepSecondChildDefersWhileFirstRowsOpen(t *testing.T) {
	db := postgrestest.New(t)
	parent, got, err := tenant.AcquireLifecycleSweep(context.Background(), db.Pool)
	if err != nil || !got {
		t.Fatalf("parent = %v %v", got, err)
	}
	t.Cleanup(func() { _ = parent.Release() })
	child, got, err := parent.TryTenant(context.Background(), lifecycleScope())
	if err != nil || !got {
		t.Fatalf("first child = %v %v", got, err)
	}
	t.Cleanup(func() { _ = child.Release() })
	tx, err := child.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	rows, err := tx.Query(context.Background(), `SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rows.Close)
	if second, got, err := parent.TryTenant(context.Background(), lifecycleScope()); err != nil || got || second != nil {
		t.Fatalf("second child = %v %v %v", second, got, err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second, got, err := parent.TryTenant(context.Background(), lifecycleScope()); err != nil || got || second != nil {
		t.Fatalf("second child after completion = %v %v %v", second, got, err)
	}
	if err := child.Release(); err != nil {
		t.Fatal(err)
	}
	again, got, err := parent.TryTenant(context.Background(), lifecycleScope())
	if err != nil || !got {
		t.Fatalf("child reacquire after release = %v %v", got, err)
	}
	t.Cleanup(func() { _ = again.Release() })
}

func TestLifecycleSweepSecondChildDefersDuringBlockedChildQuery(t *testing.T) {
	db := postgrestest.New(t)
	lockKey := int64(81234568)
	blocker, err := db.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blocker.Release() })
	if _, err := blocker.Exec(context.Background(), `SELECT pg_advisory_lock($1::bigint)`, lockKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = blocker.Exec(context.Background(), `SELECT pg_advisory_unlock($1::bigint)`, lockKey) })
	parent, got, err := tenant.AcquireLifecycleSweep(context.Background(), db.Pool)
	if err != nil || !got {
		t.Fatalf("parent = %v %v", got, err)
	}
	t.Cleanup(func() { _ = parent.Release() })
	child, got, err := parent.TryTenant(context.Background(), lifecycleScope())
	if err != nil || !got {
		t.Fatalf("child = %v %v", got, err)
	}
	t.Cleanup(func() { _ = child.Release() })
	var pid uint32
	if err := child.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	queryCtx, cancelQuery := context.WithTimeout(context.Background(), 10*time.Second)
	queryDone := make(chan error, 1)
	queryFinished := make(chan struct{})
	secondFinished := make(chan struct{})
	secondStarted := false
	defer func() {
		cancelQuery()
		_, _ = blocker.Exec(context.Background(), `SELECT pg_advisory_unlock($1::bigint)`, lockKey)
		<-queryFinished
		if secondStarted {
			<-secondFinished
		}
	}()
	go func() {
		rows, err := child.Query(queryCtx, `SELECT pg_advisory_xact_lock($1::bigint)`, lockKey)
		if rows != nil {
			rows.Close()
		}
		queryDone <- err
		close(queryFinished)
	}()
	observer, err := db.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Release() })
	waited := false
	for range 1000 {
		if err := observer.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, pid).Scan(&waited); err != nil {
			t.Fatal(err)
		}
		if waited {
			break
		}
	}
	if !waited {
		t.Fatal("child query did not block")
	}
	result := make(chan struct {
		child *tenant.LifecycleGuard
		got   bool
		err   error
	}, 1)
	secondStarted = true
	go func() {
		defer close(secondFinished)
		second, got, err := parent.TryTenant(context.Background(), lifecycleScope())
		result <- struct {
			child *tenant.LifecycleGuard
			got   bool
			err   error
		}{second, got, err}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case second := <-result:
		if second.err != nil || second.got || second.child != nil {
			t.Fatalf("second child = %+v", second)
		}
	case <-ctx.Done():
		t.Fatal("second child blocked behind child query")
	}
	if _, err := blocker.Exec(context.Background(), `SELECT pg_advisory_unlock($1::bigint)`, lockKey); err != nil {
		t.Fatal(err)
	}
	if err := <-queryDone; err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleSweepChildReleaseClosesOpenRows(t *testing.T) {
	db := postgrestest.New(t)
	parent, got, err := tenant.AcquireLifecycleSweep(context.Background(), db.Pool)
	if err != nil || !got {
		t.Fatalf("parent = %v %v", got, err)
	}
	t.Cleanup(func() { _ = parent.Release() })
	sc := lifecycleScope()
	child, got, err := parent.TryTenant(context.Background(), sc)
	if err != nil || !got {
		t.Fatalf("child = %v %v", got, err)
	}
	t.Cleanup(func() { _ = child.Release() })
	rows, err := child.Query(context.Background(), `SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rows.Close)
	if err := child.Release(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); !errors.Is(err, tenant.ErrLifecycleReleased) {
		t.Fatalf("rows after release = %v", err)
	}
	again, got, err := parent.TryTenant(context.Background(), sc)
	if err != nil || !got {
		t.Fatalf("child after release = %v %v", got, err)
	}
	t.Cleanup(func() { _ = again.Release() })
}

func TestLifecycleSweepConcurrentChildLifetimes(t *testing.T) {
	db := postgrestest.New(t)
	parent, got, err := tenant.AcquireLifecycleSweep(context.Background(), db.Pool)
	if err != nil || !got {
		t.Fatalf("parent = %v %v", got, err)
	}
	t.Cleanup(func() { _ = parent.Release() })
	start := make(chan struct{})
	type result struct {
		completed int
		err       error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			completed := 0
			for completed < 10 {
				child, got, err := parent.TryTenant(ctx, lifecycleScope())
				if err != nil {
					results <- result{completed: completed, err: err}
					return
				}
				if !got {
					if err := ctx.Err(); err != nil {
						results <- result{completed: completed, err: err}
						return
					}
					continue
				}
				tx, err := child.Begin(context.Background())
				if err == nil {
					var rows pgx.Rows
					rows, err = tx.Query(context.Background(), `SELECT 1`)
					if err == nil {
						for rows.Next() {
							var value int
							err = rows.Scan(&value)
						}
						rows.Close()
						if err == nil {
							err = rows.Err()
						}
					}
					if err == nil {
						err = tx.Commit(context.Background())
					}
				}
				if releaseErr := child.Release(); err == nil {
					err = releaseErr
				}
				if err != nil {
					results <- result{completed: completed, err: err}
					return
				}
				completed++
			}
			results <- result{completed: completed}
		}()
	}
	close(start)
	first, second := <-results, <-results
	for _, result := range []result{first, second} {
		if result.err != nil || result.completed != 10 {
			t.Fatalf("completed = %d %v", result.completed, result.err)
		}
	}
}

func TestLifecycleGuardInvalidatesAfterDeferredCommitFailure(t *testing.T) {
	db := postgrestest.New(t)
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), db.Pool, lifecycleScope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) {
			t.Error(err)
		}
	})
	tx, err := guard.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, tenant.ErrLifecycleReleased) && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	})
	if _, err := tx.Exec(context.Background(), `CREATE TEMP TABLE lifecycle_guard_deferred_unique (id integer UNIQUE DEFERRABLE INITIALLY DEFERRED)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	tx, err = guard.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO lifecycle_guard_deferred_unique (id) VALUES (1), (1)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err == nil {
		t.Fatal("deferred unique violation committed")
	}
	if _, err := guard.Query(context.Background(), `SELECT 1`); !errors.Is(err, tenant.ErrLifecycleReleased) {
		t.Fatalf("query after failed commit = %v", err)
	}
}
