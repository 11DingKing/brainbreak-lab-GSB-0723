package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"focuslab/internal/domain"
)

func seed(t *testing.T, m *Mem) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	exp, sub := uuid.New(), uuid.New()
	require.NoError(t, m.WithTx(ctx, func(tx Tx) error {
		if err := tx.UpsertExperiment(ctx, Experiment{ID: exp, Name: "t", Version: 1, CreatedAt: time.Now()}); err != nil {
			return err
		}
		return tx.UpsertSubject(ctx, StoredSubject{ID: sub, ExperimentID: exp, Auth: AuthActive})
	}))
	return exp, sub
}

// TestMemTxRollbackDiscardsChanges verifies the staged-copy transaction model:
// a closure that returns an error leaves committed state untouched.
func TestMemTxRollbackDiscardsChanges(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	exp, sub := seed(t, m)

	sentinel := errors.New("boom")
	err := m.WithTx(ctx, func(tx Tx) error {
		_, e := tx.InsertEventIfAbsent(ctx, domain.Event{
			ExperimentID: exp, SubjectID: sub, DeviceID: "A", ClientSeq: 1,
			Type: domain.EventCardView, OccurredAt: time.Now().UTC(), Duration: time.Minute,
		})
		require.NoError(t, e)
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	require.NoError(t, m.WithTx(ctx, func(tx Tx) error {
		all, e := tx.ListEvents(ctx, exp, sub)
		require.NoError(t, e)
		require.Empty(t, all, "rolled-back events must not be visible")
		return nil
	}))
}

// TestMemTxPanicRollsBack verifies a panic inside the closure rolls back and is
// converted to an error rather than crashing the process.
func TestMemTxPanicRollsBack(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	exp, sub := seed(t, m)

	err := m.WithTx(ctx, func(tx Tx) error {
		_, _ = tx.InsertEventIfAbsent(ctx, domain.Event{
			ExperimentID: exp, SubjectID: sub, DeviceID: "A", ClientSeq: 1,
			Type: domain.EventCardView, OccurredAt: time.Now().UTC(), Duration: time.Minute,
		})
		panic("mid-tx failure")
	})
	require.Error(t, err)

	require.NoError(t, m.WithTx(ctx, func(tx Tx) error {
		all, e := tx.ListEvents(ctx, exp, sub)
		require.NoError(t, e)
		require.Empty(t, all)
		return nil
	}))
}

// TestMemIdempotentInsert verifies a repeated event key inserts once.
func TestMemIdempotentInsert(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	exp, sub := seed(t, m)
	e := domain.Event{
		ExperimentID: exp, SubjectID: sub, DeviceID: "A", ClientSeq: 7,
		Type: domain.EventCardView, OccurredAt: time.Now().UTC(), Duration: time.Minute,
	}
	require.NoError(t, m.WithTx(ctx, func(tx Tx) error {
		ins1, _ := tx.InsertEventIfAbsent(ctx, e)
		ins2, _ := tx.InsertEventIfAbsent(ctx, e)
		require.True(t, ins1)
		require.False(t, ins2, "second insert of same key must be a no-op")
		return nil
	}))
}
