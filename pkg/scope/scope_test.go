package scope

import (
	"context"
	"testing"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

func TestBindRefusesIncompleteScope(t *testing.T) {
	brand, tenant := idgen.MustNewV7(), idgen.MustNewV7()
	cases := map[string]Scope{
		"empty":        {},
		"brand only":   {Brand: brand},
		"tenant only":  {Tenant: tenant},
		"nil brand id": {Brand: idgen.Nil, Tenant: tenant},
	}
	for name, s := range cases {
		if _, err := s.Bind("x"); !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
			t.Errorf("%s: Bind error = %v, want scope_required", name, err)
		}
		if _, err := New(s.Brand, s.Tenant); err == nil {
			t.Errorf("%s: New accepted an incomplete scope", name)
		}
	}
}

func TestBindOrdersBrandTenantFirst(t *testing.T) {
	s, err := New(idgen.MustNewV7(), idgen.MustNewV7())
	if err != nil {
		t.Fatal(err)
	}
	args, err := s.Bind("a", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 4 || args[0] != s.Brand || args[1] != s.Tenant || args[2] != "a" || args[3] != 2 {
		t.Fatalf("args = %v", args)
	}
}

func TestParse(t *testing.T) {
	brandID, _ := idgen.New(idgen.Brand)
	tenantID, _ := idgen.New(idgen.Tenant)
	s, err := Parse(brandID, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if s.BrandID() != brandID || s.TenantID() != tenantID {
		t.Fatalf("round trip mismatch: %s %s", s.BrandID(), s.TenantID())
	}
	if _, err := Parse("", tenantID); !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
		t.Fatalf("missing brand error = %v", err)
	}
	if _, err := Parse(brandID, ""); !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
		t.Fatalf("missing tenant error = %v", err)
	}
	if _, err := Parse(tenantID, brandID); !zserrors.IsCode(err, zserrors.CodeScopeInvalid) {
		t.Fatalf("swapped ids error = %v", err)
	}
	if _, err := Parse("brd_nope", tenantID); !zserrors.IsCode(err, zserrors.CodeScopeInvalid) {
		t.Fatalf("malformed brand error = %v", err)
	}
}

func TestContext(t *testing.T) {
	if _, err := FromContext(context.Background()); !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
		t.Fatalf("empty context error = %v", err)
	}
	if _, err := FromContext(WithContext(context.Background(), Scope{Brand: idgen.MustNewV7()})); err == nil {
		t.Fatal("FromContext accepted an incomplete scope")
	}
	s, _ := New(idgen.MustNewV7(), idgen.MustNewV7())
	got, err := FromContext(WithContext(context.Background(), s))
	if err != nil || got != s {
		t.Fatalf("FromContext = %v, %v", got, err)
	}
}

func TestBrandLevelScope(t *testing.T) {
	brand := idgen.MustNewV7()
	ctx := WithBrand(context.Background(), brand)
	got, err := BrandFromContext(ctx)
	if err != nil || got != brand {
		t.Fatalf("BrandFromContext = %v, %v", got, err)
	}
	if _, err := FromContext(ctx); err != ErrMissing {
		t.Fatalf("a brand-level scope must not satisfy FromContext: %v", err)
	}
	if _, err := BrandFromContext(context.Background()); err != ErrMissing {
		t.Fatalf("empty context: %v", err)
	}
}
