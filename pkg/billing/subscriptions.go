package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/zsoftly/zs-core/internal/killbill"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Subscription states Kill Bill reports.
const subscriptionActive = "ACTIVE"

// Plan change and cancellation policies.
const (
	PolicyImmediate = "immediate"
	PolicyEndOfTerm = "end_of_term"
)

// Subscription is a Kill Bill subscription of the tenant. ID is the Kill
// Bill subscription ID: subscriptions live in Kill Bill only.
type Subscription struct {
	ID                 string `json:"id"`
	BundleID           string `json:"bundle_id"`
	Product            string `json:"product"`
	Category           string `json:"category"`
	Plan               string `json:"plan"`
	Phase              string `json:"phase"`
	BillingPeriod      string `json:"billing_period"`
	State              string `json:"state"`
	StartDate          string `json:"start_date,omitempty"`
	ChargedThroughDate string `json:"charged_through_date,omitempty"`
	CancelledDate      string `json:"cancelled_date,omitempty"`
	BillingEndDate     string `json:"billing_end_date,omitempty"`
}

func subscriptionOf(s killbill.Subscription) Subscription {
	return Subscription{
		ID: s.SubscriptionID, BundleID: s.BundleID, Product: s.ProductName, Category: s.ProductCategory,
		Plan: s.PlanName, Phase: s.PhaseType, BillingPeriod: s.BillingPeriod, State: s.State,
		StartDate: s.StartDate, ChargedThroughDate: s.ChargedThroughDate, CancelledDate: s.CancelledDate,
		BillingEndDate: s.BillingEndDate,
	}
}

// ListSubscriptions returns every subscription of the tenant.
func (s *Service) ListSubscriptions(ctx context.Context, sc scope.Scope) ([]Subscription, error) {
	a, err := s.account(ctx, sc)
	if err != nil {
		return nil, err
	}
	bundles, err := s.KillBill.AccountBundles(ctx, a.creds, a.id())
	if err != nil {
		return nil, kbError(err)
	}
	out := []Subscription{}
	for _, b := range bundles {
		for _, sub := range b.Subscriptions {
			out = append(out, subscriptionOf(sub))
		}
	}
	return out, nil
}

func checkPlan(plan string) error {
	if plan == "" || len(plan) > 255 || !utf8.ValidString(plan) || strings.ContainsAny(plan, "\x00\r\n\t") {
		return validation("The plan must be a plan name of the catalogue.")
	}
	return nil
}

// derivedKey is a Kill Bill external key derived from the tenant, the
// actor and the caller's idempotency key, so a retry after a lost answer
// finds the object Kill Bill made.
func derivedKey(kind string, a account, actorID, key string) string {
	h := sha256.New()
	for _, part := range []string{kind, a.tenant.ID.String(), actorID, key} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "zs-" + kind + "-" + hex.EncodeToString(h.Sum(nil))[:40]
}

// requireNoHold refuses a change while billing is on hold for the tenant.
func (s *Service) requireNoHold(ctx context.Context, a account) error {
	st, err := s.standing(ctx, a)
	if err != nil {
		return err
	}
	if st.onHold() {
		return zserrors.New(zserrors.CodeWalletOnHold, "Billing changes are on hold for this tenant: "+strings.Join(st.reasons, ", ")+".")
	}
	return nil
}

// idempotent runs fn once per idempotency key of the actor in the tenant,
// stores its result for 24 hours and replays it.
func (s *Service) idempotent(ctx context.Context, sc scope.Scope, actorID, key, operation string, payload any, out any, fn func(context.Context) (any, error)) (bool, error) {
	if err := idempotency.ValidateKey(key); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	fingerprint, err := idempotency.Fingerprint(operation, encoded)
	if err != nil {
		return false, err
	}
	res, replayed, err := idempotency.Do(ctx, idempotency.NewPostgresStore(s.DB), idempotency.Key{Scope: sc, ActorID: actorID, Value: key}, fingerprint,
		func(ctx context.Context) (idempotency.Result, error) {
			v, err := fn(ctx)
			if err != nil {
				return idempotency.Result{}, err
			}
			body, err := json.Marshal(v)
			if err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Status: http.StatusCreated, Body: body}, nil
		})
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return false, zserrors.Wrap(zserrors.CodeInternal, "", err)
	}
	return replayed, nil
}

// CreateSubscription subscribes the tenant to a plan of the catalogue. It is
// idempotent on idempotencyKey for the actor in the tenant: a retry with the
// same plan returns the same subscription, a different plan is
// idempotency_key_conflict. The Kill Bill external key of the subscription
// is derived from the key, so a retry after a lost answer finds the
// subscription instead of creating a second one. It is wallet_on_hold while
// billing changes are on hold.
func (s *Service) CreateSubscription(ctx context.Context, sc scope.Scope, call Call, plan, idempotencyKey string) (Subscription, bool, error) {
	if err := call.validate(); err != nil {
		return Subscription{}, false, err
	}
	if err := checkPlan(plan); err != nil {
		return Subscription{}, false, err
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return Subscription{}, false, err
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return Subscription{}, false, err
	}
	var out Subscription
	replayed, err := s.idempotent(ctx, sc, call.Actor.ID, idempotencyKey, "billing.subscription.create", map[string]string{"plan": plan}, &out,
		func(ctx context.Context) (any, error) {
			externalKey := derivedKey("sub", a, call.Actor.ID, idempotencyKey)
			existing, err := s.KillBill.SubscriptionByExternalKey(ctx, a.creds, externalKey)
			switch {
			case err == nil:
				// An earlier attempt made it and lost the answer.
				if err := owned(a, existing.AccountID); err != nil {
					return nil, err
				}
				return s.auditSubscription(ctx, sc, call, "billing.subscription.created", a, existing, "")
			case !errors.Is(err, killbill.ErrNotFound):
				return nil, kbError(err)
			}
			if err := s.requireNoHold(ctx, a); err != nil {
				return nil, err
			}
			id, err := s.KillBill.CreateSubscription(ctx, a.creds, call.kb(), a.id(), plan, externalKey)
			if err != nil {
				return nil, kbError(err)
			}
			created, err := s.KillBill.Subscription(ctx, a.creds, id)
			if err != nil {
				return nil, kbError(err)
			}
			return s.auditSubscription(ctx, sc, call, "billing.subscription.created", a, created, "")
		})
	return out, replayed, err
}

// auditSubscription audits a subscription write and returns the
// subscription.
func (s *Service) auditSubscription(ctx context.Context, sc scope.Scope, call Call, action string, a account, sub killbill.Subscription, policy string) (Subscription, error) {
	details := map[string]any{"killbill_account_id": a.id(), "killbill_subscription_id": sub.SubscriptionID,
		"killbill_bundle_id": sub.BundleID, "plan": sub.PlanName}
	if policy != "" {
		details["policy"] = policy
	}
	if err := s.audit(ctx, sc, call, action, details); err != nil {
		return Subscription{}, err
	}
	return subscriptionOf(sub), nil
}

// ownedSubscription reads a subscription and refuses one of another
// account.
func (s *Service) ownedSubscription(ctx context.Context, a account, id string) (killbill.Subscription, error) {
	if err := checkKillBillID(id); err != nil {
		return killbill.Subscription{}, err
	}
	sub, err := s.KillBill.Subscription(ctx, a.creds, id)
	if err != nil {
		return killbill.Subscription{}, kbError(err)
	}
	if err := owned(a, sub.AccountID); err != nil {
		return killbill.Subscription{}, err
	}
	return sub, nil
}

func billingPolicy(policy string) (string, error) {
	switch policy {
	case PolicyImmediate:
		return killbill.PolicyImmediate, nil
	case PolicyEndOfTerm:
		return killbill.PolicyEndOfTerm, nil
	}
	return "", validation("The policy must be immediate or end_of_term.")
}

// ChangePlan moves a subscription of the tenant to another plan, with policy
// immediate or end_of_term. It is wallet_on_hold while billing changes are on
// hold. The catalogue's change rules apply in Kill Bill.
func (s *Service) ChangePlan(ctx context.Context, sc scope.Scope, call Call, subscriptionID, plan, policy string) (Subscription, error) {
	if err := call.validate(); err != nil {
		return Subscription{}, err
	}
	if err := checkPlan(plan); err != nil {
		return Subscription{}, err
	}
	kbPolicy, err := billingPolicy(policy)
	if err != nil {
		return Subscription{}, err
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return Subscription{}, err
	}
	if _, err := s.ownedSubscription(ctx, a, subscriptionID); err != nil {
		return Subscription{}, err
	}
	if err := s.requireNoHold(ctx, a); err != nil {
		return Subscription{}, err
	}
	if err := s.KillBill.ChangePlan(ctx, a.creds, call.kb(), subscriptionID, plan, kbPolicy); err != nil {
		return Subscription{}, kbError(err)
	}
	sub, err := s.KillBill.Subscription(ctx, a.creds, subscriptionID)
	if err != nil {
		return Subscription{}, kbError(err)
	}
	if err := s.audit(ctx, sc, call, "billing.subscription.plan_changed", map[string]any{"killbill_account_id": a.id(),
		"killbill_subscription_id": subscriptionID, "plan": plan, "policy": policy}); err != nil {
		return Subscription{}, err
	}
	return subscriptionOf(sub), nil
}

// CancelSubscription cancels a subscription of the tenant, with policy
// immediate or end_of_term.
func (s *Service) CancelSubscription(ctx context.Context, sc scope.Scope, call Call, subscriptionID, policy string) (Subscription, error) {
	if err := call.validate(); err != nil {
		return Subscription{}, err
	}
	kbPolicy, err := billingPolicy(policy)
	if err != nil {
		return Subscription{}, err
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return Subscription{}, err
	}
	if _, err := s.ownedSubscription(ctx, a, subscriptionID); err != nil {
		return Subscription{}, err
	}
	if err := s.KillBill.CancelSubscription(ctx, a.creds, call.kb(), subscriptionID, kbPolicy); err != nil {
		return Subscription{}, kbError(err)
	}
	sub, err := s.KillBill.Subscription(ctx, a.creds, subscriptionID)
	if err != nil {
		return Subscription{}, kbError(err)
	}
	return s.auditSubscription(ctx, sc, call, "billing.subscription.cancelled", a, sub, policy)
}
