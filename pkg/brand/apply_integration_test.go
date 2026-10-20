//go:build integration

package brand

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// offlineDeps points Keycloak and Kill Bill at a port nothing listens on.
// The refusals under test happen before either engine is called.
func offlineDeps(t *testing.T, db *postgrestest.DB) Deps {
	t.Helper()
	kc, err := keycloak.New(keycloak.Config{URL: "http://127.0.0.1:1", AdminUser: "admin", AdminPassword: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	kb, err := killbill.New(killbill.Config{URL: "http://127.0.0.1:1", AdminUser: "admin", AdminPassword: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	return Deps{DB: db.Pool, Keycloak: kc, KillBill: kb, Secrets: secrets.Default{}}
}

func insertBrand(t *testing.T, db *postgrestest.DB, m *Manifest) idgen.UUID {
	t.Helper()
	manifest, _ := json.Marshal(m)
	id := idgen.MustNewV7()
	_, err := db.Pool.Exec(context.Background(), `INSERT INTO brands (id, slug, name, keycloak_realm, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest, applied_at)
VALUES ($1, $2, $2, $2, $2, $3, $4, $5, now())`, id, m.Slug, m.Billing.KillBillAPISecretRef, manifest, m.Digest())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func brandManifest(t *testing.T, slug, staffRef string) *Manifest {
	m := reference(t)
	m.Slug = slug
	m.Identity.Realm = slug
	m.Billing.KillBillTenantKey = slug
	m.Billing.Catalogue = ""
	m.Identity.StaffClient.SecretRef = staffRef
	m.Billing.KillBillAPISecretRef = "env://" + secrets.EnvPrefix(slug) + "KB"
	return m
}

var integrationActor = Options{Actor: events.Actor{Type: events.ActorService, ID: "zs-core-cli:integration-test"}}

// Brands zs and zs-reference cannot reach each other's variables, whichever
// is created first, and while neither or both exist.
func TestZsAndZsReferenceCannotReachEachOtherInEitherOrder(t *testing.T) {
	ctx := context.Background()
	reaching := map[string]*Manifest{
		"zs":           brandManifest(t, "zs", "env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET"),
		"zs-reference": brandManifest(t, "zs-reference", "env://BRAND_ZS__KEYCLOAK_STAFF_CLIENT_SECRET"),
	}
	own := map[string]*Manifest{
		"zs":           brandManifest(t, "zs", "env://BRAND_ZS__KEYCLOAK_STAFF_CLIENT_SECRET"),
		"zs-reference": brandManifest(t, "zs-reference", "env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET"),
	}
	for _, order := range [][]string{{"zs", "zs-reference"}, {"zs-reference", "zs"}} {
		db := postgrestest.New(t)
		deps := offlineDeps(t, db)
		check := func(stage string) {
			t.Helper()
			for slug, m := range reaching {
				if _, err := Plan(ctx, deps, m, integrationActor); codeOf(err) != "secret_ref_invalid" {
					t.Fatalf("%v, %s: %s reaching the other brand = %v", order, stage, slug, err)
				}
				if got := foreignNamespaceRefs(own[slug], []string{"zs", "zs-reference"}); len(got) != 0 {
					t.Fatalf("%v, %s: %s own references flagged: %v", order, stage, slug, got)
				}
			}
			if err := checkForeignNamespaces(ctx, db.Pool, own["zs"]); err != nil {
				t.Fatalf("%v, %s: zs own references: %v", order, stage, err)
			}
			if err := checkForeignNamespaces(ctx, db.Pool, own["zs-reference"]); err != nil {
				t.Fatalf("%v, %s: zs-reference own references: %v", order, stage, err)
			}
		}
		check("no brand yet")
		insertBrand(t, db, own[order[0]])
		check(order[0] + " exists")
		insertBrand(t, db, own[order[1]])
		check("both exist")
	}
}

// A manifest for another brand is refused before the per-slug lock is
// taken, so a foreign operator cannot make that brand's apply busy. The
// brand's own apply still takes the lock.
func TestForeignApplyDoesNotTakeTheBrandLock(t *testing.T) {
	ctx := context.Background()
	db := postgrestest.New(t)
	deps := offlineDeps(t, db)
	mine := brandManifest(t, "zs-reference", "env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET")
	theirs := brandManifest(t, "zs-second", "env://BRAND_ZS_SECOND__KEYCLOAK_STAFF_CLIENT_SECRET")
	mineID := insertBrand(t, db, mine)
	theirsID := insertBrand(t, db, theirs)

	// Hold zs-second's apply lock, as a running apply of zs-second would.
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('zs-core.brand.apply:' || $1, 0))`, theirs.Slug).Scan(&locked); err != nil || !locked {
		t.Fatalf("lock: %v %v", locked, err)
	}

	// The zs-reference route receives the zs-second manifest: refused as
	// another brand's, not as busy.
	opts := integrationActor
	opts.ExpectBrand = mineID
	if _, err := Apply(ctx, deps, theirs, opts); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("foreign manifest while its brand is busy = %v", err)
	}
	// An unknown slug is refused the same way.
	unknown := brandManifest(t, "zs-third", "env://BRAND_ZS_THIRD__KEYCLOAK_STAFF_CLIENT_SECRET")
	if _, err := Apply(ctx, deps, unknown, opts); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("unknown brand = %v", err)
	}
	// zs-second's own route still finds its lock taken.
	opts.ExpectBrand = theirsID
	if _, err := Apply(ctx, deps, theirs, opts); codeOf(err) != "brand_apply_in_progress" {
		t.Fatalf("own manifest while busy = %v", err)
	}
	if err := tx.Rollback(ctx); err != nil && err != pgx.ErrTxClosed {
		t.Fatal(err)
	}

	// The foreign attempt left no lock behind: zs-second's lock is free.
	var free bool
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('zs-core.brand.apply:' || $1, 0))`, theirs.Slug).Scan(&free)
	}); err != nil || !free {
		t.Fatalf("lock after the foreign attempt: %v %v", free, err)
	}
}
