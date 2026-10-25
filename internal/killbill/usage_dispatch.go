package killbill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// usageAmount is the amount Kill Bill 0.24.22 stores: a non-negative decimal
// with at most nine digits after the point, written as a JSON number.
var usageAmount = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,9})?$`)

func (r SubscriptionUsageRecord) validate() error {
	if r.SubscriptionID == "" || r.TrackingID == "" {
		return errors.New("killbill usage record: subscription ID and tracking ID are required")
	}
	if len(r.UnitUsageRecords) == 0 {
		return errors.New("killbill usage record: at least one unit is required")
	}
	for _, unit := range r.UnitUsageRecords {
		if unit.UnitType == "" || len(unit.UsageRecords) == 0 {
			return errors.New("killbill usage record: a unit needs a type and at least one record")
		}
		for _, rec := range unit.UsageRecords {
			amount := string(rec.Amount)
			if rec.RecordDate == "" || !usageAmount.MatchString(amount) || strings.Trim(amount, "0.") == "" {
				return errors.New("killbill usage record: a record needs a date and a positive amount of at most nine decimals")
			}
		}
	}
	return nil
}

// Payload returns the JSON body of POST /1.0/kb/usages for the record. It is
// deterministic, so a caller can store it before sending and send exactly
// those bytes with SendUsage.
func (r SubscriptionUsageRecord) Payload() ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// ParseUsagePayload decodes a payload built by Payload, or the same JSON read
// back from a store that reorders keys. Unknown fields, trailing data and an
// incomplete record are refused.
func ParseUsagePayload(raw []byte) (SubscriptionUsageRecord, error) {
	var rec SubscriptionUsageRecord
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return SubscriptionUsageRecord{}, errors.New("killbill usage payload: not a usage record")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SubscriptionUsageRecord{}, errors.New("killbill usage payload: trailing data")
	}
	if err := rec.validate(); err != nil {
		return SubscriptionUsageRecord{}, err
	}
	return rec, nil
}

// EndpointFingerprint identifies the Kill Bill the client talks to: the
// lower-case hex SHA-256 of its scheme, host, port and path. Credentials in
// the URL are not part of it, and a default port is the same as none.
func (c *Client) EndpointFingerprint() string {
	u, err := url.Parse(c.base)
	if err != nil {
		u = &url.URL{Host: c.base}
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	host := strings.ToLower(u.Hostname())
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	id := "zs-core/killbill-endpoint/v1\n" + strings.ToLower(u.Scheme) + "://" + host + strings.TrimRight(u.Path, "/")
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// UsageTarget is where a usage record for a subscription lands.
type UsageTarget struct {
	EndpointFingerprint string
	TenantID            string
	AccountID           string
}

// UsageTarget returns the Kill Bill endpoint, tenant and account that
// usage for the subscription would land in. The tenant comes from the admin
// lookup of the API key; the account is the owner of the subscription as
// the tenant sees it, so a subscription of another tenant is ErrNotFound.
func (c *Client) UsageTarget(ctx context.Context, creds TenantCredentials, subscriptionID string) (UsageTarget, error) {
	tenant, err := c.TenantByAPIKey(ctx, creds.APIKey)
	if err != nil {
		return UsageTarget{}, err
	}
	sub, err := c.Subscription(ctx, creds, subscriptionID)
	if err != nil {
		return UsageTarget{}, err
	}
	if tenant.TenantID == "" || sub.AccountID == "" {
		return UsageTarget{}, errors.New("killbill usage target: unexpected response")
	}
	return UsageTarget{EndpointFingerprint: c.EndpointFingerprint(), TenantID: tenant.TenantID, AccountID: sub.AccountID}, nil
}

// UsageSendOutcome is what a usage POST proves about the record. The zero
// value is UsageSendUnknown, so an unset outcome is never read as safe to
// send again.
type UsageSendOutcome int

const (
	// UsageSendUnknown: Kill Bill may or may not hold the record. A
	// timeout, a connection that broke after the request was written, a
	// server error, 408, 429 or a redirect.
	UsageSendUnknown UsageSendOutcome = iota
	// UsageSent: Kill Bill answered 2xx and recorded the usage.
	UsageSent
	// UsageHeld: Kill Bill refused the tracking ID because it already holds
	// it for the subscription (HTTP 400, error code 50000). Kill Bill
	// compares the tracking ID only, not the payload.
	UsageHeld
	// UsageNotSent: Kill Bill recorded nothing. The request never left
	// (a refused or unresolved connection, an untrusted certificate, a
	// cancelled context, a payload that is not a usage record) or Kill Bill
	// rejected it with another 4xx.
	UsageNotSent
)

// UsageSendResult is the outcome of SendUsage and the HTTP status behind it,
// 0 when Kill Bill did not answer.
type UsageSendResult struct {
	Outcome UsageSendOutcome
	Status  int
}

// SendUsage posts payload, as built by Payload, to POST /1.0/kb/usages
// byte for byte, and classifies what the answer proves. The error is nil for
// UsageSent and UsageHeld and describes the failure otherwise; it never
// carries credentials or Kill Bill's message.
func (c *Client) SendUsage(ctx context.Context, creds TenantCredentials, cc CallContext, payload []byte) (UsageSendResult, error) {
	if _, err := ParseUsagePayload(payload); err != nil {
		return UsageSendResult{Outcome: UsageNotSent}, err
	}
	if cc.CreatedBy == "" {
		return UsageSendResult{Outcome: UsageNotSent}, errors.New("killbill record usage: CreatedBy is required on writes")
	}
	if err := ctx.Err(); err != nil {
		return UsageSendResult{Outcome: UsageNotSent}, fmt.Errorf("killbill record usage: %w", err)
	}
	resp, err := c.do(ctx, request{op: "record usage", method: http.MethodPost, path: "/1.0/kb/usages",
		tenant: &creds, call: &cc, contentType: "application/json", body: payload})
	res := UsageSendResult{Status: resp.Status}
	if err == nil {
		res.Outcome = UsageSent
		return res, nil
	}
	var kbErr *Error
	if errors.As(err, &kbErr) && kbErr.Status == http.StatusBadRequest && kbErr.Code == trackingIDExistsCode {
		res.Outcome = UsageHeld
		return res, nil
	}
	switch {
	case resp.Status == 0:
		// No answer. Only a failure before the request was written proves
		// Kill Bill saw nothing.
		if neverWritten(err) {
			res.Outcome = UsageNotSent
		}
	case resp.Status >= 400 && resp.Status < 500 && resp.Status != http.StatusRequestTimeout && resp.Status != http.StatusTooManyRequests:
		res.Outcome = UsageNotSent
	}
	return res, err
}

// neverWritten reports a transport error that happened before any byte of the
// request was written: the connection was not made, or the server's
// certificate was refused.
func neverWritten(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var cert *tls.CertificateVerificationError
	return errors.As(err, &cert)
}
