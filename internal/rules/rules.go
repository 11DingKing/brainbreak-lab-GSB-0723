package rules

import (
	"encoding/json"
	"sort"
	"time"

	"brainbreak-lab/focus/internal/clock"
	"brainbreak-lab/focus/internal/model"
)

// 规则常量（秒）。
const (
	AdultDailyLimitSec  int64 = 60 * 60        // 60 分钟
	AdultSingleLimitSec int64 = 15 * 60        // 15 分钟
	TeenDailyLimitSec   int64 = 30 * 60        // 30 分钟
	ChildSingleLimitSec int64 = 10 * 60        // 10 分钟
)

// 规则代码。
const (
	RuleAdultDailyExceeded  = "adult_daily_limit"
	RuleAdultSessionTooLong = "adult_session_too_long"
	RuleTeenDailyExceeded   = "teen_daily_limit"
	RuleTeenBedtimeBan      = "teen_bedtime_ban"
	RuleChildSessionTooLong = "child_session_too_long"
)

const SchemaVersion = 1

// SubjectProfile 是规则引擎所需的 subject 只读信息。
type SubjectProfile struct {
	DateOfBirth time.Time
	Timezone    *time.Location
	Bedtime     *time.Time // 仅 HH:MM 有意义
}

// DayAccumulator 累计单日数据。
type DayAccumulator struct {
	Date         time.Time
	AgeGroup     model.AgeGroup
	TotalSeconds int64
	SessionCount int
	Violations   []model.Violation
}

// Evaluate 按 occurred_at 顺序扫描事件，输出按本地日期的累计与违规。
// 事件必须已按 occurred_at 升序排序；如未排序内部会排序（不修改入参）。
func Evaluate(events []model.Event, sub SubjectProfile) []DayAccumulator {
	sorted := make([]model.Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].OccurredAt.Before(sorted[j].OccurredAt)
	})

	byDate := map[string]*DayAccumulator{}
	var keys []string

	for i := range sorted {
		ev := &sorted[i]
		date := clock.LocalDate(ev.OccurredAt, sub.Timezone)
		key := date.Format("2006-01-02")
		d, ok := byDate[key]
		if !ok {
			age := clock.AgeGroupAt(sub.DateOfBirth, ev.OccurredAt, sub.Timezone)
			d = &DayAccumulator{Date: date, AgeGroup: age}
			byDate[key] = d
			keys = append(keys, key)
		}

		if ev.EventType == model.EventViewingSession {
			dur := ev.SessionDuration()
			d.TotalSeconds += dur
			d.SessionCount++
			checkSessionRules(d, ev, dur, sub)
		}
	}

	sort.Strings(keys)
	out := make([]DayAccumulator, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byDate[k])
	}
	return out
}

func checkSessionRules(d *DayAccumulator, ev *model.Event, dur int64, sub SubjectProfile) {
	switch d.AgeGroup {
	case model.AgeAdult:
		if dur > AdultSingleLimitSec {
			d.Violations = append(d.Violations, newViolation(ev, RuleAdultSessionTooLong, dur, AdultSingleLimitSec))
		}
		if d.TotalSeconds > AdultDailyLimitSec {
			// 仅当累计在本次会话跨过阈值时记录一次。
			if d.TotalSeconds-dur <= AdultDailyLimitSec {
				d.Violations = append(d.Violations, newViolation(ev, RuleAdultDailyExceeded, d.TotalSeconds, AdultDailyLimitSec))
			}
		}
	case model.AgeTeen:
		if d.TotalSeconds > TeenDailyLimitSec && d.TotalSeconds-dur <= TeenDailyLimitSec {
			d.Violations = append(d.Violations, newViolation(ev, RuleTeenDailyExceeded, d.TotalSeconds, TeenDailyLimitSec))
		}
		if sub.Bedtime != nil {
			ws, we := clock.BedtimeWindow(*sub.Bedtime, ev.OccurredAt, sub.Timezone)
			sStart := ev.OccurredAt
			sEnd := ev.OccurredAt.Add(time.Duration(dur) * time.Second)
			if clock.Overlaps(sStart, sEnd, ws, we) {
				d.Violations = append(d.Violations, newViolation(ev, RuleTeenBedtimeBan, dur, 0))
			}
		}
	case model.AgeChild:
		if dur > ChildSingleLimitSec {
			d.Violations = append(d.Violations, newViolation(ev, RuleChildSessionTooLong, dur, ChildSingleLimitSec))
		}
	}
}

func newViolation(ev *model.Event, code string, actual, limit int64) model.Violation {
	detail, _ := json.Marshal(map[string]int64{"actual_seconds": actual, "limit_seconds": limit})
	eid := ev.ID
	return model.Violation{
		ExperimentID: ev.ExperimentID,
		SubjectID:    ev.SubjectID,
		RuleCode:     code,
		EventID:      &eid,
		Detail:       detail,
	}
}
