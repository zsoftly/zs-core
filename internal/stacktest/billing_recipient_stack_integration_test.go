//go:build integration

package stacktest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// Each invoice has a unique body marker. Earlier delivered or materialized
// messages never count as evidence for a later recipient selection.
type recipientMail struct {
	ID   string  `json:"ID"`
	HTML *string `json:"HTML"`
	To   []struct {
		Address string `json:"Address"`
	} `json:"To"`
	Cc []struct {
		Address string `json:"Address"`
	} `json:"Cc"`
}

func recipientMessages(t *testing.T, marker string) []recipientMail {
	t.Helper()
	base := strings.TrimRight(env(t, envMailpitURL), "/")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/api/v1/search?query=" + url.QueryEscape(marker))
	if err != nil {
		t.Fatal(err)
	}
	var list *struct {
		Messages *[]mailpitMessage `json:"messages"`
	}
	err = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || list == nil || list.Messages == nil {
		t.Fatalf("Mailpit search response: status=%d err=%v", resp.StatusCode, err)
	}
	var out []recipientMail
	for _, m := range *list.Messages {
		if m.ID == "" || strings.ContainsAny(m.ID, "/?#\\ \t\r\n") {
			t.Fatal("Mailpit search returned an invalid message ID")
		}
		resp, err := client.Get(base + "/api/v1/message/" + url.PathEscape(m.ID))
		if err != nil {
			t.Fatal(err)
		}
		var full recipientMail
		err = json.NewDecoder(resp.Body).Decode(&full)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || full.ID != m.ID || full.HTML == nil || *full.HTML == "" {
			t.Fatalf("Mailpit message response: status=%d err=%v", resp.StatusCode, err)
		}
		if strings.Contains(*full.HTML, marker) {
			out = append(out, full)
		}
	}
	return out
}

func waitRecipientMail(t *testing.T, marker, to, cc string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		messages := recipientMessages(t, marker)
		if len(messages) > 0 {
			for _, m := range messages {
				if len(m.To) != 1 || m.To[0].Address != to || len(m.Cc) != 1 || m.Cc[0].Address != cc {
					t.Fatal("fresh invoice did not have the expected primary and unchanged CC")
				}
			}
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("fresh invoice message not observed")
}

func TestBillingRecipientsOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	ctx := context.Background()
	deps := s.deps(t, db)
	env(t, envMailpitURL)
	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, deps, devVariant(t, ref))
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), s.deps(t, db), ref, cli) })
	if v, state := pluginOnNodes(t, s, "email-notifications"); v != "0.8.1" || state != "RUNNING" {
		t.Fatalf("pinned email plugin: %s %s", v, state)
	}
	br, err := brand.GetBySlug(ctx, db.Pool, "zs-reference")
	if err != nil {
		t.Fatal(err)
	}
	creds := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}
	eng := tenant.Engines{Keycloak: s.kc, KillBill: s.kb, Secrets: secrets.Default{}}
	api, _ := billingAPI(t, s, db, deps)
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	issuer := strings.TrimRight(s.kcURL, "/") + "/realms/zs-reference"
	users := []string{"recipient-old-" + suffix, "recipient-named-" + suffix, "recipient-fallback-" + suffix}
	tokens := make([]tokenSet, len(users))
	for i, user := range users {
		createUserWithLocale(t, s.kc, "zs-reference", user, "pw-"+suffix, "en")
		tokens[i] = passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", user, "pw-"+suffix)
		if status, _ := api.call(t, http.MethodGet, "/v1/me", tokens[i].access, nil, ""); status != http.StatusOK {
			t.Fatalf("identity discovery: %d", status)
		}
	}
	id := newTenantFor(t, api, tokens[0].access, "Recipient "+suffix)
	uuid, err := idgen.ParseAs(idgen.Tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	sc := scope.Scope{Brand: br.ID, Tenant: uuid}
	tn, err := tenant.Get(ctx, db.Pool, sc)
	if err != nil {
		t.Fatal(err)
	}
	creator := tn.CreatedByIdentityID
	accountID := tn.KillBillAccountID
	owner := func(i int) tenant.Caller { return stackCaller(t, db, br.ID, sc, issuer, tokens[i].subject) }
	members := make([]tenant.Membership, 3)
	members[0] = owner(0).Membership
	for _, i := range []int{2, 1} {
		var invitation string
		err = pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
			_, token, err := tenant.CreateInvitation(ctx, tx, sc, owner(0), users[i]+"@example.test", tenant.RoleAdmin)
			invitation = token
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		members[i], err = acceptAs(t, db, br.ID, issuer, tokens[i], invitation)
		if err != nil {
			t.Fatal(err)
		}
	}
	role := tenant.RoleOwner
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, owner(0), members[2].ID, tenant.MemberChange{Role: &role})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Test-only setup seeds non-email fields and one unmanaged AccountEmails row.
	seed := map[string]any{"billCycleDayLocal": 17, "address1": "17 Recipient Test Street", "address2": "Suite 2", "postalCode": "K1P 5G3", "company": "Recipient Test", "city": "Ottawa", "state": "ON", "country": "CA", "phone": "555-0100", "notes": "preserve-recipient-test", "locale": "en"}
	if status, _, body := kbRaw(t, creds, http.MethodPut, "/1.0/kb/accounts/"+accountID+"?treatNullAsReset=false", seed); status != http.StatusNoContent {
		t.Fatalf("seed account fields: %d %s", status, body)
	}
	cc := "unmanaged-" + suffix + "@example.test"
	if status, _, body := kbRaw(t, creds, http.MethodPost, "/1.0/kb/accounts/"+accountID+"/emails", map[string]any{"accountId": accountID, "email": cc}); status != http.StatusCreated {
		t.Fatalf("seed unmanaged CC: %d %s", status, body)
	}
	baseline := accountOf(t, creds, id)
	if baseline["billCycleDayLocal"] != float64(17) {
		t.Fatal("nonzero BCD setup not observed")
	}
	delete(baseline, "email")
	verify := func(email string, status tenant.BillingRecipientStatus) {
		t.Helper()
		r, err := tenant.ReconcileBillingRecipient(ctx, db.Pool, eng, sc, "stack-recipient")
		if err != nil || !r.Matches || r.Status != status {
			t.Fatalf("recipient reconciliation: %+v %v", r, err)
		}
		a := accountOf(t, creds, id)
		if a["email"] != email {
			t.Fatal("actual primary differs from selection")
		}
		delete(a, "email")
		if !reflect.DeepEqual(a, baseline) {
			t.Fatalf("non-email fields changed: before=%s after=%s", mustJSON(baseline), mustJSON(a))
		}
		emails, err := s.kb.AccountEmails(ctx, creds, accountID)
		if err != nil || len(emails) != 1 || emails[0].Email != cc {
			t.Fatalf("unmanaged CC not preserved: count=%d err=%v", len(emails), err)
		}
		fs, err := doctor.BillingRecipientFindings(ctx, db.Pool, eng)
		if err != nil {
			t.Fatal(err)
		}
		warn := false
		for _, f := range fs {
			if f.Check == "billing_recipient_cc" && f.Status == doctor.Warn && strings.Contains(f.Message, accountID) {
				warn = true
			}
			if f.Status == doctor.Fail {
				t.Fatalf("doctor unexpected failure: %s", f.String())
			}
		}
		if !warn {
			t.Fatal("doctor omitted unmanaged CC warning")
		}
		var text, encoded bytes.Buffer
		report := doctor.Report{Findings: fs}
		if err := report.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		if err := report.WriteJSON(&encoded); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text.String(), "@") || strings.Contains(encoded.String(), "@") {
			t.Fatal("doctor emitted address")
		}
		t.Logf("recipient observed: status=%s account=%s bcd=17 unmanaged_cc=1", r.Status, accountID)
	}
	verify(users[0]+"@example.test", tenant.BillingRecipientReady)
	oldMarker := "recipientold" + suffix
	taxedInvoice(t, creds, accountID, oldMarker, "test tax")
	waitRecipientMail(t, oldMarker, users[0]+"@example.test", cc)
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, _, err := tenant.TransferOwnership(ctx, tx, sc, owner(0), members[1].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	verify(users[1]+"@example.test", tenant.BillingRecipientReady)
	namedMarker := "recipientnamed" + suffix
	taxedInvoice(t, creds, accountID, namedMarker, "test tax")
	waitRecipientMail(t, namedMarker, users[1]+"@example.test", cc)
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { _, err := tenant.RemoveMember(ctx, tx, sc, owner(2), members[1].ID); return err }); err != nil {
		t.Fatal(err)
	}
	verify(users[2]+"@example.test", tenant.BillingRecipientReady)
	fallbackMarker := "recipientfallback" + suffix
	taxedInvoice(t, creds, accountID, fallbackMarker, "test tax")
	waitRecipientMail(t, fallbackMarker, users[2]+"@example.test", cc)
	if _, err := db.Pool.Exec(ctx, `UPDATE identities SET email_verified=false WHERE brand_id=$1 AND id=$2`, br.ID, members[2].IdentityID); err != nil {
		t.Fatal(err)
	}
	verify("", tenant.BillingRecipientNoEligibleOwner)
	blankMarker := "recipientblank" + suffix
	taxedInvoice(t, creds, accountID, blankMarker, "test tax")
	// Observe a bounded SMTP absence window, then restore eligibility and
	// require a later positive delivery. This makes no SES or recall claim.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(recipientMessages(t, blankMarker)) != 0 {
			t.Fatal("blank-primary invoice reached SMTP recipients")
		}
		time.Sleep(time.Second)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE identities SET email_verified=true WHERE brand_id=$1 AND id=$2`, br.ID, members[2].IdentityID); err != nil {
		t.Fatal(err)
	}
	verify(users[2]+"@example.test", tenant.BillingRecipientReady)
	control := "recipientcontrol" + suffix
	taxedInvoice(t, creds, accountID, control, "test tax")
	waitRecipientMail(t, control, users[2]+"@example.test", cc)
	if len(recipientMessages(t, blankMarker)) != 0 {
		t.Fatal("blank invoice delivered during later positive control")
	}
	if len(recipientMessages(t, oldMarker)) == 0 {
		t.Fatal("previously delivered message unexpectedly disappeared")
	}
	tn, err = tenant.Get(ctx, db.Pool, sc)
	if err != nil || tn.CreatedByIdentityID != creator {
		t.Fatal("creator provenance changed")
	}
	t.Log("fresh old/named/fallback/restored invoice bodies verified; blank-primary SMTP absence observed for 10 seconds plus subsequent positive control; previous mail retained")
}
