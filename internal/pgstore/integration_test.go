package pgstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"focuslab/internal/domain"
	"focuslab/internal/service"
	"focuslab/internal/store"
)

// These integration tests drive the full service against a real PostgreSQL
// instance so the transactional guarantees (per-subject serialisation, atomic
// batch write + recompute, JSONB result round-trip) are exercised end to end.
// They share the FOCUS_PG_DSN skip guard via pgTestStore.

func newPGService(t *testing.T) (*service.Service, *PG, func() time.Time) {
	t.Helper()
	pg := pgTestStore(t)
	fixed := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return fixed }
	return service.New(pg, now), pg, now
}

func createAdultPG(t *testing.T, svc *service.Service) (uuid.UUID, uuid.UUID) {
	t.Helper()
	out, err := svc.CreateExperiment(context.Background(), service.CreateExperimentInput{
		Name:        "pg-study",
		DisplayName: "Alice",
		Birth:       time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
		Timezone:    "Asia/Shanghai",
	})
	require.NoError(t, err)
	return out.ExperimentID, out.SubjectID
}

func cardEv(dev string, seq int64, at time.Time, dur time.Duration) domain.Event {
	return domain.Event{DeviceID: dev, ClientSeq: seq, Type: domain.EventCardView, OccurredAt: at, DurationMS: dur.Milliseconds()}
}

// TestPGIntegration_ConcurrentDifferentEventsConverge is the regression test for
// the lost-update bug: many goroutines each write a DISTINCT event for the same
// subject concurrently. With per-subject FOR UPDATE serialisation the final
// committed result must contain ALL events (none dropped by an overwriting
// recompute), and must equal a fresh fold over the full set (replayable).
func TestPGIntegration_ConcurrentDifferentEventsConverge(t *testing.T) {
	svc, _, now := newPGService(t)
	exp, sub := createAdultPG(t, svc)

	base := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC) // 09:00 local Shanghai
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := cardEv("A", int64(i+1), base.Add(time.Duration(i)*time.Minute), 30*time.Second)
			_, err := svc.WriteEvents(context.Background(), service.WriteEventsInput{
				ExperimentID: exp, SubjectID: sub, Events: []domain.Event{ev},
			})
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()

	res, err := svc.GetResult(context.Background(), exp, sub, 0)
	require.NoError(t, err)
	require.Equal(t, n, res.Result.EventCount, "all concurrently-written events must be counted")

	// The stored result must equal a fresh fold over the full event set: proves
	// convergence to the complete, replayable result rather than a partial one.
	subject := domain.Subject{ID: sub, Birth: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), Timezone: "Asia/Shanghai"}
	var all []domain.Event
	for i := 0; i < n; i++ {
		e := cardEv("A", int64(i+1), base.Add(time.Duration(i)*time.Minute), 30*time.Second)
		e.ExperimentID, e.SubjectID = exp, sub
		all = append(all, e)
	}
	expected := domain.Fold(subject, all, domain.FoldConfig{AsOf: now()})
	require.Equal(t, expected.Digest, res.Result.Digest)
	require.Equal(t, (time.Duration(n) * 30 * time.Second).Milliseconds(), res.Result.TotalEngagedMS)
}

// TestPGIntegration_ConcurrentLateArrivalCorrection writes an initial event then
// concurrently fires several late (earlier-timestamped) events. The final result
// must incorporate every event, matching a full replay — no correction is lost.
func TestPGIntegration_ConcurrentLateArrivalCorrection(t *testing.T) {
	svc, _, now := newPGService(t)
	exp, sub := createAdultPG(t, svc)

	origin := time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC)
	_, err := svc.WriteEvents(context.Background(), service.WriteEventsInput{
		ExperimentID: exp, SubjectID: sub,
		Events: []domain.Event{cardEv("A", 100, origin, time.Minute)},
	})
	require.NoError(t, err)

	const late = 20
	var wg sync.WaitGroup
	for i := 0; i < late; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Earlier timestamps + lower seqs = out-of-order late arrivals.
			ev := cardEv("A", int64(i+1), origin.Add(-time.Duration(i+1)*time.Minute), time.Minute)
			_, e := svc.WriteEvents(context.Background(), service.WriteEventsInput{
				ExperimentID: exp, SubjectID: sub, Events: []domain.Event{ev},
			})
			require.NoError(t, e)
		}(i)
	}
	wg.Wait()

	res, err := svc.GetResult(context.Background(), exp, sub, 0)
	require.NoError(t, err)
	require.Equal(t, late+1, res.Result.EventCount)

	subject := domain.Subject{ID: sub, Birth: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), Timezone: "Asia/Shanghai"}
	var all []domain.Event
	e0 := cardEv("A", 100, origin, time.Minute)
	e0.ExperimentID, e0.SubjectID = exp, sub
	all = append(all, e0)
	for i := 0; i < late; i++ {
		e := cardEv("A", int64(i+1), origin.Add(-time.Duration(i+1)*time.Minute), time.Minute)
		e.ExperimentID, e.SubjectID = exp, sub
		all = append(all, e)
	}
	expected := domain.Fold(subject, all, domain.FoldConfig{AsOf: now()})
	require.Equal(t, expected.Digest, res.Result.Digest, "final result must equal full replay")
}

// TestPGIntegration_MidnightSplit verifies an engagement spanning local midnight
// is split across two daily buckets after a full round-trip through JSONB.
func TestPGIntegration_MidnightSplit(t *testing.T) {
	svc, _, _ := newPGService(t)
	exp, sub := createAdultPG(t, svc)

	// 23:50 local Shanghai June 1 == 15:50 UTC June 1; 20m spans into June 2.
	start := time.Date(2026, 6, 1, 15, 50, 0, 0, time.UTC)
	_, err := svc.WriteEvents(context.Background(), service.WriteEventsInput{
		ExperimentID: exp, SubjectID: sub,
		Events: []domain.Event{cardEv("A", 1, start, 20*time.Minute)},
	})
	require.NoError(t, err)

	res, err := svc.GetResult(context.Background(), exp, sub, 0)
	require.NoError(t, err)
	require.Len(t, res.Result.Daily, 2)
	require.Equal(t, "2026-06-01", res.Result.Daily[0].Day)
	require.Equal(t, (10 * time.Minute).Milliseconds(), res.Result.Daily[0].EngagedMS)
	require.Equal(t, "2026-06-02", res.Result.Daily[1].Day)
	require.Equal(t, (10 * time.Minute).Milliseconds(), res.Result.Daily[1].EngagedMS)
}

// TestPGIntegration_SessionCapTruncation verifies the single-session cap reduces
// allowed usage (not just records a violation) through the real DB.
func TestPGIntegration_SessionCapTruncation(t *testing.T) {
	svc, _, _ := newPGService(t)
	// Adult: 15m single-session cap.
	exp, sub := createAdultPG(t, svc)

	start := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	_, err := svc.WriteEvents(context.Background(), service.WriteEventsInput{
		ExperimentID: exp, SubjectID: sub,
		Events: []domain.Event{cardEv("A", 1, start, 25*time.Minute)},
	})
	require.NoError(t, err)

	res, err := svc.GetResult(context.Background(), exp, sub, 0)
	require.NoError(t, err)
	require.Len(t, res.Result.Daily, 1)
	require.Equal(t, (25 * time.Minute).Milliseconds(), res.Result.Daily[0].EngagedMS)
	require.Equal(t, (15 * time.Minute).Milliseconds(), res.Result.Daily[0].AllowedMS,
		"session cap must truncate allowed usage")

	var hasSession bool
	for _, v := range res.Result.Violations {
		if v.Code == domain.ViolationSessionLimit {
			hasSession = true
		}
	}
	require.True(t, hasSession)
}

// TestPGIntegration_WriteRollbackOnFailure verifies that when a mutation fails
// mid-transaction, the whole batch rolls back at the database level: none of the
// batch's event rows survive. A fault-injecting store forces SaveResult to fail
// after the events were inserted within the same tx.
func TestPGIntegration_WriteRollbackOnFailure(t *testing.T) {
	pg := pgTestStore(t)
	fixed := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	// Seed experiment + subject on the clean store.
	setup := service.New(pg, func() time.Time { return fixed })
	exp, sub := createAdultPG(t, setup)

	start := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	events := []domain.Event{
		cardEv("A", 1, start, time.Minute),
		cardEv("A", 2, start.Add(time.Minute), time.Minute),
		cardEv("A", 3, start.Add(2*time.Minute), time.Minute),
	}

	// Wrap PG so SaveResult fails: the batch's inserts must roll back with it.
	faulty := store.NewFaultStore(pg, store.FaultConfig{FailSaveResult: true})
	fs := service.New(faulty, func() time.Time { return fixed })
	_, err := fs.WriteEvents(context.Background(), service.WriteEventsInput{
		ExperimentID: exp, SubjectID: sub, Events: events,
	})
	require.ErrorIs(t, err, store.ErrInjected)

	// No events persisted and no result exists — the tx left zero residue.
	assertNoEvents(t, pg, exp, sub)
	_, gerr := setup.GetResult(context.Background(), exp, sub, 0)
	require.Error(t, gerr)

	// A subsequent clean write succeeds and sees exactly the batch.
	out, err := setup.WriteEvents(context.Background(), service.WriteEventsInput{
		ExperimentID: exp, SubjectID: sub, Events: events,
	})
	require.NoError(t, err)
	require.Equal(t, 3, out.Accepted)
}

// assertNoEvents fails the test unless the subject has zero stored events.
func assertNoEvents(t *testing.T, pg *PG, exp, sub uuid.UUID) {
	t.Helper()
	require.NoError(t, pg.WithTx(context.Background(), func(tx store.Tx) error {
		all, e := tx.ListEvents(context.Background(), exp, sub)
		require.NoError(t, e)
		require.Empty(t, all)
		return nil
	}))
}
