package store

import (
	"context"
	"sync/atomic"

	"github.com/google/uuid"

	"focuslab/internal/domain"
)

// FaultTx wraps a Tx and fails a chosen operation to exercise rollback paths.
// It is used by fault-injection tests to prove that when a mutation fails
// mid-transaction, nothing the transaction did becomes visible.
type FaultTx struct {
	Tx
	// FailInsertAfter causes InsertEventIfAbsent to return ErrInjected once the
	// counter of successful inserts reaches this value (>0 to enable).
	FailInsertAfter int64
	// FailSaveResult forces SaveResult to fail.
	FailSaveResult bool
	inserts        int64
}

// NewFaultStore wraps a Store so that WithTx hands fn a FaultTx configured by
// the provided template. Each transaction gets a fresh counter.
func NewFaultStore(inner Store, cfg FaultConfig) *FaultStore {
	return &FaultStore{inner: inner, cfg: cfg}
}

// FaultConfig configures injected faults.
type FaultConfig struct {
	FailInsertAfter int64
	FailSaveResult  bool
}

// FaultStore is a Store decorator that injects faults into transactions.
type FaultStore struct {
	inner Store
	cfg   FaultConfig
}

func (f *FaultStore) WithTx(ctx context.Context, fn func(tx Tx) error) error {
	return f.inner.WithTx(ctx, func(tx Tx) error {
		ft := &FaultTx{Tx: tx, FailInsertAfter: f.cfg.FailInsertAfter, FailSaveResult: f.cfg.FailSaveResult}
		return fn(ft)
	})
}

func (f *FaultStore) PutKey(id uuid.UUID, k []byte) error { return f.inner.PutKey(id, k) }
func (f *FaultStore) GetKey(id uuid.UUID) ([]byte, error) { return f.inner.GetKey(id) }
func (f *FaultStore) DestroyKey(id uuid.UUID) error       { return f.inner.DestroyKey(id) }

func (t *FaultTx) InsertEventIfAbsent(ctx context.Context, e domain.Event) (bool, error) {
	if t.FailInsertAfter > 0 && atomic.LoadInt64(&t.inserts) >= t.FailInsertAfter {
		return false, ErrInjected
	}
	ins, err := t.Tx.InsertEventIfAbsent(ctx, e)
	if err == nil && ins {
		atomic.AddInt64(&t.inserts, 1)
	}
	return ins, err
}

func (t *FaultTx) SaveResult(ctx context.Context, r StoredResult) error {
	if t.FailSaveResult {
		return ErrInjected
	}
	return t.Tx.SaveResult(ctx, r)
}
