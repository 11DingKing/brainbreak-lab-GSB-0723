package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func mustTZ(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestAgeInLocation_TimezoneBoundary(t *testing.T) {
	// Born 2008-01-01 00:00 local Shanghai (which is 2007-12-31 16:00 UTC).
	sh := mustTZ(t, "Asia/Shanghai")
	birth := time.Date(2008, 1, 1, 0, 0, 0, 0, sh)

	// At 2025-12-31 15:00 UTC it is 2025-12-31 23:00 in Shanghai: the subject's
	// local birthday (Jan 1) has NOT arrived yet, so locally they are still 17.
	// Viewed purely in UTC, the birth projects to Dec 31 and that day has been
	// reached, so a UTC computation would say 18. The subject's timezone must
	// win — this is the timezone-dynamic age the spec requires.
	at := time.Date(2025, 12, 31, 15, 0, 0, 0, time.UTC)
	require.Equal(t, 17, AgeInLocation(birth, at, sh))
	require.Equal(t, 18, AgeInLocation(birth, at, time.UTC))
}

func TestBandFor(t *testing.T) {
	require.Equal(t, BandChild, BandFor(11))
	require.Equal(t, BandTeen, BandFor(12))
	require.Equal(t, BandTeen, BandFor(17))
	require.Equal(t, BandAdult, BandFor(18))
	require.Equal(t, BandAdult, BandFor(40))
}

func TestPolicyFor(t *testing.T) {
	require.Equal(t, 60*time.Minute, PolicyFor(BandAdult).DailyLimit)
	require.Equal(t, 15*time.Minute, PolicyFor(BandAdult).SessionLimit)
	require.Equal(t, 30*time.Minute, PolicyFor(BandTeen).DailyLimit)
	require.NotNil(t, PolicyFor(BandTeen).CurfewStart)
	require.Equal(t, 10*time.Minute, PolicyFor(BandChild).SessionLimit)
	require.Zero(t, PolicyFor(BandChild).DailyLimit)
}

// helper: build a card_view event.
func ev(exp, sub uuid.UUID, dev string, seq int64, at time.Time, dur time.Duration) Event {
	return Event{
		ExperimentID: exp, SubjectID: sub, DeviceID: dev, ClientSeq: seq,
		Type: EventCardView, OccurredAt: at, Duration: dur,
	}
}

func TestFold_AdultDailyAndSessionLimits(t *testing.T) {
	exp, sub := uuid.New(), uuid.New()
	loc := mustTZ(t, "Asia/Shanghai")
	// Adult born 2000.
	subject := Subject{ID: sub, Birth: time.Date(2000, 6, 1, 0, 0, 0, 0, loc), Timezone: "Asia/Shanghai"}
	asOf := time.Date(2026, 6, 1, 12, 0, 0, 0, loc)

	base := time.Date(2026, 6, 1, 9, 0, 0, 0, loc)
	events := []Event{
		// One 20-minute session on device A → exceeds 15m session cap.
		ev(exp, sub, "A", 1, base, 20*time.Minute),
		// Attention switch closes the session.
		{ExperimentID: exp, SubjectID: sub, DeviceID: "A", ClientSeq: 2, Type: EventAttentionSwitch, OccurredAt: base.Add(21 * time.Minute)},
		// Another 50-minute chunk pushes daily total to 70m > 60m cap.
		ev(exp, sub, "A", 3, base.Add(30*time.Minute), 50*time.Minute),
	}
	res := Fold(subject, events, FoldConfig{AsOf: asOf})
	require.Equal(t, BandAdult, res.Band)
	require.Len(t, res.Daily, 1)
	require.Equal(t, (70 * time.Minute).Milliseconds(), res.Daily[0].EngagedMS)
	require.Equal(t, (60 * time.Minute).Milliseconds(), res.Daily[0].AllowedMS)
	require.True(t, res.Daily[0].OverDailyLimit)

	var haveDaily, haveSession bool
	for _, v := range res.Violations {
		if v.Code == ViolationDailyLimit {
			haveDaily = true
		}
		if v.Code == ViolationSessionLimit {
			haveSession = true
		}
	}
	require.True(t, haveDaily, "expected daily limit violation")
	require.True(t, haveSession, "expected session limit violation")
}

func TestFold_TeenCurfew(t *testing.T) {
	exp, sub := uuid.New(), uuid.New()
	loc := mustTZ(t, "Asia/Shanghai")
	// Teen born 2012 → 14 in 2026.
	subject := Subject{ID: sub, Birth: time.Date(2012, 1, 1, 0, 0, 0, 0, loc), Timezone: "Asia/Shanghai"}
	asOf := time.Date(2026, 6, 1, 23, 30, 0, 0, loc)

	// Event at 22:30 local falls inside the [22:00,23:00) curfew.
	curfewEvent := ev(exp, sub, "A", 1, time.Date(2026, 6, 1, 22, 30, 0, 0, loc), 10*time.Minute)
	// Event at 20:00 local is allowed.
	okEvent := ev(exp, sub, "A", 2, time.Date(2026, 6, 1, 20, 0, 0, 0, loc), 10*time.Minute)

	res := Fold(subject, []Event{curfewEvent, okEvent}, FoldConfig{AsOf: asOf})
	require.Equal(t, BandTeen, res.Band)

	var curfew bool
	for _, v := range res.Violations {
		if v.Code == ViolationCurfew {
			curfew = true
		}
	}
	require.True(t, curfew, "expected curfew violation")
	// Curfew-blocked time is not counted as engaged.
	require.Equal(t, (10 * time.Minute).Milliseconds(), res.Daily[0].EngagedMS)
	require.Equal(t, (10 * time.Minute).Milliseconds(), res.Daily[0].CurfewBlockedMS)
}

func TestFold_ChildSessionLimit(t *testing.T) {
	exp, sub := uuid.New(), uuid.New()
	loc := mustTZ(t, "UTC")
	subject := Subject{ID: sub, Birth: time.Date(2018, 1, 1, 0, 0, 0, 0, loc), Timezone: "UTC"}
	asOf := time.Date(2026, 6, 1, 12, 0, 0, 0, loc)
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, loc)
	// 12-minute single session > 10m child cap.
	res := Fold(subject, []Event{ev(exp, sub, "A", 1, base, 12*time.Minute)}, FoldConfig{AsOf: asOf})
	require.Equal(t, BandChild, res.Band)
	var session bool
	for _, v := range res.Violations {
		if v.Code == ViolationSessionLimit {
			session = true
		}
	}
	require.True(t, session)
}

func TestFold_TimezoneCrossDaySplitsUsage(t *testing.T) {
	exp, sub := uuid.New(), uuid.New()
	loc := mustTZ(t, "Asia/Shanghai")
	subject := Subject{ID: sub, Birth: time.Date(1990, 1, 1, 0, 0, 0, 0, loc), Timezone: "Asia/Shanghai"}
	asOf := time.Date(2026, 6, 2, 12, 0, 0, 0, loc)

	// 23:50 local June 1 and 00:10 local June 2 — same ~UTC window, different
	// local calendar days. Must land in two daily buckets.
	e1 := ev(exp, sub, "A", 1, time.Date(2026, 6, 1, 23, 50, 0, 0, loc), 5*time.Minute)
	e2 := ev(exp, sub, "A", 2, time.Date(2026, 6, 2, 0, 10, 0, 0, loc), 5*time.Minute)
	res := Fold(subject, []Event{e1, e2}, FoldConfig{AsOf: asOf})
	require.Len(t, res.Daily, 2)
	require.Equal(t, "2026-06-01", res.Daily[0].Day)
	require.Equal(t, "2026-06-02", res.Daily[1].Day)
}
