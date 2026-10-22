package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/notifications"
	"github.com/zsoftly/zs-core/pkg/outbox"
)

// newNotificationService shares the database pool and namespaced resolver used
// by brand apply.
func newNotificationService(pool *pgxpool.Pool, resolver secrets.Resolver, log *slog.Logger) *notifications.Service {
	return &notifications.Service{DB: pool, Secrets: resolver, Log: log}
}

func notificationReconciler(svc *notifications.Service) func(context.Context, pgx.Tx, brand.Record, brand.Options) error {
	return func(ctx context.Context, tx pgx.Tx, record brand.Record, opts brand.Options) error {
		_, err := svc.Reconcile(ctx, tx, record, audit.Entry{Actor: opts.Actor, Reason: opts.Reason, Outcome: audit.OutcomeSuccess, CorrelationID: opts.CorrelationID})
		return err
	}
}

func startNotifications(ctx context.Context, pool *pgxpool.Pool, resolver secrets.Resolver, log *slog.Logger) error {
	svc := newNotificationService(pool, resolver, log)
	if err := svc.ValidateEndpoints(ctx); err != nil {
		return err
	}
	go outbox.NewRelay("webhook-fanout", outbox.NewPostgresReader(pool), svc.FanoutHandler(), 0, 0, log).Run(ctx, time.Second)
	go svc.Run(ctx, time.Second)
	log.Info("outbound webhook relays started")
	return nil
}
