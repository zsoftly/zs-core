package doctor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// BillingRecipientFindings checks actual account primary email against live
// Core selection and warns about unmanaged additional CC recipients. It
// ignores reconciliation backoff and never schedules or
// repairs anything. Neither findings nor upstream errors disclose addresses.
func BillingRecipientFindings(ctx context.Context, db tenant.DB, eng tenant.Engines) ([]Finding, error) {
	var findings []Finding
	after := idgen.Nil
	checked := 0
	for {
		rows, err := db.Query(ctx, `SELECT brand_id, id FROM tenants WHERE id > $1 ORDER BY id LIMIT 100`, after)
		if err != nil {
			return nil, err
		}
		scopes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (scope.Scope, error) {
			var s scope.Scope
			err := row.Scan(&s.Brand, &s.Tenant)
			return s, err
		})
		if err != nil {
			return nil, err
		}
		for _, s := range scopes {
			report, err := tenant.InspectBillingRecipient(ctx, db, eng, s, "")
			id := idgen.Format(idgen.Tenant, s.Tenant)
			if err != nil || !report.AdditionalEmailsChecked {
				findings = append(findings, Finding{"core", "billing_recipients", Fail, "cannot verify primary and additional billing recipients for " + id})
				checked++
				continue
			}
			if report.AdditionalEmails > 0 {
				findings = append(findings, Finding{"core", "billing_recipient_cc", Warn, "unmanaged additional billing recipients exist for account " + report.AccountID + "; Core does not change them"})
			}
			if !report.Matches {
				findings = append(findings, Finding{"core", "billing_recipients", Fail, "billing recipient drift for " + id + "; run the tenant sweeper"})
			} else if report.Status == tenant.BillingRecipientNoEligibleOwner {
				findings = append(findings, Finding{"core", "billing_recipients", Warn, "no eligible billing owner for " + id + "; primary email is clear"})
			}
			checked++
		}
		if len(scopes) < 100 {
			break
		}
		after = scopes[len(scopes)-1].Tenant
	}
	if len(findings) == 0 {
		findings = append(findings, Finding{"core", "billing_recipients", OK, fmt.Sprintf("billing recipients match current eligible owners for %d accounts", checked)})
	}
	return findings, nil
}
