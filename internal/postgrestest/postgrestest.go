// Package postgrestest gives each integration test its own Postgres schema.
//
// The server is either the one named by ZS_TEST_POSTGRES_URL or a Postgres 18
// container started once per test binary through testcontainers. Each call to
// New creates a fresh schema named zs_test_<random>, points a new pool's
// search_path at it, applies the embedded migrations and drops the schema
// when the test ends. Tests therefore never see each other's rows and can run
// in parallel.
//
// When ZS_TEST_POSTGRES_URL is unset and Docker is not reachable, New skips
// the test with a message that says how to run it.
//
// A container server is faster still. The first call to New migrates one
// template database; every later call clones it with CREATE DATABASE ...
// TEMPLATE, which copies the files instead of running every migration and
// then dropping a schema with its partitions under the lock below. Each test
// then has a database of its own, not a schema, and DB.Schema names it. A
// server named by ZS_TEST_POSTGRES_URL keeps the schema mode, because it may
// not let the test user create databases, and a failed clone falls back to it.
//
// Migrating a schema and dropping it lock every table, index and partition
// of the schema. Many test binaries doing that at once on one server run
// out of Postgres's shared lock table, so the harness runs those two steps
// one at a time across every test process, under a server-wide advisory
// lock. The tests themselves still run in parallel.
package postgrestest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

const (
	// EnvURL names an existing Postgres server to use instead of a
	// container. It must point at a loopback host.
	EnvURL = "ZS_TEST_POSTGRES_URL"
	// EnvRequireTemplate, set to 1, makes a missing or broken template
	// database fail the test instead of quietly falling back to the slower
	// schema per test.
	EnvRequireTemplate = "ZS_TEST_REQUIRE_TEMPLATE"
	// Image is the container image used when EnvURL is unset.
	Image = "postgres:18-alpine"

	schemaPrefix   = "zs_test_"
	startupTimeout = 2 * time.Minute
)

// DB is one isolated, migrated schema, or one cloned database.
type DB struct {
	Pool *pgxpool.Pool
	// Schema names the isolation unit: a schema, or the database name when
	// the test runs in a clone of the template, where the schema is public.
	Schema string
	// URL connects to the same server with search_path set to Schema.
	URL string
}

type server struct {
	url      string
	admin    *pgxpool.Pool
	skip     string
	err      error
	external bool
}

var (
	shared     server
	sharedOnce sync.Once
)

// New returns a migrated, isolated schema for t.
func New(t testing.TB) *DB {
	t.Helper()
	if db := newFromTemplate(t); db != nil {
		return db
	}
	return NewSchema(t)
}

// NewSchema returns a migrated schema in the one database of the server, for
// the tests whose subject is state that the schemas of one database share,
// such as a server-wide advisory lock. Two calls give two schemas of the same
// database, where New gives two databases that share nothing.
func NewSchema(t testing.TB) *DB {
	t.Helper()
	db := NewEmpty(t)
	err := withDDLLock(func() error {
		_, err := database.Migrate(context.Background(), db.Pool, migrations.FS)
		return err
	})
	if err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return db
}

// ddlLockSQL is the key of the server-wide lock that serializes the
// migration and the drop of test schemas.
const ddlLockSQL = `hashtextextended('zs-core.postgrestest.ddl', 0)`

// withDDLLock runs fn while this process holds the server-wide DDL lock of
// the harness, on a connection of the admin pool.
func withDDLLock(fn func() error) error {
	ctx := context.Background()
	conn, err := shared.admin.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire admin connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+ddlLockSQL+`)`); err != nil {
		return fmt.Errorf("take the harness DDL lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(`+ddlLockSQL+`)`) }()
	return fn()
}

// NewEmpty returns an isolated schema without migrations, for tests of the
// migration runner itself.
func NewEmpty(t testing.TB) *DB {
	t.Helper()
	srv := start()
	if srv.skip != "" {
		t.Skip(srv.skip)
	}
	if srv.err != nil {
		t.Fatalf("start test Postgres: %v", srv.err)
	}

	ctx := context.Background()
	schema := schemaPrefix + randomHex(t, 8)
	if _, err := srv.admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(srv.url)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		err := withDDLLock(func() error {
			_, err := srv.admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			return err
		})
		if err != nil {
			t.Errorf("drop test schema %s: %v", schema, err)
		}
	})

	schemaURL, err := withSearchPath(srv.url, schema)
	if err != nil {
		t.Fatalf("build schema URL: %v", err)
	}
	return &DB{Pool: pool, Schema: schema, URL: schemaURL}
}

// NewScope returns a random, complete brand and tenant scope.
func NewScope(t testing.TB) scope.Scope {
	t.Helper()
	s, err := scope.New(idgen.MustNewV7(), idgen.MustNewV7())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func start() server {
	sharedOnce.Do(func() {
		ctx := context.Background()
		if raw := strings.TrimSpace(os.Getenv(EnvURL)); raw != "" {
			if err := validateExternalURL(raw); err != nil {
				shared.err = err
				return
			}
			shared.url = raw
			shared.external = true
		} else {
			if reason := dockerUnavailable(ctx); reason != "" {
				shared.skip = "integration test skipped: " + EnvURL + " is not set and Docker is not reachable (" + reason +
					"). Start Docker or set " + EnvURL + "; see docs/development.md."
				return
			}
			url, err := runContainer(ctx)
			if err != nil {
				shared.err = err
				return
			}
			shared.url = url
		}
		admin, err := pgxpool.New(ctx, shared.url)
		if err != nil {
			shared.err = fmt.Errorf("connect admin pool: %w", err)
			return
		}
		pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := admin.Ping(pingCtx); err != nil {
			admin.Close()
			shared.err = fmt.Errorf("ping test Postgres: %w", err)
			return
		}
		shared.admin = admin
	})
	return shared
}

func dockerUnavailable(ctx context.Context) (reason string) {
	defer func() {
		if r := recover(); r != nil {
			reason = fmt.Sprint(r)
		}
	}()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return err.Error()
	}
	defer func() { _ = provider.Close() }()
	healthCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := provider.Health(healthCtx); err != nil {
		return err.Error()
	}
	return ""
}

func runContainer(ctx context.Context) (string, error) {
	container, err := postgrescontainer.Run(ctx, Image,
		postgrescontainer.WithDatabase("zs_core_test"),
		postgrescontainer.WithUsername("zs"),
		postgrescontainer.WithPassword("zs-test"),
		testcontainers.WithWaitStrategyAndDeadline(startupTimeout,
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(startupTimeout),
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(startupTimeout),
		),
	)
	if err != nil {
		return "", fmt.Errorf("start Postgres container: %w", err)
	}
	// The container is removed by the testcontainers reaper when the test
	// binary exits.
	connection, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return "", fmt.Errorf("container connection string: %w", err)
	}
	return connection, nil
}

func validateExternalURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" {
		return errors.New(EnvURL + " must be a postgres:// URL")
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New(EnvURL + " must point at a loopback host; tests create and drop schemas there")
}

func withSearchPath(raw, schema string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
	// cloneMu serializes CREATE DATABASE ... TEMPLATE: Postgres refuses a
	// copy of a template that another copy is reading.
	cloneMu sync.Mutex
)

// newFromTemplate returns a migrated database cloned from the template of
// this test binary, or nil when the schema mode is to be used: on a server
// named by EnvURL, or when the template or the clone could not be made.
func newFromTemplate(t testing.TB) *DB {
	t.Helper()
	srv := start()
	if srv.skip != "" {
		t.Skip(srv.skip)
	}
	if srv.err != nil {
		t.Fatalf("start test Postgres: %v", srv.err)
	}
	if srv.external {
		return nil
	}
	ctx := context.Background()
	templateOnce.Do(func() { templateName, templateErr = buildTemplate(ctx, srv) })
	if templateErr != nil {
		if templateRequired() {
			t.Fatalf("%s is set and the template database is unavailable: %v", EnvRequireTemplate, templateErr)
		}
		t.Logf("template database unavailable, using a schema per test: %v", templateErr)
		return nil
	}
	name := schemaPrefix + randomHex(t, 8)
	if err := cloneDatabase(ctx, srv, name); err != nil {
		if templateRequired() {
			t.Fatalf("%s is set and cloning the template failed: %v", EnvRequireTemplate, err)
		}
		t.Logf("cloning the template failed, using a schema per test: %v", err)
		return nil
	}
	dbURL, err := withDatabase(srv.url, name)
	if err != nil {
		t.Fatalf("build database URL: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := srv.admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	return &DB{Pool: pool, Schema: name, URL: dbURL}
}

// buildTemplate migrates the template database once.
func buildTemplate(ctx context.Context, srv server) (string, error) {
	name := "zs_tmpl_" + hex.EncodeToString(randomBytes(8))
	if _, err := srv.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", fmt.Errorf("create template database: %w", err)
	}
	dbURL, err := withDatabase(srv.url, name)
	if err != nil {
		return "", err
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return "", fmt.Errorf("connect template database: %w", err)
	}
	// The pool must be closed before the template is copied: Postgres does
	// not copy a database that has sessions.
	defer pool.Close()
	if _, err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		return "", fmt.Errorf("migrate template database: %w", err)
	}
	return name, nil
}

// cloneDatabase copies the template into a new database, retrying while
// Postgres still sees a session on the template.
func cloneDatabase(ctx context.Context, srv server, name string) error {
	cloneMu.Lock()
	defer cloneMu.Unlock()
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		_, err = srv.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{templateName}.Sanitize())
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55006" {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// templateRequired reports whether a missing template database must fail.
func templateRequired() bool { return os.Getenv(EnvRequireTemplate) == "1" }
