//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

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
