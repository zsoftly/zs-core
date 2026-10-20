package idempotency

import (
	"bytes"
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-process Store for tests and single-process tools. It is
// not durable.
type MemoryStore struct {
	mu      sync.Mutex
	records map[Key]memoryRecord
	now     func() time.Time
}

type memoryRecord struct {
	Record
	owner []byte
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[Key]memoryRecord), now: func() time.Time { return time.Now().UTC() }}
}

// Reserve implements Store.
func (s *MemoryStore) Reserve(_ context.Context, key Key, fingerprint string) (Reservation, error) {
	owner, err := NewOwnerToken()
	if err != nil {
		return Reservation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if existing, ok := s.records[key]; ok && existing.ExpiresAt.After(now) {
		// A live record blocks the key. A stale pending reservation can be
		// taken over only by the identical request, so a delayed retry with a
		// different payload stays a conflict.
		if existing.Status != StatusPending || existing.LeaseExpiresAt.After(now) || existing.Fingerprint != fingerprint {
			return Reservation{Existing: existing.Record}, nil
		}
	}
	lease := now.Add(PendingLeaseDuration)
	s.records[key] = memoryRecord{
		Record: Record{
			Fingerprint:    fingerprint,
			Status:         StatusPending,
			CreatedAt:      now,
			LeaseExpiresAt: lease,
			ExpiresAt:      now.Add(RecordRetention),
		},
		owner: owner,
	}
	return Reservation{Reserved: true, OwnerToken: owner, LeaseExpiresAt: lease}, nil
}

// held returns the pending record of key when owner holds it.
func (s *MemoryStore) held(key Key, owner []byte) (memoryRecord, bool) {
	record, ok := s.records[key]
	if !ok || record.Status != StatusPending || !bytes.Equal(record.owner, owner) {
		return memoryRecord{}, false
	}
	return record, true
}

// Complete implements Store.
func (s *MemoryStore) Complete(_ context.Context, key Key, owner []byte, result Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.held(key, owner)
	if !ok {
		return ErrNotOwner
	}
	if len(result.Body) > MaxResultBytes {
		return errorResultTooLarge
	}
	record.Status = StatusCompleted
	record.Result = Result{Status: result.Status, Body: append([]byte(nil), result.Body...)}
	record.LeaseExpiresAt = time.Time{}
	record.ExpiresAt = s.now().Add(RecordRetention)
	s.records[key] = record
	return nil
}

// MarkUnstored implements Store.
func (s *MemoryStore) MarkUnstored(_ context.Context, key Key, owner []byte, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.held(key, owner)
	if !ok {
		return ErrNotOwner
	}
	record.Status = StatusCompletedUnstored
	record.LeaseExpiresAt = time.Time{}
	record.ExpiresAt = s.now().Add(RecordRetention)
	s.records[key] = record
	return nil
}

// Release implements Store.
func (s *MemoryStore) Release(_ context.Context, key Key, owner []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held(key, owner); ok {
		delete(s.records, key)
	}
	return nil
}
