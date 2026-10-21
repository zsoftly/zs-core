//go:build integration

package tenant_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func TestBillingRecipientDoctorWarnsOnCCWithoutDisclosureOrRepair(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cc, drift bool
	}{
		{name: "empty"}, {name: "cc", cc: true}, {name: "cc and primary drift", cc: true, drift: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			p := e.person("sensitive-owner@example.test")
			tn := e.mustOrg(p, "Doctor")
			e.recipient(tn, p.Email, tenant.BillingRecipientReady)
			if tc.cc && !e.fake.SetAdditionalAccountEmails(e.brand.Slug, tn.PublicID(), []string{"sensitive-cc@example.test"}) {
				t.Fatal("missing account")
			}
			if tc.drift {
				if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET email='changed-owner@example.test' WHERE id=$1`, p.Identity.ID); err != nil {
					t.Fatal(err)
				}
			}
			var before string
			if err := e.db.Pool.QueryRow(ctx, `SELECT row_to_json(r)::text FROM tenant_billing_recipients r WHERE tenant_id=$1`, tn.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			writes := len(e.fake.Writes())
			findings, err := doctor.BillingRecipientFindings(ctx, e.db.Pool, e.eng)
			if err != nil {
				t.Fatal(err)
			}
			var warned, failed bool
			for _, f := range findings {
				if f.Check == "billing_recipient_cc" && f.Status == doctor.Warn {
					warned = true
					if !strings.Contains(f.Message, tn.KillBillAccountID) {
						t.Fatal("warning omitted account")
					}
				}
				if f.Status == doctor.Fail {
					failed = true
				}
			}
			if warned != tc.cc || failed != tc.drift {
				t.Fatalf("wrong findings: %+v", findings)
			}
			if !tc.cc && !tc.drift && (len(findings) != 1 || findings[0].Status != doctor.OK) {
				t.Fatalf("empty control: %+v", findings)
			}
			report := doctor.Report{Findings: findings}
			var text, encoded bytes.Buffer
			if err := report.WriteText(&text); err != nil {
				t.Fatal(err)
			}
			if err := report.WriteJSON(&encoded); err != nil {
				t.Fatal(err)
			}
			for _, out := range []string{text.String(), encoded.String()} {
				if strings.Contains(out, "@") || strings.Contains(out, "sensitive-") || strings.Contains(out, "kb-secret") {
					t.Fatal("doctor disclosed recipient or credential")
				}
			}
			var after string
			if err := e.db.Pool.QueryRow(ctx, `SELECT row_to_json(r)::text FROM tenant_billing_recipients r WHERE tenant_id=$1`, tn.ID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before || len(e.fake.Writes()) != writes {
				t.Fatal("doctor repaired state")
			}
		})
	}
}

func TestBillingRecipientDoctorFailsClosedOnCCRead(t *testing.T) {
	for _, body := range []string{"null", `{}`, `[{"accountId":"wrong","email":"private@example.test"}]`, "unavailable"} {
		t.Run(body, func(t *testing.T) {
			e := newEnv(t)
			owner := e.person("private-owner@example.test")
			tn := e.mustOrg(owner, "Failed inspection")
			var mutations atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if r.Header.Get(killbill.HeaderAPIKey) != e.brand.Slug || r.Header.Get(killbill.HeaderAPISecret) != "kb-secret-"+e.brand.Slug {
					t.Error("wrong credential scope")
				}
				if strings.HasSuffix(r.URL.Path, "/emails") {
					if r.URL.Path != "/1.0/kb/accounts/"+tn.KillBillAccountID+"/emails" {
						t.Error("wrong account collection")
					}
					if body == "unavailable" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					_, _ = w.Write([]byte(body))
					return
				}
				_ = json.NewEncoder(w).Encode(killbill.Account{AccountID: tn.KillBillAccountID, ExternalKey: tn.PublicID(), Email: owner.Email})
			}))
			t.Cleanup(srv.Close)
			kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
			if err != nil {
				t.Fatal(err)
			}
			e.eng.KillBill = kb
			r, err := tenant.InspectBillingRecipient(context.Background(), e.db.Pool, e.eng, tn.Scope(), "")
			if err == nil || r.Matches || r.AdditionalEmailsChecked {
				t.Fatalf("failed read claimed absence: %+v %v", r, err)
			}
			fs, err := doctor.BillingRecipientFindings(context.Background(), e.db.Pool, e.eng)
			if err != nil || len(fs) != 1 || fs[0].Status != doctor.Fail || mutations.Load() != 0 {
				t.Fatalf("failed read all-clear or mutation: %+v %v", fs, err)
			}
		})
	}
}
