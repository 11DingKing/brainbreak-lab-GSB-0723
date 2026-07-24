package pgstore

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"focuslab/internal/cryptoshred"
	"focuslab/internal/domain"
	"focuslab/internal/store"
)

// pgTestStore connects to the DSN in FOCUS_PG_DSN, migrating a scratch schema.
// The whole suite is skipped when the variable is unset so it runs cleanly in
// environments without a database (e.g. CI without Postgres) yet provides full
// integration coverage where one is available.
func pgTestStore(t *testing.T) *PG {
	t.Helper()
	dsn := os.Getenv("FOCUS_PG_DSN")
	if dsn == "" {
		t.Skip("FOCUS_PG_DSN not set; skipping Postgres integration tests")
	}
	ctx := context.Background()
	pg, err := Open(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pg.Migrate(ctx))
	t.Cleanup(pg.Close)
	return pg
}

func seedSubject(t *testing.T, pg *PG) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	exp, sub := uuid.New(), uuid.New()
	require.NoError(t, pg.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.UpsertExperiment(ctx, store.Experiment{ID: exp, Name: "it", Version: 1, CreatedAt: time.Now()}); err != nil {
			return err
		}
		return tx.UpsertSubject(ctx, store.StoredSubject{ID: sub, ExperimentID: exp, Auth: store.AuthActive, SealedPersonal: []byte("sealed")})
	}))
	return exp, sub
}

// TestPG_IdempotentInsertUnderConcurrency fires the same event from many
// goroutines and asserts exactly one insert wins (UNIQUE + ON CONFLICT).
func TestPG_IdempotentInsertUnderConcurrency(t *testing.T) {
	pg := pgTestStore(t)
	ctx := context.Background()
	exp, sub := seedSubject(t, pg)

	e := domain.Event{
		ExperimentID: exp, SubjectID: sub, DeviceID: "A", ClientSeq: 1,
		Type: domain.EventCardView, OccurredAt: time.Now().UTC(), DurationMS: 60000,
	}

	const workers = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pg.WithTx(ctx, func(tx store.Tx) error {
				ins, err := tx.InsertEventIfAbsent(ctx, e)
				if err != nil {
					return err
				}
				if ins {
					mu.Lock()
					inserted++
					mu.Unlock()
				}
				return nil
			})
		}()
	}
	wg.Wait()
	require.Equal(t, 1, inserted, "event must be inserted exactly once under concurrency")

	require.NoError(t, pg.WithTx(ctx, func(tx store.Tx) error {
		all, err := tx.ListEvents(ctx, exp, sub)
		require.NoError(t, err)
		require.Len(t, all, 1)
		return nil
	}))
}

// TestPG_TxRollback verifies a failing closure leaves no residue.
func TestPG_TxRollback(t *testing.T) {
	pg := pgTestStore(t)
	ctx := context.Background()
	exp, sub := seedSubject(t, pg)

	sentinel := errInjected{}
	err := pg.WithTx(ctx, func(tx store.Tx) error {
		_, e := tx.InsertEventIfAbsent(ctx, domain.Event{
			ExperimentID: exp, SubjectID: sub, DeviceID: "A", ClientSeq: 99,
			Type: domain.EventCardView, OccurredAt: time.Now().UTC(), DurationMS: 1000,
		})
		require.NoError(t, e)
		return sentinel // force rollback
	})
	require.ErrorIs(t, err, sentinel)

	require.NoError(t, pg.WithTx(ctx, func(tx store.Tx) error {
		all, e := tx.ListEvents(ctx, exp, sub)
		require.NoError(t, e)
		require.Empty(t, all, "rolled-back insert must not persist")
		return nil
	}))
}

type errInjected struct{}

func (errInjected) Error() string { return "injected" }

// TestPG_HardDeleteCascadesAndShreds proves personal data cannot be recovered
// from derived tables after delete + key destruction.
func TestPG_HardDeleteCascadesAndShreds(t *testing.T) {
	pg := pgTestStore(t)
	ctx := context.Background()
	exp, sub := seedSubject(t, pg)

	// Insert an event and a key.
	require.NoError(t, pg.PutKey(sub, []byte("0123456789abcdef0123456789abcdef")))
	require.NoError(t, pg.WithTx(ctx, func(tx store.Tx) error {
		_, e := tx.InsertEventIfAbsent(ctx, domain.Event{
			ExperimentID: exp, SubjectID: sub, DeviceID: "A", ClientSeq: 1,
			Type: domain.EventCardView, OccurredAt: time.Now().UTC(), DurationMS: 1000,
		})
		return e
	}))

	// Purge derived data + destroy key.
	require.NoError(t, pg.WithTx(ctx, func(tx store.Tx) error {
		return tx.PurgeSubjectData(ctx, exp, sub)
	}))
	require.NoError(t, pg.DestroyKey(sub))

	// Events are gone.
	require.NoError(t, pg.WithTx(ctx, func(tx store.Tx) error {
		all, e := tx.ListEvents(ctx, exp, sub)
		require.NoError(t, e)
		require.Empty(t, all)
		return nil
	}))
	// Key is destroyed (unrecoverable).
	_, err := pg.GetKey(sub)
	require.ErrorIs(t, err, cryptoshred.ErrKeyDestroyed)
}
