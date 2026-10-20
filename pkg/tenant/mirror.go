package tenant

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// The Keycloak Organization mirror. A membership or tenant change writes a
// MirrorEventType event to the outbox in its own transaction. Mirror relays
// claim those events and correct the tenant's Organization towards Core with
// SyncOrganization. At most MirrorWorkers events are in flight per Core
// instance, whatever the rate of writes. A failed event is retried with the
// outbox backoff and the sweeper corrects whatever the mirror misses.
const (
	MirrorEventType = "tenant.mirror"
	// MirrorWorkers is how many mirror relays one Core instance runs, so
	// how many Organization updates it has in flight at most.
	MirrorWorkers = 4
	// MirrorBatch is how many events one relay claims at a time.
	MirrorBatch = 10
	// MirrorInterval is how often an idle relay looks for events.
	MirrorInterval = time.Second
	// mirrorTimeout bounds one Organization update.
	mirrorTimeout = 30 * time.Second
)

var (
	producerOnce sync.Once
	producer     events.Producer
)

func coreProducer() events.Producer {
	producerOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil || host == "" || len(host) > 160 {
			host = "zs-core"
		}
		producer = events.Producer{Service: "zs-core", Instance: host}
	})
	return producer
}

// EnqueueMirror writes a MirrorEventType event for the tenant of s in tx, so
// it commits with the change that needs the Organization updated.
func EnqueueMirror(ctx context.Context, tx pgx.Tx, s scope.Scope, actor events.Actor, correlationID string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if correlationID == "" {
		correlationID = idgen.MustNewV7().String()
	}
	env, err := events.New(ctx, events.Params{
		Type: MirrorEventType, SchemaVersion: 1, Producer: coreProducer(), Actor: actor,
		Brand: s.Brand, Tenant: s.Tenant,
		Aggregate:     events.Aggregate{Type: "tenant", ID: idgen.Format(idgen.Tenant, s.Tenant)},
		CorrelationID: correlationID, Payload: nil,
	})
	if err != nil {
		return err
	}
	if err := outbox.Write(ctx, tx, env); err != nil {
		return unavailable(err)
	}
	return nil
}

// MirrorHandler returns the outbox handler of MirrorEventType events. It
// reads the tenant and runs SyncOrganization. A tenant that no longer exists
// needs nothing. Without the engines the event waits.
func MirrorHandler(db DB, eng Engines) outbox.Handler {
	return outbox.HandlerFunc(func(ctx context.Context, env events.Envelope) error {
		if env.Type != MirrorEventType {
			return outbox.ErrDeliveryTerminal
		}
		if !eng.Ready() {
			return outbox.ErrDeliveryDeferred
		}
		ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
		defer cancel()
		t, err := Get(ctx, db, scope.Scope{Brand: env.Brand(), Tenant: env.Tenant()})
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = SyncOrganization(ctx, db, eng, t, env.CorrelationID)
		return err
	})
}

// MirrorDB is what the mirror relays need: transactions for the handler and
// plain statements for the outbox reader.
type MirrorDB interface {
	DB
	outbox.DB
}

// RunMirror runs MirrorWorkers relays of MirrorEventType events until ctx
// ends, and returns when they have stopped. Each relay handles one event at
// a time.
func RunMirror(ctx context.Context, db MirrorDB, eng Engines, log *slog.Logger) {
	reader := outbox.NewPostgresReader(db).WithEventTypes(MirrorEventType)
	handler := MirrorHandler(db, eng)
	var wg sync.WaitGroup
	for range MirrorWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outbox.NewRelay("tenant-mirror", reader, handler, MirrorBatch, 0, log).Run(ctx, MirrorInterval)
		}()
	}
	wg.Wait()
}
