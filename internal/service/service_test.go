package service

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"testing"
	"testing/quick"
	"time"

	"brainbreak-lab/internal/models"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestCalculateAgeSimple(t *testing.T) {
	utc, _ := time.LoadLocation("UTC")
	bd := time.Date(2000, 6, 15, 0, 0, 0, 0, time.UTC)

	assert.Equal(t, 25, CalculateAge(bd, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), utc))
	assert.Equal(t, 26, CalculateAge(bd, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), utc))
	assert.Equal(t, 26, CalculateAge(bd, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), utc))
	assert.Equal(t, 25, CalculateAge(bd, time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC), utc))
}

func TestCalculateAgeTimezoneEffect(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	bd := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	dec31utc := time.Date(2025, 12, 31, 16, 0, 0, 0, time.UTC)
	age := CalculateAge(bd, dec31utc, shanghai)
	assert.Equal(t, 26, age, "16:00 UTC Dec 31 = 00:00 Jan 1 in Shanghai, so age should be 26")

	dec31utcEarly := time.Date(2025, 12, 31, 15, 0, 0, 0, time.UTC)
	age2 := CalculateAge(bd, dec31utcEarly, shanghai)
	assert.Equal(t, 25, age2, "15:00 UTC Dec 31 = 23:00 Dec 31 in Shanghai, age still 25")
}

func TestAgeGroupClassification(t *testing.T) {
	assert.Equal(t, models.AgeGroupChild, ClassifyAgeGroup(0))
	assert.Equal(t, models.AgeGroupChild, ClassifyAgeGroup(12))
	assert.Equal(t, models.AgeGroupTeen, ClassifyAgeGroup(13))
	assert.Equal(t, models.AgeGroupTeen, ClassifyAgeGroup(17))
	assert.Equal(t, models.AgeGroupAdult, ClassifyAgeGroup(18))
	assert.Equal(t, models.AgeGroupAdult, ClassifyAgeGroup(100))
}

func TestAdultRuleCompliance(t *testing.T) {
	re := NewRulesEngine()
	tz, _ := time.LoadLocation("UTC")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, tz)

	compliant := []models.Event{
		{EventType: models.EventWatchingSession, OccurredAt: day.Add(10 * time.Hour), Payload: json.RawMessage(`{"duration_seconds":900}`)},
		{EventType: models.EventWatchingSession, OccurredAt: day.Add(12 * time.Hour), Payload: json.RawMessage(`{"duration_seconds":900}`)},
		{EventType: models.EventWatchingSession, OccurredAt: day.Add(14 * time.Hour), Payload: json.RawMessage(`{"duration_seconds":900}`)},
		{EventType: models.EventWatchingSession, OccurredAt: day.Add(16 * time.Hour), Payload: json.RawMessage(`{"duration_seconds":900}`)},
	}
	eval := re.EvaluateDay(compliant, day, models.AgeGroupAdult, "", tz)
	assert.Equal(t, int64(3600), eval.Aggregate.TotalDuration)
	assert.Len(t, eval.Violations, 0, "4x900=3600 should be exactly at adult daily limit, no violation")
}

func TestAdultRuleDailyExceed(t *testing.T) {
	re := NewRulesEngine()
	tz, _ := time.LoadLocation("UTC")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, tz)

	overLimit := []models.Event{
		{EventType: models.EventWatchingSession, OccurredAt: day.Add(10 * time.Hour), Payload: json.RawMessage(`{"duration_seconds":3601}`)},
	}
	eval := re.EvaluateDay(overLimit, day, models.AgeGroupAdult, "", tz)
	assert.GreaterOrEqual(t, len(eval.Violations), 1)
	foundDaily := false
	for _, v := range eval.Violations {
		if v.Type == "daily_limit_exceeded" {
			foundDaily = true
		}
	}
	assert.True(t, foundDaily, "should flag daily limit exceeded")
}

func TestChildSessionLimit(t *testing.T) {
	re := NewRulesEngine()
	tz, _ := time.LoadLocation("UTC")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, tz)

	over := []models.Event{
		{EventType: models.EventWatchingSession, OccurredAt: day.Add(10 * time.Hour), Payload: json.RawMessage(`{"duration_seconds":601}`)},
	}
	eval := re.EvaluateDay(over, day, models.AgeGroupChild, "", tz)
	found := false
	for _, v := range eval.Violations {
		if v.Rule == "child_session_10min" {
			found = true
		}
	}
	assert.True(t, found)
}

func TestGroupByUserDateMultipleDays(t *testing.T) {
	tz, _ := time.LoadLocation("UTC")
	events := []models.Event{
		{OccurredAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)},
		{OccurredAt: time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)},
		{OccurredAt: time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)},
	}
	groups := groupByUserDate(events, tz)
	assert.Len(t, groups, 2)
	assert.Len(t, groups["2026-07-20"], 2)
	assert.Len(t, groups["2026-07-21"], 1)
}

func TestGroupByUserDateTimezoneBoundary(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	events := []models.Event{
		{OccurredAt: time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)},
	}
	groups := groupByUserDate(events, shanghai)
	assert.Contains(t, groups, "2026-07-20")
	assert.Len(t, groups["2026-07-20"], 1)
}

func makeWatchingEventForRules(at time.Time, dur int64) models.Event {
	return models.Event{
		ID:         uuid.New(),
		EventType:  models.EventWatchingSession,
		OccurredAt: at,
		Payload:    json.RawMessage(`{"duration_seconds":` + strconv.FormatInt(dur, 10) + `}`),
	}
}

func TestProperty_ReplaySameEventsSameResult(t *testing.T) {
	re := NewRulesEngine()
	tz, _ := time.LoadLocation("UTC")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, tz)

	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		n := rng.Intn(20) + 1
		var events []models.Event
		for i := 0; i < n; i++ {
			dur := int64(rng.Intn(2000))
			ev := makeWatchingEventForRules(day.Add(time.Duration(rng.Intn(86400))*time.Second), dur)
			events = append(events, ev)
		}
		eval1 := re.EvaluateDay(events, day, models.AgeGroupAdult, "", tz)
		eval2 := re.EvaluateDay(events, day, models.AgeGroupAdult, "", tz)
		return eval1.Aggregate.TotalDuration == eval2.Aggregate.TotalDuration &&
			len(eval1.Violations) == len(eval2.Violations)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestProperty_EventOrderDoesNotMatter(t *testing.T) {
	re := NewRulesEngine()
	tz, _ := time.LoadLocation("UTC")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, tz)

	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		n := rng.Intn(15) + 1
		var events []models.Event
		for i := 0; i < n; i++ {
			dur := int64(rng.Intn(2000))
			ev := makeWatchingEventForRules(day.Add(time.Duration(rng.Intn(86400))*time.Second), dur)
			events = append(events, ev)
		}
		eval1 := re.EvaluateDay(events, day, models.AgeGroupAdult, "", tz)

		shuffled := make([]models.Event, len(events))
		copy(shuffled, events)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		eval2 := re.EvaluateDay(shuffled, day, models.AgeGroupAdult, "", tz)

		return eval1.Aggregate.TotalDuration == eval2.Aggregate.TotalDuration &&
			eval1.Aggregate.SessionCount == eval2.Aggregate.SessionCount &&
			eval1.Aggregate.LongestSession == eval2.Aggregate.LongestSession &&
			len(eval1.Violations) == len(eval2.Violations)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestProperty_AgeNonNegative(t *testing.T) {
	f := func(birthYear, birthMonth, birthDay int, nowYear, nowMonth, nowDay int, tzOffset int) bool {
		bYear := 1900 + (birthYear % 120)
		bMonth := time.Month((birthMonth % 12) + 1)
		bDay := (birthDay % 28) + 1
		nYear := bYear + (nowYear % 100)
		nMonth := time.Month((nowMonth % 12) + 1)
		nDay := (nowDay % 28) + 1

		bd := time.Date(bYear, bMonth, bDay, 0, 0, 0, 0, time.UTC)
		now := time.Date(nYear, nMonth, nDay, 0, 0, 0, 0, time.UTC)
		age := CalculateAge(bd, now, time.UTC)
		return age >= 0
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

func TestProperty_DurationNeverNegative(t *testing.T) {
	re := NewRulesEngine()
	tz, _ := time.LoadLocation("UTC")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, tz)

	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		n := rng.Intn(10) + 1
		var events []models.Event
		for i := 0; i < n; i++ {
			dur := int64(rng.Intn(3600))
			events = append(events, makeWatchingEventForRules(day.Add(time.Duration(i)*time.Hour), dur))
		}
		eval := re.EvaluateDay(events, day, models.AgeGroupAdult, "", tz)
		return eval.Aggregate.TotalDuration >= 0 &&
			eval.Aggregate.LongestSession >= 0 &&
			eval.Aggregate.SessionCount >= 0
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}
