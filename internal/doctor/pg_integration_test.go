//go:build integration

package doctor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/migrations"
)

func TestPGDatabase(t *testing.T) {
	ctx := context.Background()

	empty := postgrestest.NewEmpty(t)
	pg := doctor.PGDatabase{DB: empty.Pool, Migrations: migrations.FS}
	if err := pg.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := pg.Pending(ctx)
	if err != nil || len(pending) == 0 {
		t.Fatalf("an unmigrated schema must report pending migrations: %v %v", pending, err)
	}

	if _, err := database.Migrate(ctx, empty.Pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	pending, err = pg.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after migrate = %v, %v", pending, err)
	}

	version, err := pg.ServerVersion(ctx)
	if err != nil || !strings.HasPrefix(version, "1") {
		t.Fatalf("server version = %q, %v", version, err)
	}
	now, err := pg.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if skew := time.Since(now); skew > time.Minute || skew < -time.Minute {
		t.Fatalf("database clock differs from this host by %s", skew)
	}
}
