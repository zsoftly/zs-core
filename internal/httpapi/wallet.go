package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// walletService returns the wallet, or not_ready.
func (s *server) walletService() (*wallet.Service, error) {
	if s.wallet == nil || !s.wallet.Ready() {
		return nil, zserrors.New(zserrors.CodeNotReady, "The wallet needs the Kill Bill admin settings.")
	}
	return s.wallet, nil
}

// tenantWallet is the common start of a wallet route: the permission, the
// scope, the caller and the service.
func (s *server) tenantWallet(w http.ResponseWriter, r *http.Request, permission string) (*wallet.Service, scope.Scope, tenant.Caller, bool) {
	ctx := r.Context()
	if err := scope.Require(ctx, permission); err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, tenant.Caller{}, false
	}
	svc, err := s.walletService()
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, tenant.Caller{}, false
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, tenant.Caller{}, false
	}
	return svc, sc, caller, true
}

// walletCall is the call of a tenant route. The holds it makes belong to
// the caller's API key or membership.
func walletCall(r *http.Request, caller tenant.Caller, reason string) wallet.Call {
	return wallet.Call{Actor: caller.Actor, Reason: strings.TrimSpace(reason), CorrelationID: RequestID(r.Context()), Holder: callerHolder(caller)}
}

// reservationRef reads the reservation reference of the path. A malformed
// one is reservation_not_found.
func reservationRef(r *http.Request) (string, error) {
	ref := r.PathValue("ref")
	if !wallet.ValidRef(ref) {
		return "", zserrors.New(zserrors.CodeReservationNotFound, "")
	}
	return ref, nil
}

func attemptOf(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func leaseOf(p *int) time.Duration {
	if p == nil {
		return 0
	}
	return time.Duration(*p) * time.Second
}

func (s *server) getWallet(w http.ResponseWriter, r *http.Request) {
	svc, sc, _, ok := s.tenantWallet(w, r, "billing:read")
	if !ok {
		return
	}
	v, err := svc.View(r.Context(), sc)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, v, &gen.WalletView{})
}

func (s *server) listReservations(w http.ResponseWriter, r *http.Request) {
	_, sc, _, ok := s.tenantWallet(w, r, "wallet:reserve")
	if !ok {
		return
	}
	q := r.URL.Query()
	page, err := pageParams(q)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ref := q.Get("external_ref")
	if ref != "" && !wallet.ValidRef(ref) {
		s.writeError(w, r, zserrors.New(zserrors.CodeValidation, "The external_ref is malformed."))
		return
	}
	list, next, err := wallet.List(r.Context(), s.db, sc, q.Get("status"), ref, page.Limit, page.Cursor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": list, "next_cursor": nextCursor(next)}, &gen.ReservationPage{})
}

func (s *server) createReservation(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantWallet(w, r, "wallet:reserve")
	if !ok {
		return
	}
	var body gen.ReservationCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	req := wallet.ReserveRequest{ExternalRef: body.ExternalRef, Attempt: attemptOf(body.Attempt), Label: deref(body.Label),
		AmountMinor: body.AmountMinor, Lease: leaseOf(body.LeaseSeconds)}
	if body.Currency != nil {
		req.Currency = *body.Currency
	}
	var (
		res     wallet.Reservation
		created bool
	)
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		res, created, err = svc.Reserve(r.Context(), tx, sc, walletCall(r, caller, ""), req)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeAs(w, r, status, res, &gen.Reservation{})
}

func settlementOf(usage *gen.UsageSettlement, credit *int64) wallet.Settlement {
	var st wallet.Settlement
	if credit != nil {
		st.CreditMinor = *credit
	}
	if u := usage; u != nil {
		st.Usage = &wallet.UsageSettlement{Unit: u.Unit, QuantityMicro: u.QuantityMicro, EventID: deref(u.EventId)}
		if u.Subscription != nil {
			st.Usage.Subscription = *u.Subscription
		}
	}
	return st
}

// settled answers a settlement: 200 when it is settled, 202 while it waits
// for Kill Bill.
func (s *server) settled(w http.ResponseWriter, r *http.Request, res wallet.Reservation, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if res.SettlementPending {
		status = http.StatusAccepted
	}
	s.writeAs(w, r, status, res, &gen.Reservation{})
}

// settleReservation settles in steps of its own: Kill Bill is never called
// while the tenant lock is held.
func (s *server) settleReservation(w http.ResponseWriter, r *http.Request) {
	svc, sc, caller, ok := s.tenantWallet(w, r, "wallet:reserve")
	if !ok {
		return
	}
	ref, err := reservationRef(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.ReservationSettle
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	reason := deref(body.Reason)
	if err := checkReasonLength(reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := svc.SettleNow(r.Context(), sc, walletCall(r, caller, reason), ref, attemptOf(body.Attempt), settlementOf(body.Usage, body.CreditMinor))
	s.settled(w, r, res, err)
}

func (s *server) releaseReservation(w http.ResponseWriter, r *http.Request) {
	s.reservationWrite(w, r, func(ctx context.Context, tx pgx.Tx, svc *wallet.Service, sc scope.Scope, call wallet.Call, ref string) (wallet.Reservation, error) {
		var body gen.ReservationRelease
		if err := decodeBody(r, &body); err != nil {
			return wallet.Reservation{}, err
		}
		return svc.Release(ctx, tx, sc, call, ref, attemptOf(body.Attempt))
	})
}

func (s *server) extendReservation(w http.ResponseWriter, r *http.Request) {
	s.reservationWrite(w, r, func(ctx context.Context, tx pgx.Tx, svc *wallet.Service, sc scope.Scope, call wallet.Call, ref string) (wallet.Reservation, error) {
		var body gen.ReservationExtend
		if err := decodeBody(r, &body); err != nil {
			return wallet.Reservation{}, err
		}
		return svc.Extend(ctx, tx, sc, call, ref, attemptOf(body.Attempt), time.Duration(body.LeaseSeconds)*time.Second)
	})
}

// reservationWrite runs a write on one reservation of the path in a
// transaction and answers the reservation.
func (s *server) reservationWrite(w http.ResponseWriter, r *http.Request, op func(context.Context, pgx.Tx, *wallet.Service, scope.Scope, wallet.Call, string) (wallet.Reservation, error)) {
	svc, sc, caller, ok := s.tenantWallet(w, r, "wallet:reserve")
	if !ok {
		return
	}
	ref, err := reservationRef(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var res wallet.Reservation
	err = s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		res, err = op(r.Context(), tx, svc, sc, walletCall(r, caller, ""), ref)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, res, &gen.Reservation{})
}

// staffWallet is the start of a staff route on a tenant's reservation: the
// support grant, the tenant, the reference and a staff call with the
// reason. Anyone without the grant gets 404.
func (s *server) staffWallet(w http.ResponseWriter, r *http.Request) (*wallet.Service, scope.Scope, string, wallet.Call, bool) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, identity.StaffSupport)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, "", wallet.Call{}, false
	}
	tenantID, err := pathID(r, "id", idgen.Tenant, tenant.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, "", wallet.Call{}, false
	}
	svc, err := s.walletService()
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, "", wallet.Call{}, false
	}
	ref, err := reservationRef(r)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, "", wallet.Call{}, false
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	call := wallet.Call{Actor: events.Actor{Type: events.ActorStaff, ID: actorID}, CorrelationID: RequestID(ctx), Staff: true}
	return svc, scope.Scope{Brand: p.Brand.ID, Tenant: tenantID}, ref, call, true
}

func (s *server) staffSettleReservation(w http.ResponseWriter, r *http.Request) {
	svc, sc, ref, call, ok := s.staffWallet(w, r)
	if !ok {
		return
	}
	var body gen.StaffReservationSettle
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkReasonLength(body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	call.Reason = strings.TrimSpace(body.Reason)
	res, err := svc.SettleNow(r.Context(), sc, call, ref, attemptOf(body.Attempt), settlementOf(body.Usage, body.CreditMinor))
	s.settled(w, r, res, err)
}

func (s *server) staffReleaseReservation(w http.ResponseWriter, r *http.Request) {
	svc, sc, ref, call, ok := s.staffWallet(w, r)
	if !ok {
		return
	}
	var body gen.StaffReservationRelease
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkReasonLength(body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	call.Reason = strings.TrimSpace(body.Reason)
	var res wallet.Reservation
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		res, err = svc.Release(r.Context(), tx, sc, call, ref, attemptOf(body.Attempt))
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, res, &gen.Reservation{})
}

func (s *server) staffExtendReservation(w http.ResponseWriter, r *http.Request) {
	svc, sc, ref, call, ok := s.staffWallet(w, r)
	if !ok {
		return
	}
	var body gen.StaffReservationExtend
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkReasonLength(body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	call.Reason = strings.TrimSpace(body.Reason)
	var res wallet.Reservation
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		res, err = svc.Extend(r.Context(), tx, sc, call, ref, attemptOf(body.Attempt), time.Duration(body.LeaseSeconds)*time.Second)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, res, &gen.Reservation{})
}
