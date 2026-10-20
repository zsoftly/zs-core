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
	// Image is the container image used when EnvURL is unset.
	Image = "postgres:18-alpine"

	schemaPrefix   = "zs_test_"
	startupTimeout = 2 * time.Minute
)

// DB is one isolated, migrated schema.
type DB struct {
	Pool   *pgxpool.Pool
	Schema string
	// URL connects to the same server with search_path set to Schema.
	URL string
}

type server struct {
	url   string
	admin *pgxpool.Pool
	skip  string
	err   error
}

var (
	shared     server
	sharedOnce sync.Once
)

// New returns a migrated, isolated schema for t.
func New(t testing.TB) *DB {
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
