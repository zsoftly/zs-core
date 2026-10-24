package tenant

import (
	"context"
	"time"

	"github.com/zsoftly/zs-core/internal/killbill"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ClosureDrain says whether the usage of a closing tenant has all been
// forwarded to Kill Bill, so that its final invoice bills it. It is a seam so
// that the tenant package does not import the metering package that imports it.
type ClosureDrain interface {
	Drained(ctx context.Context, sc scope.Scope) (bool, error)
}

// EngineClosureSteps are the ClosureSteps that act on Kill Bill: the
// cancellation of every subscription of the tenant before finance, and the
// blocking of its account after it. Both read what Kill Bill has before they
// write, and read it again after, so each is idempotent and a call whose
// answer was lost is settled by what is there and never repeated blindly.
type EngineClosureSteps struct {
	Engines Engines
	// Drain is asked once the cancellation is done. Without one the usage is
	// never reported drained, and the closure waits.
	Drain ClosureDrain
	// Today returns today as YYYY-MM-DD in UTC. It defaults to the clock.
	Today func() string
}

func (c EngineClosureSteps) today() string {
	if c.Today != nil {
		return c.Today()
	}
	return time.Now().UTC().Format(time.DateOnly)
}

// errClosureCancelReview says a subscription cannot be cancelled by the
// closure and needs a person.
func errClosureCancelReview() error {
	return zserrors.New(zserrors.CodeConflict, "A subscription of the closing tenant cannot be cancelled without a person.")
}

func cancellableState(state string) bool {
	return state == "ACTIVE" || state == "BLOCKED" || state == "PENDING"
}

// cancelNeedsAPerson reports whether a receipt lists a subscription that
// cancelling it again cannot fix.
func cancelNeedsAPerson(r killbill.CancelReceipt) bool {
	for _, f := range r.Failing {
		if !cancellableState(f.State) {
			return true
		}
	}
	return false
}

// Prepare cancels, immediately, every subscription of the tenant that is not
// already cancelled: the base subscriptions first, which take their add-ons
// with them, then any add-on that is left. It reads the account again and
// answers Cancelled only when every subscription is CANCELLED with a day that
// is not after today. A cancel whose outcome is unknown is not repeated: the
// read after it settles it, and what is still missing is cancelled by the next
// call, which reads first. It then asks the drain. The target date is today.
func (c EngineClosureSteps) Prepare(ctx context.Context, db DB, sc scope.Scope) (ClosurePrepared, error) {
	t, err := Get(ctx, db, sc)
	if err != nil {
		return ClosurePrepared{}, err
	}
	if t.Status != StatusClosing || t.KillBillAccountID == "" {
		return ClosurePrepared{}, zserrors.New(zserrors.CodeNotReady, "The tenant is not closing.")
	}
	be, err := c.Engines.forBillingBrand(ctx, db, sc.Brand)
	if err != nil {
		return ClosurePrepared{}, err
	}
	kb, account, today := c.Engines.KillBill, t.KillBillAccountID, c.today()
	cc := callContext("closure:"+sc.TenantID(), "tenant closure")
	read := func() (killbill.CancelReceipt, error) {
		bundles, err := kb.AccountBundles(ctx, be.creds, account)
		if err != nil {
			return killbill.CancelReceipt{}, engineError(err)
		}
		return killbill.CancelledOn(bundles, account, today), nil
	}
	receipt, err := read()
	if err != nil {
		return ClosurePrepared{}, err
	}
	var firstErr error
	notFound := false
	cancel := func(addOns bool) {
		for _, f := range receipt.Failing {
			if !cancellableState(f.State) || (f.Category == "ADD_ON") != addOns {
				continue
			}
			res, err := kb.CancelSubscriptionOnce(ctx, be.creds, cc, account, f.SubscriptionID, killbill.CancelHow{Policy: killbill.PolicyImmediate})
			switch res.Outcome {
			case killbill.Cancelled, killbill.CancelRefused:
			case killbill.CancelNotFound:
				notFound = true
			default:
				if err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	if !receipt.OK() {
		cancel(false)
		if receipt, err = read(); err != nil {
			return ClosurePrepared{}, err
		}
		// The add-ons that are left are cancelled only if the base cancels did
		// not fail: after a failure the next call reads first and goes on.
		if !receipt.OK() && firstErr == nil && !notFound {
			cancel(true)
			if receipt, err = read(); err != nil {
				return ClosurePrepared{}, err
			}
		}
	}
	prepared := ClosurePrepared{}
	if !receipt.OK() {
		switch {
		case notFound || cancelNeedsAPerson(receipt):
			return prepared, errClosureCancelReview()
		case firstErr != nil:
			return prepared, engineError(firstErr)
		}
		return prepared, nil
	}
	prepared.Cancelled = true
	day, err := time.Parse(time.DateOnly, today)
	if err != nil {
		return ClosurePrepared{}, zserrors.Wrap(zserrors.CodeInternal, "", err)
	}
	prepared.TargetDate = day
	if c.Drain != nil {
		if prepared.Drained, err = c.Drain.Drained(ctx, sc); err != nil {
			return ClosurePrepared{}, engineError(err)
		}
	}
	return prepared, nil
}

// closedBlock is the blocking state of a closed tenant. It stops new
// entitlements and changes and leaves billing alone, so the final invoice and
// any balance still owed stay collectible, which closing the account would
// write off.
var closedBlock = killbill.BlockingState{
	StateName: BlockingClosed, Service: BlockingService,
	IsBlockChange: true, IsBlockEntitlement: true, IsBlockBilling: false,
}

func closedBlockInForce(states []killbill.BlockingState) bool {
	for i := len(states) - 1; i >= 0; i-- {
		if states[i].Service != BlockingService {
			continue
		}
		s := states[i]
		return s.StateName == closedBlock.StateName && s.IsBlockChange == closedBlock.IsBlockChange &&
			s.IsBlockEntitlement == closedBlock.IsBlockEntitlement && s.IsBlockBilling == closedBlock.IsBlockBilling
	}
	return false
}

// Block puts the account of the closing tenant under the closed blocking state,
// unless it already is, and answers true only when it has read that state back
// as the latest of the service. A write whose outcome is unknown is not
// repeated in the same call: the read settles it, and if the state is not
// there the call returns the error and the next one writes it.
func (c EngineClosureSteps) Block(ctx context.Context, db DB, sc scope.Scope) (bool, error) {
	t, err := Get(ctx, db, sc)
	if err != nil {
		return false, err
	}
	if t.Status != StatusClosing || t.KillBillAccountID == "" {
		return false, zserrors.New(zserrors.CodeNotReady, "The tenant is not closing.")
	}
	be, err := c.Engines.forBillingBrand(ctx, db, sc.Brand)
	if err != nil {
		return false, err
	}
	kb, account := c.Engines.KillBill, t.KillBillAccountID
	states, err := kb.AccountBlockingStates(ctx, be.creds, account)
	if err != nil {
		return false, engineError(err)
	}
	if closedBlockInForce(states) {
		return true, nil
	}
	writeErr := kb.SetAccountBlockingState(ctx, be.creds, callContext("closure:"+sc.TenantID(), "tenant closure"), account, closedBlock)
	if states, err = kb.AccountBlockingStates(ctx, be.creds, account); err != nil {
		return false, engineError(err)
	}
	if closedBlockInForce(states) {
		return true, nil
	}
	if writeErr != nil {
		return false, engineError(writeErr)
	}
	return false, nil
}
