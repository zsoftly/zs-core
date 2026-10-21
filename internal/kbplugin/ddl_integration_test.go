//go:build integration

package kbplugin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/postgrestest"
)

func TestApplyCreatesMissingTablesOnceAndRecordsTheDDL(t *testing.T) {
	ctx := context.Background()
	db := postgrestest.New(t)
	jar := jarWith(t, map[string]string{"ddl.sql": testDDL, "ddl-postgresql.sql": testPrelude})
	pin := pinOf("demo", jar, "demo_responses", "demo_methods")
	d, err := ExtractDDL(jar, pin)
	if err != nil {
		t.Fatal(err)
	}
	if missing, err := MissingTables(ctx, db.Pool, pin); err != nil || strings.Join(missing, ",") != "demo_responses,demo_methods" {
		t.Fatalf("missing before = %v %v", missing, err)
	}
	if _, err := Apply(ctx, db.Pool, db.Pool, d, pin, ApplyOptions{}); err == nil {
		t.Fatal("an apply without an operator is accepted")
	}
	opts := ApplyOptions{AppliedBy: "zs-core-cli:test"}
	res, err := Apply(ctx, db.Pool, db.Pool, d, pin, opts)
	if err != nil || strings.Join(res.Created, ",") != "demo_responses,demo_methods" || len(res.Domains) != 2 {
		t.Fatalf("first apply = %+v %v", res, err)
	}
	if missing, err := MissingTables(ctx, db.Pool, pin); err != nil || len(missing) != 0 {
		t.Fatalf("missing after = %v %v", missing, err)
	}
	// Rows survive a second apply: the DROP of the DDL never runs for a
	// table that exists.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO demo_responses (kb_account_id, created_date) VALUES ('a', now())`); err != nil {
		t.Fatal(err)
	}
	res, err = Apply(ctx, db.Pool, db.Pool, d, pin, opts)
	if err != nil || res.Applied() || strings.Join(res.Present, ",") != "demo_responses,demo_methods" {
		t.Fatalf("second apply = %+v %v", res, err)
	}
	var rows int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM demo_responses`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows after the second apply = %d %v", rows, err)
	}
	// A dropped table is created again, alone.
	if _, err := db.Pool.Exec(ctx, `DROP TABLE demo_methods`); err != nil {
		t.Fatal(err)
	}
	res, err = Apply(ctx, db.Pool, db.Pool, d, pin, opts)
	if err != nil || strings.Join(res.Created, ",") != "demo_methods" || len(res.Domains) != 0 {
		t.Fatalf("apply after a drop = %+v %v", res, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM demo_responses`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows after the repair = %d %v", rows, err)
	}
	var recorded int
	var created []string
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) OVER (), created_tables FROM killbill_plugin_ddl WHERE plugin = 'demo' AND version = '1.2.3' AND ddl_sha256 = $1`, d.Digest).Scan(&recorded, &created); err != nil ||
		recorded != 1 || strings.Join(created, ",") != "demo_methods" {
		t.Fatalf("records = %d %v %v", recorded, created, err)
	}

	// Another DDL for the same version is refused without force.
	changed := jarWith(t, map[string]string{"ddl.sql": testDDL + "\ncreate index demo_methods_extra on demo_methods(additional_data);\n", "ddl-postgresql.sql": testPrelude})
	d2, err := ExtractDDL(changed, pin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, db.Pool, db.Pool, d2, pin, opts); !errors.Is(err, ErrDDLChanged) {
		t.Fatalf("changed DDL: %v", err)
	}
	opts.Force = true
	if res, err := Apply(ctx, db.Pool, db.Pool, d2, pin, opts); err != nil || res.Applied() || !res.Forced {
		t.Fatalf("forced apply = %+v %v", res, err)
	}
	recs, err := Recorded(ctx, db.Pool)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recorded = %+v %v", recs, err)
	}

	// A DDL that creates other tables than components.yaml lists is refused
	// before anything runs.
	other := pin
	other.Tables = []string{"demo_responses"}
	if _, err := Apply(ctx, db.Pool, db.Pool, d, other, ApplyOptions{AppliedBy: "x"}); err == nil {
		t.Fatal("a DDL with other tables is accepted")
	}
}

// delayedBeginDB lets another deployment finish after this caller starts
// applying, but before it opens its Kill Bill transaction.
type delayedBeginDB struct {
	DB
	reached chan struct{}
	proceed <-chan struct{}
}

func (d delayedBeginDB) Begin(ctx context.Context) (pgx.Tx, error) {
	close(d.reached)
	select {
	case <-d.proceed:
		return d.DB.Begin(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConcurrentApplyPreservesCommittedRowsAndSharedDomains(t *testing.T) {
	for _, samePlugin := range []bool{true, false} {
		name := "different plugins share domains"
		if samePlugin {
			name = "same plugin preserves rows"
		}
		t.Run(name, func(t *testing.T) {
			kb := postgrestest.New(t)
			coreA, coreB := postgrestest.New(t), postgrestest.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if samePlugin {
				// Reach the destructive stale DROP path even without locking:
				// duplicate domain creation must not fail first and hide row loss.
				if _, err := kb.Pool.Exec(ctx, testPrelude); err != nil {
					t.Fatal(err)
				}
			}
			jarA := jarWith(t, map[string]string{"ddl.sql": testDDL, "ddl-postgresql.sql": testPrelude})
			pinA := pinOf("demo", jarA, "demo_responses", "demo_methods")
			ddlA, err := ExtractDDL(jarA, pinA)
			if err != nil {
				t.Fatal(err)
			}
			pinB, ddlB := pinA, ddlA
			if !samePlugin {
				jarB := jarWith(t, map[string]string{"ddl.sql": "CREATE TABLE second_responses (id integer, created_date datetime);", "ddl-postgresql.sql": testPrelude})
				pinB = pinOf("second", jarB, "second_responses")
				ddlB, err = ExtractDDL(jarB, pinB)
				if err != nil {
					t.Fatal(err)
				}
			}
			reached, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(proceed) }) }
			type outcome struct {
				result Result
				err    error
			}
			answer := make(chan outcome, 1)
			go func() {
				defer close(done)
				result, err := Apply(ctx, delayedBeginDB{DB: kb.Pool, reached: reached, proceed: proceed}, coreB.Pool, ddlB, pinB, ApplyOptions{AppliedBy: "deployment-b"})
				answer <- outcome{result, err}
			}()
			defer func() {
				cancel()
				release()
				<-done
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("second deployment did not reach its Kill Bill transaction")
			}
			// Separate Core records model two deployments targeting one
			// Kill Bill schema. Both must inspect its current committed state.
			first, err := Apply(ctx, kb.Pool, coreA.Pool, ddlA, pinA, ApplyOptions{AppliedBy: "deployment-a"})
			wantDomains := 2
			if samePlugin {
				wantDomains = 0
			}
			if err != nil || len(first.Created) != 2 || len(first.Domains) != wantDomains {
				t.Fatalf("first deployment = %+v, %v", first, err)
			}
			if _, err := kb.Pool.Exec(ctx, "INSERT INTO demo_responses (kb_account_id, created_date) VALUES ('preserved', now())"); err != nil {
				t.Fatal(err)
			}
			release()
			second := <-answer
			if second.err != nil || len(second.result.Domains) != 0 {
				t.Fatalf("second deployment = %+v, %v", second.result, second.err)
			}
			if samePlugin && (second.result.Applied() || len(second.result.Present) != 2) {
				t.Fatalf("second deployment recreated existing tables: %+v", second.result)
			}
			if !samePlugin && strings.Join(second.result.Created, ",") != "second_responses" {
				t.Fatalf("second plugin tables = %+v", second.result)
			}
			var rows int
			if err := kb.Pool.QueryRow(ctx, "SELECT count(*) FROM demo_responses WHERE kb_account_id = 'preserved'").Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("previously committed rows = %d, %v", rows, err)
			}
			for _, core := range []*postgrestest.DB{coreA, coreB} {
				records, err := Recorded(ctx, core.Pool)
				if err != nil || len(records) != 1 {
					t.Fatalf("deployment records = %+v, %v", records, err)
				}
			}
		})
	}
}

// observedDDLDB wraps real transactions so the test controls overlap without
// replacing PostgreSQL locking or object-existence queries with mock answers.
type observedDDLDB struct {
	DB
	wrap    func(pgx.Tx) pgx.Tx
	onQuery func()
}

func (d observedDDLDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return d.wrap(tx), nil
}

func (d observedDDLDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if d.onQuery != nil {
		d.onQuery()
	}
	return d.DB.Query(ctx, sql, args...)
}

type observedDDLTx struct {
	pgx.Tx
	onQuery      func()
	beforeExec   func(context.Context, string) error
	beforeCommit func(context.Context, pgx.Tx) error
}

func (tx observedDDLTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx.onQuery != nil {
		tx.onQuery()
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx observedDDLTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.beforeExec != nil {
		if err := tx.beforeExec(ctx, sql); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx observedDDLTx) Commit(ctx context.Context) error {
	if tx.beforeCommit != nil {
		if err := tx.beforeCommit(ctx, tx.Tx); err != nil {
			return err
		}
	}
	return tx.Tx.Commit(ctx)
}

func TestConcurrentApplyLocksSchemaBeforeReadingObjects(t *testing.T) {
	for _, samePlugin := range []bool{true, false} {
		name := "different plugins share domains"
		if samePlugin {
			name = "same plugin preserves rows"
		}
		t.Run(name, func(t *testing.T) {
			kb := postgrestest.New(t)
			coreA, coreB := postgrestest.New(t), postgrestest.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if samePlugin {
				if _, err := kb.Pool.Exec(ctx, testPrelude); err != nil {
					t.Fatal(err)
				}
			}
			jarA := jarWith(t, map[string]string{"ddl.sql": testDDL, "ddl-postgresql.sql": testPrelude})
			pinA := pinOf("demo", jarA, "demo_responses", "demo_methods")
			ddlA, err := ExtractDDL(jarA, pinA)
			if err != nil {
				t.Fatal(err)
			}
			pinB, ddlB := pinA, ddlA
			if !samePlugin {
				jarB := jarWith(t, map[string]string{"ddl.sql": "CREATE TABLE second_responses (id integer, created_date datetime);", "ddl-postgresql.sql": testPrelude})
				pinB = pinOf("second", jarB, "second_responses")
				ddlB, err = ExtractDDL(jarB, pinB)
				if err != nil {
					t.Fatal(err)
				}
			}
			type outcome struct {
				result Result
				err    error
			}
			firstDDL, proceed := make(chan struct{}), make(chan struct{})
			pidA, pidB := make(chan uint32, 1), make(chan uint32, 1)
			queriedB := make(chan struct{}, 1)
			answerA, answerB := make(chan outcome, 1), make(chan outcome, 1)
			var holdOnce, releaseOnce sync.Once
			var holdErr error
			release := func() { releaseOnce.Do(func() { close(proceed) }) }
			readB := func() {
				select {
				case queriedB <- struct{}{}:
				default:
				}
			}
			firstDB := observedDDLDB{DB: kb.Pool, wrap: func(tx pgx.Tx) pgx.Tx {
				pidA <- tx.Conn().PgConn().PID()
				return observedDDLTx{Tx: tx, beforeExec: func(ctx context.Context, sql string) error {
					if strings.Contains(sql, "pg_advisory_xact_lock") {
						return nil
					}
					holdOnce.Do(func() {
						close(firstDDL)
						select {
						case <-proceed:
						case <-ctx.Done():
							holdErr = ctx.Err()
						}
					})
					return holdErr
				}, beforeCommit: func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, "INSERT INTO demo_responses (kb_account_id, created_date) VALUES ('preserved', now())")
					return err
				}}
			}}
			secondDB := observedDDLDB{DB: kb.Pool, onQuery: readB, wrap: func(tx pgx.Tx) pgx.Tx {
				pidB <- tx.Conn().PgConn().PID()
				return observedDDLTx{Tx: tx, onQuery: readB}
			}}
			var workers sync.WaitGroup
			defer func() {
				cancel()
				release()
				workers.Wait()
			}()
			workers.Add(1)
			go func() {
				defer workers.Done()
				res, err := Apply(ctx, firstDB, coreA.Pool, ddlA, pinA, ApplyOptions{AppliedBy: "deployment-a"})
				answerA <- outcome{res, err}
			}()
			select {
			case <-firstDDL:
			case answer := <-answerA:
				t.Fatalf("first apply finished before its DDL barrier: %+v, %v", answer.result, answer.err)
			case <-ctx.Done():
				t.Fatal("first apply did not reach its DDL barrier")
			}
			firstPID := <-pidA
			// Separate Core schemas prevent the per-plugin record lock from
			// hiding missing Kill Bill schema serialization.
			workers.Add(1)
			go func() {
				defer workers.Done()
				res, err := Apply(ctx, secondDB, coreB.Pool, ddlB, pinB, ApplyOptions{AppliedBy: "deployment-b"})
				answerB <- outcome{res, err}
			}()
			var secondPID uint32
			select {
			case secondPID = <-pidB:
			case <-queriedB:
				t.Fatal("second apply read objects before opening its transaction")
			case answer := <-answerB:
				t.Fatalf("second apply finished before its transaction: %+v, %v", answer.result, answer.err)
			case <-ctx.Done():
				t.Fatal("second apply did not open an overlapping transaction")
			}
			// Do not infer blocking from a sleep: require the server to report
			// that A blocks B while A has not created any objects yet.
			for {
				select {
				case <-queriedB:
					t.Fatal("second apply read objects before the schema lock was released")
				case answer := <-answerB:
					t.Fatalf("second apply bypassed schema serialization: %+v, %v", answer.result, answer.err)
				default:
				}
				var blocked bool
				if err := kb.Pool.QueryRow(ctx, "SELECT $1::integer = ANY(pg_blocking_pids($2::integer))", int32(firstPID), int32(secondPID)).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("second transaction never waited for the first schema lock")
				}
			}
			release()
			first, second := <-answerA, <-answerB
			wantDomains := 2
			if samePlugin {
				wantDomains = 0
			}
			if first.err != nil || len(first.result.Created) != 2 || len(first.result.Domains) != wantDomains {
				t.Fatalf("first apply = %+v, %v", first.result, first.err)
			}
			if second.err != nil || len(second.result.Domains) != 0 {
				t.Fatalf("second apply = %+v, %v", second.result, second.err)
			}
			if samePlugin && (second.result.Applied() || len(second.result.Present) != 2) {
				t.Fatalf("second apply recreated committed tables: %+v", second.result)
			}
			if !samePlugin && strings.Join(second.result.Created, ",") != "second_responses" {
				t.Fatalf("second plugin tables = %+v", second.result)
			}
			var rows int
			if err := kb.Pool.QueryRow(ctx, "SELECT count(*) FROM demo_responses WHERE kb_account_id = 'preserved'").Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("committed sentinel rows = %d, %v", rows, err)
			}
			for _, core := range []*postgrestest.DB{coreA, coreB} {
				records, err := Recorded(ctx, core.Pool)
				if err != nil || len(records) != 1 {
					t.Fatalf("deployment records = %+v, %v", records, err)
				}
			}
		})
	}
}

func TestConcurrentApplyRejectsCompetingDigests(t *testing.T) {
	kb, core := postgrestest.New(t), postgrestest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	jar := jarWith(t, map[string]string{"ddl.sql": "CREATE TABLE demo (id integer);"})
	pin := pinOf("demo", jar, "demo")
	first, err := ExtractDDL(jar, pin)
	if err != nil {
		t.Fatal(err)
	}
	otherJar := jarWith(t, map[string]string{"ddl.sql": "CREATE TABLE demo (id integer, value text);"})
	second, err := ExtractDDL(otherJar, pin)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result Result
		err    error
	}
	start := make(chan struct{})
	answers := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, ddl := range []DDL{first, second} {
		wg.Add(1)
		go func(d DDL) {
			defer wg.Done()
			<-start
			result, err := Apply(ctx, kb.Pool, core.Pool, d, pin, ApplyOptions{AppliedBy: "concurrent-deployment"})
			answers <- outcome{result, err}
		}(ddl)
	}
	close(start)
	wg.Wait()
	close(answers)
	var applied, rejected int
	var digest string
	for answer := range answers {
		switch {
		case answer.err == nil:
			applied++
			digest = answer.result.Digest
			if strings.Join(answer.result.Created, ",") != "demo" {
				t.Fatalf("successful apply did not create demo: %+v", answer.result)
			}
		case errors.Is(answer.err, ErrDDLChanged):
			rejected++
		default:
			t.Fatalf("concurrent apply: %v", answer.err)
		}
	}
	if applied != 1 || rejected != 1 {
		t.Fatalf("applied %d, rejected competing digest %d", applied, rejected)
	}
	records, err := Recorded(ctx, core.Pool)
	if err != nil || len(records) != 1 || records[0].Digest != digest {
		t.Fatalf("records = %+v, %v", records, err)
	}
}
