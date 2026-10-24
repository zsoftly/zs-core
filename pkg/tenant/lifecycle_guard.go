package tenant

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

const (
	lifecycleAcquireLimit = 5 * time.Second
	lifecycleCleanupLimit = 2 * time.Second
	// Global sweep keys use the nonnegative half of bigint; tenant lifecycle
	// keys use the negative half. The class bit makes cross-class collisions
	// impossible without claiming collision freedom within either hash class.
	globalSweepNamespace     int64 = 0
	tenantLifecycleNamespace int64 = -9223372036854775808
	lifecycleResourceMask    int64 = 9223372036854775807
)

var ErrLifecycleReleased = errors.New("tenant lifecycle capability is released")
var ErrWrongScope = errors.New("tenant lifecycle capability scope does not match target")

var _ DB = (*LifecycleGuard)(nil)
var _ pgx.Tx = (*lifecycleTx)(nil)
var _ pgx.Rows = (*lifecycleRows)(nil)

// LifecycleGuard owns one connection from the configured pool and one
// session advisory lock. It is intentionally not a general DB: callers may
// use its short transactions, but cannot recover the pool or raw connection.
type LifecycleGuard struct {
	mu               *sync.Mutex
	conn             *pgxpool.Conn
	scope            scope.Scope
	key              int64
	schema           string
	global           bool
	released         bool
	child            *LifecycleGuard
	parent           *LifecycleGuard
	parentGeneration uint64
	generation       uint64
	activeTx         *lifecycleTx
	rows             *lifecycleRows
	discarded        *bool
	hijacked         *bool
}

// AcquireLifecycleGuard takes a tenant-scoped session lock. got is false when
// another owner already holds the tenant capability.
func AcquireLifecycleGuard(ctx context.Context, pool *pgxpool.Pool, s scope.Scope) (*LifecycleGuard, bool, error) {
	if err := s.Validate(); err != nil {
		return nil, false, err
	}
	return acquireLifecycle(ctx, pool, s, false, nil)
}

// LifecycleSweep owns the global sweep capability. It may admit one tenant
// child at a time, on the same session, through TryTenant.
type LifecycleSweep struct{ *LifecycleGuard }

// AcquireLifecycleSweep takes the nonnegative global sweep lock. got is false
// when another sweep owns it.
func AcquireLifecycleSweep(ctx context.Context, pool *pgxpool.Pool) (*LifecycleSweep, bool, error) {
	g, got, err := acquireLifecycle(ctx, pool, scope.Scope{}, true, nil)
	if err != nil || !got {
		return nil, got, err
	}
	return &LifecycleSweep{LifecycleGuard: g}, true, nil
}

// TryTenant admits one tenant child while the global sweep lock is live. A
// busy child is skipped without releasing the parent capability.
func (g *LifecycleSweep) TryTenant(ctx context.Context, s scope.Scope) (*LifecycleGuard, bool, error) {
	if g == nil || g.LifecycleGuard == nil {
		return nil, false, ErrLifecycleReleased
	}
	if err := s.Validate(); err != nil {
		return nil, false, err
	}
	return acquireLifecycle(ctx, nil, s, false, g.LifecycleGuard)
}

func acquireLifecycle(ctx context.Context, pool *pgxpool.Pool, s scope.Scope, global bool, parent *LifecycleGuard) (*LifecycleGuard, bool, error) {
	acquireCtx, cancel := lifecycleContext(ctx, lifecycleAcquireLimit)
	defer cancel()
	var conn *pgxpool.Conn
	parentLocked := false
	if parent != nil {
		parent.mu.Lock()
		parentLocked = true
		defer func() {
			if parentLocked {
				parent.mu.Unlock()
			}
		}()
		if parent.readyLocked(true) != nil || !parent.global {
			return nil, false, ErrLifecycleReleased
		}
		if parent.child != nil || parent.activeTx != nil || parent.rows != nil {
			return nil, false, nil
		}
		conn = parent.conn
	} else {
		if pool == nil {
			return nil, false, errors.New("tenant lifecycle guard needs a connection pool")
		}
		var err error
		conn, err = pool.Acquire(acquireCtx)
		if err != nil {
			return nil, false, unavailable(err)
		}
	}

	discarded, hijacked := false, false
	g := &LifecycleGuard{conn: conn, scope: s, global: global, parent: parent, generation: 1, mu: &sync.Mutex{}, discarded: &discarded, hijacked: &hijacked}
	if parent != nil {
		g.parentGeneration = parent.generation
		g.mu = parent.mu
		g.discarded = parent.discarded
		g.hijacked = parent.hijacked
	}
	var err error
	if global {
		err = conn.QueryRow(acquireCtx, `SELECT current_schema(), (hashtextextended('zs-core.tenant.sweep:' || current_schema(), 0) & 9223372036854775807::bigint)`).Scan(&g.schema, &g.key)
	} else {
		err = conn.QueryRow(acquireCtx, `SELECT current_schema(), (hashtextextended('zs-core.tenant.closure:' || current_schema() || ':' || $1::uuid::text || ':' || $2::uuid::text, 0) | '-9223372036854775808'::bigint)`, s.Brand, s.Tenant).Scan(&g.schema, &g.key)
	}
	if err != nil || g.schema == "" || (global && g.key < globalSweepNamespace) || (!global && g.key >= globalSweepNamespace) {
		if parentLocked {
			parent.invalidateLocked()
			parent.mu.Unlock()
			parentLocked = false
		}
		g.discard()
		if err != nil {
			return nil, false, unavailable(err)
		}
		return nil, false, errors.New("invalid tenant lifecycle advisory key")
	}
	var locked bool
	if err := conn.QueryRow(acquireCtx, `SELECT pg_try_advisory_lock($1::bigint)`, g.key).Scan(&locked); err != nil {
		if parentLocked {
			parent.invalidateLocked()
			parent.mu.Unlock()
			parentLocked = false
		}
		g.discard()
		return nil, false, unavailable(err)
	}
	if !locked {
		if parent == nil {
			conn.Release()
		}
		return nil, false, nil
	}
	if parent != nil {
		parent.child = g
	}
	return g, true, nil
}

// Begin starts the one short transaction allowed by this capability.
func (g *LifecycleGuard) Begin(ctx context.Context) (pgx.Tx, error) {
	g.mu.Lock()
	if err := g.readyLocked(false); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if err := g.targetErr(ctx, nil); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.activeTx != nil || g.rows != nil {
		g.mu.Unlock()
		return nil, errors.New("tenant lifecycle operation already active")
	}
	tx, err := g.conn.Begin(ctx)
	if err != nil {
		g.invalidateLocked()
		g.mu.Unlock()
		g.discard()
		return nil, unavailable(err)
	}
	bound := &lifecycleTx{owner: g, generation: g.generation, tx: tx}
	g.activeTx = bound
	g.mu.Unlock()
	return bound, nil
}

// Query opens one owned result. The returned wrapper deliberately omits the
// pgx Rows Conn escape hatch and invalidates when its owner generation ends.
func (g *LifecycleGuard) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	g.mu.Lock()
	if err := g.readyLocked(false); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if err := g.targetErr(ctx, args); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.activeTx != nil || g.rows != nil {
		g.mu.Unlock()
		return nil, errors.New("tenant lifecycle operation already active")
	}
	rows, err := g.conn.Query(ctx, sql, args...)
	if err != nil {
		g.invalidateLocked()
		g.mu.Unlock()
		g.discard()
		return nil, unavailable(err)
	}
	bound := &lifecycleRows{owner: g, generation: g.generation, rows: rows}
	g.rows = bound
	g.mu.Unlock()
	return bound, nil
}

// QueryRow materializes its row through an owned Rows wrapper, so unread
// single-row results are closed during capability cleanup.
func (g *LifecycleGuard) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := g.Query(ctx, sql, args...)
	if err != nil {
		return lifecycleErrorRow{err}
	}
	return &lifecycleRow{rows: rows.(*lifecycleRows)}
}

func (g *LifecycleGuard) ready(child bool) error {
	if g == nil {
		return ErrLifecycleReleased
	}
	g.mu.Lock()
	err := g.readyLocked(child)
	g.mu.Unlock()
	return err
}

func (g *LifecycleGuard) readyLocked(child bool) error {
	if g.released || *g.discarded || (!child && g.child != nil) {
		return ErrLifecycleReleased
	}
	if g.parent != nil {
		parentLive := !g.parent.released && !*g.parent.discarded && g.parent.generation == g.parentGeneration && g.parent.child == g
		if !parentLive {
			return ErrLifecycleReleased
		}
	}
	return nil
}

func (g *LifecycleGuard) targetErr(ctx context.Context, args []any) error {
	if brand, err := scope.BrandFromContext(ctx); err == nil && brand != g.scope.Brand {
		return ErrWrongScope
	}
	if target, err := scope.FromContext(ctx); err == nil && (target.Brand != g.scope.Brand || target.Tenant != g.scope.Tenant) {
		return ErrWrongScope
	}
	if len(args) >= 2 {
		brand, brandOK := args[0].(idgen.UUID)
		tenant, tenantOK := args[1].(idgen.UUID)
		if brandOK && tenantOK && (brand != g.scope.Brand || tenant != g.scope.Tenant) {
			return ErrWrongScope
		}
	}
	return nil
}

func (g *LifecycleGuard) invalidateLocked() {
	g.released = true
	*g.discarded = true
	g.generation++
	if g.parent != nil {
		g.parent.released = true
		*g.parent.discarded = true
		g.parent.generation++
	}
}

type lifecycleRow struct {
	rows *lifecycleRows
	err  error
}

func (r *lifecycleRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.rows == nil || !r.rows.Next() {
		err := r.rows.Err()
		r.rows.Close()
		if err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	err := r.rows.Scan(dest...)
	r.rows.Close()
	return err
}

type lifecycleRows struct {
	owner      *LifecycleGuard
	generation uint64
	rows       pgx.Rows
	closed     bool
}

func (r *lifecycleRows) Next() bool {
	if r == nil {
		return false
	}
	r.owner.mu.Lock()
	if r.closed || r.owner.readyLocked(false) != nil || r.owner.generation != r.generation {
		r.owner.mu.Unlock()
		return false
	}
	ok := r.rows.Next()
	failed := false
	if !ok {
		failed = r.rows.Err() != nil
		r.closed = true
		r.rows.Close()
		if r.owner.rows == r {
			r.owner.rows = nil
		}
		if failed {
			r.owner.invalidateLocked()
		}
	}
	r.owner.mu.Unlock()
	if failed {
		r.owner.discard()
	}
	return ok
}

func (r *lifecycleRows) Scan(dest ...any) error {
	if r == nil {
		return ErrLifecycleReleased
	}
	r.owner.mu.Lock()
	if r.closed || r.owner.readyLocked(false) != nil || r.owner.generation != r.generation {
		r.owner.mu.Unlock()
		return ErrLifecycleReleased
	}
	err := r.rows.Scan(dest...)
	if err != nil {
		r.owner.invalidateLocked()
	}
	r.owner.mu.Unlock()
	if err != nil {
		r.owner.discard()
	}
	return err
}

func (r *lifecycleRows) Err() error {
	if r == nil || r.owner.ready(false) != nil || r.owner.generation != r.generation {
		return ErrLifecycleReleased
	}
	err := r.rows.Err()
	if err != nil {
		r.owner.mu.Lock()
		r.owner.invalidateLocked()
		r.owner.mu.Unlock()
		r.owner.discard()
	}
	return err
}

func (r *lifecycleRows) CommandTag() pgconn.CommandTag {
	if r == nil || r.owner.ready(false) != nil || r.owner.generation != r.generation {
		return pgconn.CommandTag{}
	}
	return r.rows.CommandTag()
}

func (r *lifecycleRows) FieldDescriptions() []pgconn.FieldDescription {
	if r == nil || r.owner.ready(false) != nil || r.owner.generation != r.generation {
		return nil
	}
	return r.rows.FieldDescriptions()
}

func (r *lifecycleRows) Values() ([]any, error) {
	if r == nil || r.owner.ready(false) != nil || r.owner.generation != r.generation {
		return nil, ErrLifecycleReleased
	}
	return r.rows.Values()
}

func (r *lifecycleRows) RawValues() [][]byte {
	if r == nil || r.owner.ready(false) != nil || r.owner.generation != r.generation {
		return nil
	}
	return r.rows.RawValues()
}

func (r *lifecycleRows) TypeMap() *pgtype.Map {
	if r == nil || r.owner.ready(false) != nil || r.owner.generation != r.generation {
		return nil
	}
	return r.rows.TypeMap()
}

// Conn deliberately does not expose the guarded connection.
func (r *lifecycleRows) Conn() *pgx.Conn { return nil }

func (r *lifecycleRows) Close() {
	if r == nil || r.closed {
		return
	}
	r.owner.mu.Lock()
	r.closed = true
	r.rows.Close()
	failed := r.rows.Err() != nil
	if r.owner.rows == r {
		r.owner.rows = nil
	}
	if failed {
		r.owner.invalidateLocked()
	}
	r.owner.mu.Unlock()
	if failed {
		r.owner.discard()
	}
}

type lifecycleTx struct {
	owner      *LifecycleGuard
	generation uint64
	tx         pgx.Tx
	done       bool
}

func (t *lifecycleTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := t.owner.targetErr(ctx, args); err != nil {
		return pgconn.CommandTag{}, err
	}
	if err := t.readyWithoutRows(); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		t.owner.mu.Lock()
		t.owner.invalidateLocked()
		t.owner.mu.Unlock()
		t.owner.discard()
	}
	return tag, err
}

func (t *lifecycleTx) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("nested tenant lifecycle transactions are not allowed")
}

func (t *lifecycleTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("copy is not allowed by tenant lifecycle capability")
}

func (t *lifecycleTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	return lifecycleBatchResults{err: errors.New("batch is not allowed by tenant lifecycle capability")}
}

func (t *lifecycleTx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }

func (t *lifecycleTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	if err := t.owner.targetErr(ctx, nil); err != nil {
		return nil, err
	}
	if err := t.readyWithoutRows(); err != nil {
		return nil, err
	}
	return t.tx.Prepare(ctx, name, sql)
}

func (t *lifecycleTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := t.owner.targetErr(ctx, args); err != nil {
		return nil, err
	}
	if err := t.ready(); err != nil {
		return nil, err
	}
	t.owner.mu.Lock()
	if t.owner.rows != nil {
		t.owner.mu.Unlock()
		return nil, errors.New("tenant lifecycle result already active")
	}
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		t.owner.invalidateLocked()
		t.owner.mu.Unlock()
		t.owner.discard()
		return nil, err
	}
	bound := &lifecycleRows{owner: t.owner, generation: t.generation, rows: rows}
	t.owner.rows = bound
	t.owner.mu.Unlock()
	return bound, nil
}

func (t *lifecycleTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := t.Query(ctx, sql, args...)
	if err != nil {
		return lifecycleErrorRow{err}
	}
	return &lifecycleRow{rows: rows.(*lifecycleRows)}
}

// Conn deliberately does not expose the guarded connection.
func (t *lifecycleTx) Conn() *pgx.Conn { return nil }

type lifecycleBatchResults struct{ err error }

func (r lifecycleBatchResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }
func (r lifecycleBatchResults) Query() (pgx.Rows, error)         { return nil, r.err }
func (r lifecycleBatchResults) QueryRow() pgx.Row                { return lifecycleErrorRow{r.err} }
func (r lifecycleBatchResults) Close() error                     { return r.err }

type lifecycleErrorRow struct{ err error }

func (r lifecycleErrorRow) Scan(...any) error { return r.err }

func (t *lifecycleTx) Commit(ctx context.Context) error   { return t.finish(ctx, true) }
func (t *lifecycleTx) Rollback(ctx context.Context) error { return t.finish(ctx, false) }

func (t *lifecycleTx) finish(ctx context.Context, commit bool) error {
	if t == nil {
		return ErrLifecycleReleased
	}
	if err := t.owner.targetErr(ctx, nil); err != nil {
		return err
	}
	t.owner.mu.Lock()
	if t.done {
		t.owner.mu.Unlock()
		return pgx.ErrTxClosed
	}
	if t.owner.readyLocked(false) != nil || t.owner.generation != t.generation {
		t.owner.mu.Unlock()
		return ErrLifecycleReleased
	}
	if commit && t.owner.rows != nil {
		t.owner.mu.Unlock()
		return errors.New("tenant lifecycle operation already active")
	}
	t.owner.mu.Unlock()
	if err := t.ready(); err != nil {
		return err
	}
	var err error
	if commit {
		err = t.tx.Commit(ctx)
	} else {
		err = t.tx.Rollback(ctx)
	}
	t.done = true
	t.owner.mu.Lock()
	if t.owner.activeTx == t {
		t.owner.activeTx = nil
	}
	t.owner.mu.Unlock()
	if err != nil {
		t.owner.discard()
		return unavailable(err)
	}
	return err
}

func (t *lifecycleTx) ready() error {
	if t == nil {
		return ErrLifecycleReleased
	}
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	if t.done {
		return pgx.ErrTxClosed
	}
	if t.owner.readyLocked(false) != nil || t.owner.generation != t.generation {
		return ErrLifecycleReleased
	}
	return nil
}

func (t *lifecycleTx) readyWithoutRows() error {
	if err := t.ready(); err != nil {
		return err
	}
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	if t.owner.rows != nil {
		return errors.New("tenant lifecycle operation already active")
	}
	return nil
}

// Release verifies the matching unlock before returning a connection to the
// pool. Any uncertainty discards the session instead.
func (g *LifecycleGuard) Release() error {
	if g == nil {
		return ErrLifecycleReleased
	}
	g.mu.Lock()
	if g.released || *g.discarded {
		g.mu.Unlock()
		return ErrLifecycleReleased
	}
	g.released = true
	g.generation++
	rows, tx, child := g.rows, g.activeTx, g.child
	g.mu.Unlock()
	cleanup, cancel := context.WithTimeout(context.Background(), lifecycleCleanupLimit)
	defer cancel()
	if rows != nil {
		rows.Close()
	}
	if tx != nil {
		if err := tx.tx.Rollback(cleanup); err != nil {
			g.discardContext(cleanup)
			return unavailable(err)
		}
		g.mu.Lock()
		if g.activeTx == tx {
			g.activeTx = nil
		}
		g.mu.Unlock()
	}
	if child != nil {
		g.discardContext(cleanup)
		return errors.New("tenant lifecycle child remains active")
	}
	var unlocked bool
	if err := g.conn.QueryRow(cleanup, `SELECT pg_advisory_unlock($1::bigint)`, g.key).Scan(&unlocked); err != nil || !unlocked {
		g.discardContext(cleanup)
		if err != nil {
			return unavailable(err)
		}
		return errors.New("tenant lifecycle advisory unlock was not acknowledged")
	}
	if g.parent != nil {
		g.parent.mu.Lock()
		g.parent.child = nil
		g.parent.mu.Unlock()
		return nil
	}
	g.conn.Release()
	return nil
}

func (g *LifecycleGuard) discard() {
	cleanup, cancel := context.WithTimeout(context.Background(), lifecycleCleanupLimit)
	defer cancel()
	g.discardContext(cleanup)
}

func (g *LifecycleGuard) discardContext(cleanup context.Context) {
	if g == nil || g.conn == nil {
		return
	}
	g.mu.Lock()
	if *g.hijacked {
		g.mu.Unlock()
		return
	}
	*g.hijacked = true
	g.released = true
	*g.discarded = true
	g.generation++
	if g.parent != nil {
		g.parent.released = true
		*g.parent.discarded = true
		g.parent.generation++
	}
	conn := g.conn
	g.mu.Unlock()
	_ = conn.Hijack().Close(cleanup)
}

func lifecycleContext(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < limit {
		return context.WithDeadline(ctx, deadline)
	}
	return context.WithTimeout(ctx, limit)
}
