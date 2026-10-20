// Package errors defines the stable ZS error codes, the typed application
// error and the JSON error envelope that every ZSoftly Core API returns.
//
// Each Code has a fixed public name in snake_case, an HTTP status and a
// retryable flag. Clients branch on the name. The ZS-xxxx code is the stable
// internal classification that appears in logs and documentation. The wire
// message is always the safe message of the error; the wrapped cause, which
// may hold driver or provider text, never leaves the process.
package errors

import (
	stderrors "errors"
	"fmt"
	"net/http"
)

// Code is a stable ZS error code such as "ZS-1001".
type Code string

// Definition describes one error code.
type Definition struct {
	Code      Code
	Name      string
	Status    int
	Retryable bool
	Message   string
}

// The error code ranges are:
//
//	ZS-1000..1099 request and protocol
//	ZS-1100..1199 dependencies
//	ZS-1200..1299 idempotency
//	ZS-1300..1399 pagination
//	ZS-1400..1499 scope and authentication
//	ZS-1500..1599 brands
//	ZS-1600..1699 tenants, memberships, teams and invitations
//	ZS-1700..1799 API keys
//	ZS-1800..1899 billing
//	ZS-1900..1999 wallet, metering and quotas
const (
	CodeInternal             Code = "ZS-1000"
	CodeValidation           Code = "ZS-1001"
	CodeBadRequest           Code = "ZS-1002"
	CodeNotFound             Code = "ZS-1003"
	CodeConflict             Code = "ZS-1004"
	CodeForbidden            Code = "ZS-1005"
	CodeUnauthenticated      Code = "ZS-1006"
	CodeRateLimited          Code = "ZS-1007"
	CodeMethodNotAllowed     Code = "ZS-1008"
	CodePayloadTooLarge      Code = "ZS-1009"
	CodeUnsupportedMediaType Code = "ZS-1010"
	CodeReasonRequired       Code = "ZS-1011"

	CodeDatabaseUnavailable         Code = "ZS-1100"
	CodeNotReady                    Code = "ZS-1101"
	CodeIdentityProviderUnavailable Code = "ZS-1102"
	CodeBillingUnavailable          Code = "ZS-1103"

	CodeIdempotencyKeyInvalid     Code = "ZS-1200"
	CodeIdempotencyKeyConflict    Code = "ZS-1201"
	CodeIdempotencyKeyInProgress  Code = "ZS-1202"
	CodeIdempotencyResultTooLarge Code = "ZS-1203"

	CodeInvalidCursor Code = "ZS-1300"
	CodeInvalidLimit  Code = "ZS-1301"

	CodeScopeRequired             Code = "ZS-1400"
	CodeScopeInvalid              Code = "ZS-1401"
	CodeTokenInvalid              Code = "ZS-1402"
	CodeTokenAudienceUnknown      Code = "ZS-1403"
	CodeAuthenticationUnavailable Code = "ZS-1404"
	CodeEmailNotVerified          Code = "ZS-1405"
	CodeRecentMFARequired         Code = "ZS-1406"
	CodeMFARequired               Code = "ZS-1407"

	CodeBrandManifestInvalid Code = "ZS-1500"
	CodeBrandApplyRejected   Code = "ZS-1501"
	CodeBrandApplyInProgress Code = "ZS-1502"
	CodeSecretUnresolved     Code = "ZS-1503"
	CodeSecretRefInvalid     Code = "ZS-1504"

	CodeTenantNotFound          Code = "ZS-1600"
	CodePermissionDenied        Code = "ZS-1601"
	CodeLastOwner               Code = "ZS-1602"
	CodeInvitationNotFound      Code = "ZS-1603"
	CodeInvitationEmailMismatch Code = "ZS-1604"
	CodePermissionUnknown       Code = "ZS-1605"
	CodeTeamLocked              Code = "ZS-1606"
	CodeAlreadyMember           Code = "ZS-1607"
	CodeTenantKindUnsupported   Code = "ZS-1608"
	CodeRecentAuthRequired      Code = "ZS-1609"
	CodeTenantClosed            Code = "ZS-1610"

	CodeAPIKeyInvalid      Code = "ZS-1700"
	CodeAPIKeyOwnerRemoved Code = "ZS-1701"

	CodeBillingRejected       Code = "ZS-1800"
	CodeInvoicePDFUnavailable Code = "ZS-1801"
	CodePluginUnavailable     Code = "ZS-1802"
	CodeCreditCodeInvalid     Code = "ZS-1803"
	CodeCreditCodeExhausted   Code = "ZS-1804"
	CodeWalletOnHold          Code = "ZS-1805"
	CodeBillingTokenInvalid   Code = "ZS-1806"

	CodeInsufficientCredit           Code = "ZS-1900"
	CodeReservationNotFound          Code = "ZS-1901"
	CodeReservationNotHeld           Code = "ZS-1902"
	CodeReservationSettlementPending Code = "ZS-1903"
	CodeReservationConflict          Code = "ZS-1904"
	CodeWalletUnavailable            Code = "ZS-1905"
	CodeSettlementExceedsHold        Code = "ZS-1906"
	CodeUnitUnrated                  Code = "ZS-1907"
	CodeReservationClosed            Code = "ZS-1908"
	CodeSettlementConflict           Code = "ZS-1909"
	CodeMeterNotFound                Code = "ZS-1910"
	CodeUsageEventConflict           Code = "ZS-1911"
	CodeUsageOutOfWindow             Code = "ZS-1912"
	CodeUsageOverflow                Code = "ZS-1913"
	CodeQuotaExceeded                Code = "ZS-1920"
	CodeQuotaNotConcurrent           Code = "ZS-1921"
)

var definitions = []Definition{
	{CodeInternal, "internal_error", http.StatusInternalServerError, false, "An internal error occurred."},
	{CodeValidation, "validation_failed", http.StatusBadRequest, false, "The request failed validation."},
	{CodeBadRequest, "bad_request", http.StatusBadRequest, false, "The request is malformed."},
	{CodeNotFound, "not_found", http.StatusNotFound, false, "The resource was not found."},
	{CodeConflict, "conflict", http.StatusConflict, false, "The request conflicts with the current state."},
	{CodeForbidden, "forbidden", http.StatusForbidden, false, "The caller is not allowed to perform this action."},
	{CodeUnauthenticated, "unauthenticated", http.StatusUnauthorized, false, "Authentication is required."},
	{CodeRateLimited, "rate_limited", http.StatusTooManyRequests, true, "Too many requests."},
	{CodeMethodNotAllowed, "method_not_allowed", http.StatusMethodNotAllowed, false, "The method is not allowed for this resource."},
	{CodePayloadTooLarge, "payload_too_large", http.StatusRequestEntityTooLarge, false, "The request body is too large."},
	{CodeUnsupportedMediaType, "unsupported_media_type", http.StatusUnsupportedMediaType, false, "The request content type is not supported."},
	{CodeReasonRequired, "reason_required", http.StatusBadRequest, false, "This change requires a reason."},
	{CodeDatabaseUnavailable, "database_unavailable", http.StatusServiceUnavailable, true, "The database is unavailable."},
	{CodeNotReady, "not_ready", http.StatusServiceUnavailable, true, "The service is not ready."},
	{CodeIdentityProviderUnavailable, "identity_provider_unavailable", http.StatusServiceUnavailable, true, "The identity provider is unavailable."},
	{CodeBillingUnavailable, "billing_unavailable", http.StatusServiceUnavailable, true, "The billing engine is unavailable."},
	{CodeIdempotencyKeyInvalid, "idempotency_key_invalid", http.StatusBadRequest, false, "The idempotency_key must be 8 to 255 printable characters."},
	{CodeIdempotencyKeyConflict, "idempotency_key_conflict", http.StatusConflict, false, "The idempotency_key was already used with a different request."},
	{CodeIdempotencyKeyInProgress, "idempotency_key_in_progress", http.StatusConflict, true, "A request with this idempotency_key is still in progress."},
	{CodeIdempotencyResultTooLarge, "idempotency_result_too_large", http.StatusInternalServerError, false, "The command ran but its result was too large to store; it is not run again for this idempotency_key."},
	{CodeInvalidCursor, "invalid_cursor", http.StatusBadRequest, false, "The cursor is invalid."},
	{CodeInvalidLimit, "invalid_limit", http.StatusBadRequest, false, "The limit is out of range."},
	{CodeScopeRequired, "scope_required", http.StatusBadRequest, false, "A brand and a tenant scope are required."},
	{CodeScopeInvalid, "scope_invalid", http.StatusBadRequest, false, "The brand or tenant scope is invalid."},
	{CodeTokenInvalid, "token_invalid", http.StatusUnauthorized, false, "The bearer token is invalid."},
	{CodeTokenAudienceUnknown, "token_audience_unknown", http.StatusUnauthorized, false, "The token was not issued for a known brand."},
	{CodeAuthenticationUnavailable, "authentication_unavailable", http.StatusServiceUnavailable, true, "Authentication is temporarily unavailable."},
	{CodeEmailNotVerified, "email_not_verified", http.StatusForbidden, false, "A verified email address is required."},
	{CodeRecentMFARequired, "recent_mfa_required", http.StatusForbidden, false, "Sign in again with multi-factor authentication."},
	{CodeMFARequired, "mfa_required", http.StatusForbidden, false, "This brand requires multi-factor authentication for staff. Sign in again with a second factor."},
	{CodeBrandManifestInvalid, "brand_manifest_invalid", http.StatusBadRequest, false, "The brand manifest is invalid."},
	{CodeBrandApplyRejected, "brand_apply_rejected", http.StatusUnprocessableEntity, false, "An engine rejected part of the brand manifest."},
	{CodeBrandApplyInProgress, "brand_apply_in_progress", http.StatusConflict, true, "Another apply of this brand is in progress."},
	{CodeSecretUnresolved, "secret_unresolved", http.StatusUnprocessableEntity, false, "A secret reference in the brand manifest could not be resolved."},
	{CodeSecretRefInvalid, "secret_ref_invalid", http.StatusUnprocessableEntity, false, "A secret reference in the brand manifest is outside the brand's namespace."},
	{CodeTenantNotFound, "tenant_not_found", http.StatusNotFound, false, "The tenant was not found."},
	{CodePermissionDenied, "permission_denied", http.StatusForbidden, false, "The caller does not have the permission this action needs."},
	{CodeLastOwner, "last_owner", http.StatusConflict, false, "The last active owner cannot be removed, suspended or demoted."},
	{CodeInvitationNotFound, "invitation_not_found", http.StatusNotFound, false, "The invitation was not found, has expired or was revoked."},
	{CodeInvitationEmailMismatch, "invitation_email_mismatch", http.StatusForbidden, false, "The invitation was sent to another email address."},
	{CodePermissionUnknown, "permission_unknown", http.StatusBadRequest, false, "A permission is not registered for this brand."},
	{CodeTeamLocked, "team_locked", http.StatusConflict, false, "This team is locked."},
	{CodeAlreadyMember, "already_member", http.StatusConflict, false, "The caller is already a member of this tenant."},
	{CodeTenantKindUnsupported, "tenant_kind_unsupported", http.StatusConflict, false, "This action is not available for this kind of tenant."},
	{CodeRecentAuthRequired, "recent_auth_required", http.StatusForbidden, false, "Sign in again to continue."},
	{CodeTenantClosed, "tenant_closed", http.StatusConflict, false, "The tenant is closed. Closing is final."},
	{CodeAPIKeyInvalid, "api_key_invalid", http.StatusUnauthorized, false, "The API key is invalid, expired or revoked."},
	{CodeAPIKeyOwnerRemoved, "api_key_owner_removed", http.StatusUnauthorized, false, "The member who created this API key is no longer active."},
	{CodeBillingRejected, "billing_rejected", http.StatusUnprocessableEntity, false, "The billing engine rejected the request."},
	{CodeInvoicePDFUnavailable, "invoice_pdf_unavailable", http.StatusNotFound, false, "No invoice template plugin renders PDF invoices for this brand."},
	{CodePluginUnavailable, "plugin_unavailable", http.StatusUnprocessableEntity, false, "The payment plugin cannot be installed or used."},
	{CodeCreditCodeInvalid, "credit_code_invalid", http.StatusNotFound, false, "The code is unknown, disabled or expired."},
	{CodeCreditCodeExhausted, "credit_code_exhausted", http.StatusConflict, false, "The code has no redemptions left."},
	{CodeWalletOnHold, "wallet_on_hold", http.StatusConflict, false, "Billing changes are on hold for this tenant."},
	{CodeBillingTokenInvalid, "billing_token_invalid", http.StatusUnauthorized, false, "The billing credential is invalid or expired."},
	{CodeInsufficientCredit, "insufficient_credit", http.StatusConflict, false, "The tenant does not have enough available credit."},
	{CodeReservationNotFound, "reservation_not_found", http.StatusNotFound, false, "The reservation was not found."},
	{CodeReservationNotHeld, "reservation_not_held", http.StatusConflict, false, "The reservation is no longer held."},
	{CodeReservationSettlementPending, "reservation_settlement_pending", http.StatusConflict, true, "The reservation's settlement is waiting for the billing engine."},
	{CodeReservationConflict, "reservation_conflict", http.StatusConflict, false, "The reservation was already settled differently."},
	{CodeWalletUnavailable, "wallet_unavailable", http.StatusServiceUnavailable, true, "The wallet cannot tell what the tenant has used and not yet been invoiced."},
	{CodeSettlementExceedsHold, "settlement_exceeds_hold", http.StatusConflict, false, "The settlement is worth more than the reservation holds."},
	{CodeUnitUnrated, "unit_unrated", http.StatusUnprocessableEntity, false, "The catalogue has no price for this unit on the subscription's current phase."},
	{CodeReservationClosed, "reservation_closed", http.StatusConflict, false, "The reservation was settled, released or expired."},
	{CodeSettlementConflict, "settlement_conflict", http.StatusConflict, false, "The billing engine already holds a different settlement under this reservation's key."},
	{CodeMeterNotFound, "meter_not_found", http.StatusNotFound, false, "The meter is unknown or retired."},
	{CodeUsageEventConflict, "usage_event_conflict", http.StatusConflict, false, "A usage event with this event_id was recorded with a different payload."},
	{CodeUsageOutOfWindow, "usage_out_of_window", http.StatusBadRequest, false, "The usage event is outside the brand's backfill window."},
	{CodeUsageOverflow, "usage_overflow", http.StatusUnprocessableEntity, false, "The usage quantity would overflow."},
	{CodeQuotaExceeded, "quota_exceeded", http.StatusConflict, false, "The tenant has reached the limit of this feature."},
	{CodeQuotaNotConcurrent, "quota_not_concurrent", http.StatusConflict, false, "A feature whose quota has no window cannot be consumed."},
}

var byCode = func() map[Code]Definition {
	m := make(map[Code]Definition, len(definitions))
	for _, d := range definitions {
		m[d.Code] = d
	}
	return m
}()

// Definitions returns every registered error definition in code order.
func Definitions() []Definition {
	return append([]Definition(nil), definitions...)
}

// Lookup returns the definition of code. Unknown codes resolve to the
// internal error definition.
func Lookup(code Code) Definition {
	if d, ok := byCode[code]; ok {
		return d
	}
	return byCode[CodeInternal]
}

// Error is a safe, typed error that can cross package and transport
// boundaries. Message is safe to show to a caller. The cause is kept for logs
// and errors.Is and errors.As, and is never written to a response.
type Error struct {
	Code    Code
	Message string
	cause   error
}

// New returns an error with a safe message. An empty message uses the default
// message of the code.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap returns an error with a safe message that keeps cause for logging.
func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

// Error returns the code, the safe message and the cause for logs.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.SafeMessage())
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.SafeMessage(), e.cause)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Definition returns the definition of the error code.
func (e *Error) Definition() Definition { return Lookup(e.Code) }

// SafeMessage returns the message that may be written to a response.
func (e *Error) SafeMessage() string {
	if e.Message != "" {
		return e.Message
	}
	return Lookup(e.Code).Message
}

// IsCode reports whether err is or wraps an *Error with the given code.
func IsCode(err error, code Code) bool {
	var zsErr *Error
	if !stderrors.As(err, &zsErr) {
		return false
	}
	return zsErr.Code == code
}

// Body is the inner object of the error envelope.
type Body struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Envelope is the JSON error response: {"error":{"code","message","retryable"}}.
type Envelope struct {
	Error Body `json:"error"`
}

// ToEnvelope maps any error to its HTTP status and envelope. Errors that are
// not an *Error become internal_error with the default message, so no
// unclassified text reaches a caller.
func ToEnvelope(err error) (int, Envelope) {
	var zsErr *Error
	if err == nil || !stderrors.As(err, &zsErr) {
		d := byCode[CodeInternal]
		return d.Status, Envelope{Error: Body{Code: d.Name, Message: d.Message, Retryable: d.Retryable}}
	}
	d := Lookup(zsErr.Code)
	message := zsErr.SafeMessage()
	if _, known := byCode[zsErr.Code]; !known {
		message = d.Message
	}
	return d.Status, Envelope{Error: Body{Code: d.Name, Message: message, Retryable: d.Retryable}}
}
