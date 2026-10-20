//go:build integration

package brand

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// A row whose stored manifest cannot be decoded is logged and skipped: the
// other brands keep working and that brand is inactive for verification.
func TestListSkipsUndecodableManifests(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	good := reference(t)
	manifest, _ := json.Marshal(good)
	insert := func(slug string, manifest []byte) {
		t.Helper()
		_, err := db.Pool.Exec(ctx, `INSERT INTO brands (id, slug, name, keycloak_realm, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest, applied_at)
VALUES ($1, $2, $2, $2, $2, 'env://BRAND_X_Y', $3, 'sha256:' || repeat('0', 64), now())`, idgen.MustNewV7(), slug, manifest)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("zs-reference", manifest)
	insert("zs-broken", []byte(`{"schema_version": "one", "identity": []}`))

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	records, err := List(ctx, db.Pool, log)
	if err != nil {
		t.Fatalf("one bad row failed the list: %v", err)
	}
	if len(records) != 1 || records[0].Slug != "zs-reference" {
		t.Fatalf("records = %+v", records)
	}
	if !strings.Contains(logs.String(), "brand=zs-broken") {
		t.Fatalf("the bad row was not logged with its slug: %s", logs.String())
	}

	reg := NewRegistry(db.Pool, time.Minute, log)
	if _, err := reg.BrandByRealm(ctx, "zs-reference"); err != nil {
		t.Fatalf("good brand: %v", err)
	}
	if _, err := reg.BrandByRealm(ctx, "zs-broken"); err != identity.ErrBrandUnknown {
		t.Fatalf("broken brand: %v", err)
	}
}
