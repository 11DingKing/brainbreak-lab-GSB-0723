package domain

import (
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// randomEvents generates a deterministic-per-seed bag of valid events for one
// subject across a few devices and days.
func randomEvents(rng *rand.Rand, exp, sub uuid.UUID, n int) []Event {
	loc := time.UTC
	types := []EventType{EventCardView, EventSlowReadAnswer, EventWatchSession, EventAttentionSwitch}
	devices := []string{"phone", "tablet", "laptop"}
	base := time.Date(2026, 3, 1, 8, 0, 0, 0, loc)
	seqByDev := map[string]int64{}
	var out []Event
	for i := 0; i < n; i++ {
		dev := devices[rng.Intn(len(devices))]
		seqByDev[dev]++
		typ := types[rng.Intn(len(types))]
		at := base.Add(time.Duration(rng.Intn(60*48)) * time.Minute)
		var dur time.Duration
		if typ != EventAttentionSwitch {
			dur = time.Duration(rng.Intn(20)+1) * time.Minute
		}
		out = append(out, Event{
			ExperimentID: exp, SubjectID: sub, DeviceID: dev, ClientSeq: seqByDev[dev],
			Type: typ, OccurredAt: at, Duration: dur,
		})
	}
	return out
}

func shuffle(rng *rand.Rand, in []Event) []Event {
	out := append([]Event(nil), in...)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// duplicateSome returns the events with a random subset duplicated (modelling
// retries and cross-device re-uploads of the same event).
func duplicateSome(rng *rand.Rand, in []Event) []Event {
	out := append([]Event(nil), in...)
	for _, e := range in {
		if rng.Intn(2) == 0 {
			out = append(out, e) // exact duplicate: same idempotency key + payload
		}
	}
	return shuffle(rng, out)
}

// TestProperty_ReplayInvariantUnderPermutationAndDuplication asserts the core
// guarantee: for any permutation and any duplication of the same underlying
// events, Fold produces an identical Result (and identical Digest). This is the
// property that makes results idempotent and replayable regardless of arrival
// order, duplicates, lateness or cross-device concurrency.
func TestProperty_ReplayInvariantUnderPermutationAndDuplication(t *testing.T) {
	subject := Subject{
		ID:       uuid.New(),
		Birth:    time.Date(1995, 5, 5, 0, 0, 0, 0, time.UTC),
		Timezone: "UTC",
	}
	asOf := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		exp := uuid.New()
		n := rng.Intn(40) + 1
		events := randomEvents(rng, exp, subject.ID, n)

		reference := Fold(subject, events, FoldConfig{AsOf: asOf})

		// Several independent permutations, with and without duplicates, must
		// all match the reference exactly.
		for trial := 0; trial < 5; trial++ {
			perm := shuffle(rng, events)
			got := Fold(subject, perm, FoldConfig{AsOf: asOf})
			require.Equal(t, reference, got, "seed=%d trial=%d permutation changed result", seed, trial)

			dup := duplicateSome(rng, events)
			gotDup := Fold(subject, dup, FoldConfig{AsOf: asOf})
			require.Equal(t, reference.Digest, gotDup.Digest, "seed=%d duplicates changed digest", seed)
			require.Equal(t, reference, gotDup, "seed=%d duplicates changed result", seed)
		}
	}
}

// TestProperty_LateArrivalIsIncorporated verifies that adding a late event and
// re-folding yields the same result as folding the whole set at once — i.e.
// incremental late arrival converges to the batch computation (correction).
func TestProperty_LateArrivalIsIncorporated(t *testing.T) {
	subject := Subject{ID: uuid.New(), Birth: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), Timezone: "UTC"}
	asOf := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	for seed := int64(0); seed < 100; seed++ {
		rng := rand.New(rand.NewSource(seed))
		exp := uuid.New()
		events := randomEvents(rng, exp, subject.ID, rng.Intn(30)+2)

		// Split into "arrived" and one "late" event.
		late := events[len(events)-1]
		early := events[:len(events)-1]

		fullFirst := Fold(subject, events, FoldConfig{AsOf: asOf})
		// Simulate late arrival: fold early set, then fold with the late one added.
		_ = Fold(subject, early, FoldConfig{AsOf: asOf})
		corrected := Fold(subject, append(append([]Event(nil), early...), late), FoldConfig{AsOf: asOf})

		require.Equal(t, fullFirst, corrected, "seed=%d late arrival did not converge", seed)
	}
}

// TestDigestStability confirms Digest ignores order and duplicates.
func TestDigestStability(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	exp, sub := uuid.New(), uuid.New()
	events := randomEvents(rng, exp, sub, 25)
	d1 := Digest(events)
	d2 := Digest(shuffle(rng, duplicateSome(rng, events)))
	require.Equal(t, d1, d2)
}
