package rules

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"brainbreak-lab/focus/internal/model"
)

func mkSession(seq int64, at time.Time, durSec int64) model.Event {
	p, _ := json.Marshal(map[string]int64{"duration_seconds": durSec})
	return model.Event{
		ID:           uuid.New(),
		ExperimentID: uuid.New(),
		SubjectID:    uuid.New(),
		ClientSeq:    seq,
		DeviceID:     "dev",
		EventType:    model.EventViewingSession,
		OccurredAt:   at.UTC(),
		Payload:      p,
	}
}

func TestAdultLimits(t *testing.T) {
	loc := time.UTC
	dob := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	day := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	profile := SubjectProfile{DateOfBirth: dob, Timezone: loc}

	// 单次 20 分钟 -> too long; 当天累计 50+20=70 分钟 -> daily exceeded.
	events := []model.Event{
		mkSession(1, day, 50*60),
		mkSession(2, day.Add(2*time.Hour), 20*60),
	}
	days := Evaluate(events, profile)
	if len(days) != 1 {
		t.Fatalf("days=%d want 1", len(days))
	}
	d := days[0]
	if d.AgeGroup != model.AgeAdult {
		t.Fatalf("age group=%s", d.AgeGroup)
	}
	if d.TotalSeconds != 70*60 {
		t.Fatalf("total=%d", d.TotalSeconds)
	}
	var hasSessionLong, hasDaily bool
	for _, v := range d.Violations {
		if v.RuleCode == RuleAdultSessionTooLong {
			hasSessionLong = true
		}
		if v.RuleCode == RuleAdultDailyExceeded {
			hasDaily = true
		}
	}
	if !hasSessionLong || !hasDaily {
		t.Fatalf("violations=%+v", d.Violations)
	}
}

func TestChildSingleLimit(t *testing.T) {
	loc := time.UTC
	dob := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC) // 8 岁
	day := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	events := []model.Event{mkSession(1, day, 11*60)}
	days := Evaluate(events, SubjectProfile{DateOfBirth: dob, Timezone: loc})
	if len(days) != 1 || len(days[0].Violations) != 1 || days[0].Violations[0].RuleCode != RuleChildSessionTooLong {
		t.Fatalf("unexpected %+v", days)
	}
}

func TestTeenBedtimeAndDaily(t *testing.T) {
	nyc, _ := time.LoadLocation("America/New_York")
	dob := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC) // 16 岁
	bedtime := time.Date(0, 1, 1, 22, 0, 0, 0, time.UTC)
	profile := SubjectProfile{DateOfBirth: dob, Timezone: nyc, Bedtime: &bedtime}
	// 20:00 local a 20 分钟 session; 21:30 local a 25 分钟 session (overlaps 21:00-22:00 窗口)
	dayLocal := time.Date(2026, 7, 24, 0, 0, 0, 0, nyc)
	events := []model.Event{
		mkSession(1, dayLocal.Add(20*time.Hour), 20*60),
		mkSession(2, dayLocal.Add(21*time.Hour+30*time.Minute), 25*60),
	}
	days := Evaluate(events, profile)
	if len(days) != 1 {
		t.Fatalf("days=%d", len(days))
	}
	var bed, daily bool
	for _, v := range days[0].Violations {
		switch v.RuleCode {
		case RuleTeenBedtimeBan:
			bed = true
		case RuleTeenDailyExceeded:
			daily = true
		}
	}
	if !bed {
		t.Fatalf("missing bedtime violation: %+v", days[0].Violations)
	}
	// total = 45 min > 30 -> daily exceeded.
	if !daily {
		t.Fatalf("missing daily violation: %+v", days[0].Violations)
	}
}

// Property: 乱序输入与按 occurred_at 升序输入产出相同累计与违规数。
func TestOrderingInvariance(t *testing.T) {
	loc := time.UTC
	dob := time.Date(1995, 1, 1, 0, 0, 0, 0, time.UTC)
	base := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	events := []model.Event{
		mkSession(1, base, 10*60),
		mkSession(2, base.Add(1*time.Hour), 16*60),   // too long
		mkSession(3, base.Add(2*time.Hour), 35*60),   // pushes to 61 min
		mkSession(4, base.Add(3*time.Hour), 5*60),
	}
	// 多次洗牌：简单逆序、交叉、原始。
	permutes := [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}, {1, 3, 0, 2}}
	var first *model.DaySummary
	firstDays := Evaluate(events, SubjectProfile{DateOfBirth: dob, Timezone: loc})
	if len(firstDays) != 1 {
		t.Fatalf("days=%d", len(firstDays))
	}
	fd := firstDays[0]
	first = &model.DaySummary{TotalSeconds: fd.TotalSeconds, SessionCount: fd.SessionCount}
	first.Violations = make([]model.ViolationView, len(fd.Violations))
	for i := range fd.Violations {
		first.Violations[i] = model.ViolationView{RuleCode: fd.Violations[i].RuleCode}
	}
	for _, p := range permutes {
		shuffled := make([]model.Event, len(events))
		for i, idx := range p {
			shuffled[i] = events[idx]
		}
		got := Evaluate(shuffled, SubjectProfile{DateOfBirth: dob, Timezone: loc})
		if len(got) != 1 {
			t.Fatalf("perm %v: days=%d", p, len(got))
		}
		g := got[0]
		if g.TotalSeconds != first.TotalSeconds || g.SessionCount != first.SessionCount {
			t.Fatalf("perm %v totals differ: %+v vs %+v", p, g, first)
		}
		if len(g.Violations) != len(first.Violations) {
			t.Fatalf("perm %v violations count differ: %d vs %d", p, len(g.Violations), len(first.Violations))
		}
		for i := range g.Violations {
			if g.Violations[i].RuleCode != first.Violations[i].RuleCode {
				t.Fatalf("perm %v violation[%d] differ: %s vs %s", p, i, g.Violations[i].RuleCode, first.Violations[i].RuleCode)
			}
		}
	}
}
