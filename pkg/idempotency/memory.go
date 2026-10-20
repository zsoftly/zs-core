package idempotency

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MemoryStore is an in-process Store for tests and single-process tools. It is
// not durable.
type MemoryStore struct {
	mu      sync.Mutex
	records map[Key]Record
	now     func() time.Time
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[Key]Record), now: func() time.Time { return time.Now().UTC() }}
}

// Reserve implements Store.
func (s *MemoryStore) Reserve(_ context.Context, key Key, fingerprint string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if existing, ok := s.records[key]; ok && existing.ExpiresAt.After(now) {
		// A live record blocks the key. A stale pending reservation can be
		// taken over only by the identical request, so a delayed retry with a
		// different payload stays a conflict.
		if existing.Status != StatusPending || existing.LeaseExpiresAt.After(now) || existing.Fingerprint != fingerprint {
			return existing, false, nil
		}
	}
	s.records[key] = Record{
		Fingerprint:    fingerprint,
		Status:         StatusPending,
		CreatedAt:      now,
		LeaseExpiresAt: now.Add(PendingLeaseDuration),
		ExpiresAt:      now.Add(RecordRetention),
	}
	return Record{}, true, nil
}

// Complete implements Store.
func (s *MemoryStore) Complete(_ context.Context, key Key, result Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.records[key]
	if !ok || record.Status != StatusPending {
		return errors.New("idempotency reservation is not pending")
	}
	if len(result.Body) > MaxResultBytes {
		return errors.New("idempotency result exceeds storage limit")
	}
	record.Status = StatusCompleted
	record.Result = Result{Status: result.Status, Body: append([]byte(nil), result.Body...)}
	record.LeaseExpiresAt = time.Time{}
	record.ExpiresAt = s.now().Add(RecordRetention)
	s.records[key] = record
	return nil
}

// Release implements Store.
func (s *MemoryStore) Release(_ context.Context, key Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, key)
	return nil
}
