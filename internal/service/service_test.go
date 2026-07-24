package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"focuslab/internal/cryptoshred"
	"focuslab/internal/domain"
	"focuslab/internal/store"
)

func fixedNow() func() time.Time {
	t := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func newSvc(t *testing.T) (*Service, *store.Mem) {
	t.Helper()
	mem := store.NewMem()
	return New(mem, fixedNow()), mem
}

func createAdult(t *testing.T, s *Service) (uuid.UUID, uuid.UUID) {
	t.Helper()
	out, err := s.CreateExperiment(context.Background(), CreateExperimentInput{
		Name:        "focus-study",
		DisplayName: "Alice",
		Birth:       time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
		Timezone:    "Asia/Shanghai",
	})
	require.NoError(t, err)
	return out.ExperimentID, out.SubjectID
}

func cardEvent(dev string, seq int64, at time.Time, dur time.Duration) domain.Event {
	return domain.Event{DeviceID: dev, ClientSeq: seq, Type: domain.EventCardView, OccurredAt: at, DurationMS: dur.Milliseconds()}
}

// TestConcurrentCrossDeviceUploads_CountedOnce fires the same batch from many
// goroutines (modelling concurrent cross-device retries) and asserts each event
// is counted exactly once and the final result is stable.
func TestConcurrentCrossDeviceUploads_CountedOnce(t *testing.T) {
	s, _ := newSvc(t)
	exp, sub := createAdult(t, s)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	events := []domain.Event{
		cardEvent("A", 1, base, 3*time.Minute),
		cardEvent("A", 2, base.Add(4*time.Minute), 3*time.Minute),
		cardEvent("B", 1, base.Add(8*time.Minute), 3*time.Minute),
	}

	const workers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	totalAccepted := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := s.WriteEvents(context.Background(), WriteEventsInput{
				ExperimentID: exp, SubjectID: sub, Events: events,
			})
			require.NoError(t, err)
			mu.Lock()
			totalAccepted += out.Accepted
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Across all workers, exactly len(events) inserts should have happened.
	require.Equal(t, len(events), totalAccepted, "each event must be accepted exactly once")

	res, err := s.GetResult(context.Background(), exp, sub, 0)
	require.NoError(t, err)
	require.Equal(t, 3, res.Result.EventCount)
	require.Equal(t, (9 * time.Minute).Milliseconds(), res.Result.TotalEngagedMS)
}

// TestLateArrivalCorrectsResult verifies that a late, out-of-order event
// triggers a corrected result (different digest) on the next write.
func TestLateArrivalCorrectsResult(t *testing.T) {
	s, _ := newSvc(t)
	exp, sub := createAdult(t, s)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	first, err := s.WriteEvents(context.Background(), WriteEventsInput{
		ExperimentID: exp, SubjectID: sub,
		Events: []domain.Event{cardEvent("A", 2, base.Add(10*time.Minute), 5*time.Minute)},
	})
	require.NoError(t, err)
	require.False(t, first.ResultCorrected)
	firstDigest := first.ResultDigest

	// A late event with an EARLIER timestamp and lower seq arrives afterwards.
	second, err := s.WriteEvents(context.Background(), WriteEventsInput{
		ExperimentID: exp, SubjectID: sub,
		Events: []domain.Event{cardEvent("A", 1, base, 5*time.Minute)},
	})
	require.NoError(t, err)
	require.NotEqual(t, firstDigest, second.ResultDigest, "late event must change the result")
	require.True(t, second.ResultCorrected, "late arrival must be flagged as a correction")

	res, err := s.GetResult(context.Background(), exp, sub, 0)
	require.NoError(t, err)
	require.Equal(t, 2, res.Result.EventCount)
	require.Equal(t, (10 * time.Minute).Milliseconds(), res.Result.TotalEngagedMS)
}

// TestFaultInjection_RollsBackBatch proves that when a mutation fails
// mid-transaction, none of the batch's inserts survive (atomic rollback).
func TestFaultInjection_RollsBackBatch(t *testing.T) {
	mem := store.NewMem()
	// First create the experiment/subject on the clean store.
	setup := New(mem, fixedNow())
	exp, sub := createAdult(t, setup)

	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	events := []domain.Event{
		cardEvent("A", 1, base, 3*time.Minute),
		cardEvent("A", 2, base.Add(4*time.Minute), 3*time.Minute),
		cardEvent("A", 3, base.Add(8*time.Minute), 3*time.Minute),
	}

	// Wrap the store so SaveResult fails: the whole WriteEvents tx must roll back.
	faulty := store.NewFaultStore(mem, store.FaultConfig{FailSaveResult: true})
	fs := New(faulty, fixedNow())
	_, err := fs.WriteEvents(context.Background(), WriteEventsInput{ExperimentID: exp, SubjectID: sub, Events: events})
	require.Error(t, err)
	require.True(t, errors.Is(err, store.ErrInjected))

	// No events should have been persisted, and no result should exist.
	res, gerr := setup.GetResult(context.Background(), exp, sub, 0)
	require.ErrorIs(t, gerr, ErrNotFound)
	require.Zero(t, res.Result.EventCount)

	// A subsequent clean write succeeds and sees exactly the batch — proving the
	// earlier failure left zero residue.
	out, err := setup.WriteEvents(context.Background(), WriteEventsInput{ExperimentID: exp, SubjectID: sub, Events: events})
	require.NoError(t, err)
	require.Equal(t, 3, out.Accepted)
}

// TestDeleteSubject_CryptoShredIrreversible verifies that after hard delete the
// personal data cannot be recovered from any derived table, and the key is gone.
func TestDeleteSubject_CryptoShredIrreversible(t *testing.T) {
	s, mem := newSvc(t)
	exp, sub := createAdult(t, s)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	_, err := s.WriteEvents(context.Background(), WriteEventsInput{
		ExperimentID: exp, SubjectID: sub, Events: []domain.Event{cardEvent("A", 1, base, 5*time.Minute)},
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteSubject(context.Background(), exp, sub))

	// Key must be destroyed (tombstoned).
	_, kerr := mem.GetKey(sub)
	require.ErrorIs(t, kerr, cryptoshred.ErrKeyDestroyed)

	// Result query now reports the subject as deleted / gone.
	_, gerr := s.GetResult(context.Background(), exp, sub, 0)
	require.Error(t, gerr)

	// Even attempting to re-establish a key is refused, so personal data stays
	// unrecoverable.
	c := cryptoshred.New(mem)
	require.ErrorIs(t, c.EnsureKey(sub), cryptoshred.ErrKeyDestroyed)

	// Writing new events for a deleted subject is refused.
	_, werr := s.WriteEvents(context.Background(), WriteEventsInput{
		ExperimentID: exp, SubjectID: sub, Events: []domain.Event{cardEvent("A", 2, base, 5*time.Minute)},
	})
	require.ErrorIs(t, werr, ErrDeleted)
}

// TestRevokeBlocksIngestion verifies revoked subjects reject new events but keep
// their existing result.
func TestRevokeBlocksIngestion(t *testing.T) {
	s, _ := newSvc(t)
	exp, sub := createAdult(t, s)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	_, err := s.WriteEvents(context.Background(), WriteEventsInput{
		ExperimentID: exp, SubjectID: sub, Events: []domain.Event{cardEvent("A", 1, base, 5*time.Minute)},
	})
	require.NoError(t, err)

	require.NoError(t, s.RevokeAuthorization(context.Background(), exp, sub))

	_, werr := s.WriteEvents(context.Background(), WriteEventsInput{
		ExperimentID: exp, SubjectID: sub, Events: []domain.Event{cardEvent("A", 2, base, 5*time.Minute)},
	})
	require.ErrorIs(t, werr, ErrRevoked)

	// Existing result is retained.
	res, gerr := s.GetResult(context.Background(), exp, sub, 0)
	require.NoError(t, gerr)
	require.Equal(t, 1, res.Result.EventCount)
}

// TestRecomputeByVersion verifies version bump retains prior result and produces
// a new one deterministically.
func TestRecomputeByVersion(t *testing.T) {
	s, _ := newSvc(t)
	exp, sub := createAdult(t, s)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	_, err := s.WriteEvents(context.Background(), WriteEventsInput{
		ExperimentID: exp, SubjectID: sub, Events: []domain.Event{cardEvent("A", 1, base, 5*time.Minute)},
	})
	require.NoError(t, err)

	out, err := s.Recompute(context.Background(), RecomputeInput{ExperimentID: exp, SubjectID: sub, NewVersion: true})
	require.NoError(t, err)
	require.Equal(t, int64(2), out.ResultVersion)

	// Both versions are queryable and share the same digest (same events).
	v1, err := s.GetResult(context.Background(), exp, sub, 1)
	require.NoError(t, err)
	v2, err := s.GetResult(context.Background(), exp, sub, 2)
	require.NoError(t, err)
	require.Equal(t, v1.Digest, v2.Digest)
}
