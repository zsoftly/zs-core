package tenant

import (
	"slices"
	"strings"
	"testing"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

func TestRoleImplications(t *testing.T) {
	registered := union(Builtin, []string{"vm:create", "billing:refund"})
	owner := RoleImplies(RoleOwner, registered)
	if len(owner) != len(registered) {
		t.Fatalf("owner = %v, want every registered permission", owner)
	}
	admin := RoleImplies(RoleAdmin, registered)
	for _, p := range admin {
		if strings.HasPrefix(p, "billing:") || p == TransferPermission || slices.Contains(WorkloadPermissions, p) {
			t.Fatalf("admin holds %s", p)
		}
	}
	for _, p := range WorkloadPermissions {
		if !slices.Contains(owner, p) {
			t.Fatalf("owner lacks %s", p)
		}
	}
	if !slices.Contains(admin, "usage:read") {
		t.Fatalf("admin lacks usage:read: %v", admin)
	}
	for _, want := range []string{"member:update", "member:remove", "team:manage", "apikey:create", "vm:create", "audit:read"} {
		if !slices.Contains(admin, want) {
			t.Fatalf("admin lacks %s: %v", want, admin)
		}
	}
	billing := RoleImplies(RoleBillingAdmin, registered)
	if strings.Join(billing, ",") != "billing:manage,billing:read,billing:refund" {
		t.Fatalf("billing_admin = %v", billing)
	}
	if got := RoleImplies(RoleMember, registered); len(got) != 0 {
		t.Fatalf("member = %v", got)
	}
}

func TestCheckRegistered(t *testing.T) {
	registered := union(Builtin, []string{"vm:create"})
	if err := CheckRegistered([]string{"vm:create", "tenant:read"}, registered); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"vm:delete", "NOT A PERMISSION"} {
		err := CheckRegistered([]string{"tenant:read", bad}, registered)
		if !zserrors.IsCode(err, zserrors.CodePermissionUnknown) {
			t.Fatalf("%q: %v", bad, err)
		}
		if bad == "NOT A PERMISSION" && strings.Contains(err.Error(), bad) {
			t.Fatalf("malformed permission echoed: %v", err)
		}
	}
}

func TestPermissionSetHelpers(t *testing.T) {
	if got := Intersect([]string{"b:x", "a:x", "a:x", "c:x"}, []string{"a:x", "c:x"}); strings.Join(got, ",") != "a:x,c:x" {
		t.Fatalf("Intersect = %v", got)
	}
	if !Subset([]string{"a:x"}, []string{"a:x", "b:x"}) || Subset([]string{"a:x", "z:x"}, []string{"a:x"}) {
		t.Fatal("Subset")
	}
	if !ValidPermission("tenant:read") || ValidPermission("tenant") || ValidPermission("Tenant:read") {
		t.Fatal("ValidPermission")
	}
}

func TestCallerFailsClosed(t *testing.T) {
	var c Caller
	if c.Has("tenant:read") || c.require("tenant:read") == nil || c.requireMFA() == nil {
		t.Fatal("an empty caller holds something")
	}
	c = Caller{Permissions: []string{"tenant:read"}, RecentMFA: true}
	if c.require("tenant:read") != nil || c.requireAll([]string{"tenant:read", "tenant:update"}) == nil || c.requireMFA() != nil {
		t.Fatal("caller checks")
	}
}

func TestSystemTeamsFollowRoles(t *testing.T) {
	cases := map[Role]string{
		RoleOwner: "owners,members", RoleAdmin: "admins,members", RoleBillingAdmin: "members", RoleMember: "members",
	}
	for role, want := range cases {
		if got := strings.Join(systemTeamsFor(role), ","); got != want {
			t.Errorf("%s: %s, want %s", role, got, want)
		}
	}
}

func TestDeriveSlug(t *testing.T) {
	id := idgen.MustNewV7()
	tail := idgen.Format(idgen.Tenant, id)
	tail = tail[len(tail)-8:]
	for name, want := range map[string]string{
		"Acme Inc.":               "acme-inc-" + tail,
		"  Élan -- Studio ":       "lan-studio-" + tail,
		"!!!":                     "tenant-" + tail,
		strings.Repeat("ab ", 30): strings.Repeat("ab-", 13) + "a-" + tail,
	} {
		got := deriveSlug(name, id)
		if got != want {
			t.Errorf("deriveSlug(%q) = %q, want %q", name, got, want)
		}
		if len(got) > 63 || strings.Contains(got, "--") || strings.HasPrefix(got, "-") {
			t.Errorf("deriveSlug(%q) = %q is not a valid slug", name, got)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	id := idgen.MustNewV7()
	limit, after, err := Page{Limit: 10, Cursor: EncodeCursor(id)}.After()
	if err != nil || limit != 10 || after != id {
		t.Fatalf("After = %d %v %v", limit, after, err)
	}
	if limit, after, err := (Page{}).After(); err != nil || limit != DefaultLimit || !after.IsNil() {
		t.Fatalf("zero page = %d %v %v", limit, after, err)
	}
	for _, bad := range []Page{{Limit: -1}, {Limit: MaxLimit + 1}} {
		if _, _, err := bad.After(); !zserrors.IsCode(err, zserrors.CodeInvalidLimit) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	for _, bad := range []string{"nope", EncodeCursor(id)[:10], strings.Repeat("a", 200)} {
		if _, _, err := (Page{Cursor: bad}).After(); !zserrors.IsCode(err, zserrors.CodeInvalidCursor) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestEmailChecks(t *testing.T) {
	if normalizeEmail("  Ana@Example.TEST ") != "ana@example.test" {
		t.Fatal("normalizeEmail")
	}
	for email, ok := range map[string]bool{
		"ana@example.test": true, "ana": false, "@example.test": false, "ana@": false, "a b@example.test": false,
		"ana@x@y": false,
	} {
		if validEmail(email) != ok {
			t.Errorf("validEmail(%q) != %v", email, ok)
		}
	}
}
