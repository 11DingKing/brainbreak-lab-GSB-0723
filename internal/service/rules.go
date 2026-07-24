package service

import (
	"encoding/json"
	"fmt"
	"time"

	"brainbreak-lab/internal/models"
)

const (
	AdultDailyMaxSeconds     int64 = 60 * 60
	AdultSessionMaxSeconds   int64 = 15 * 60
	TeenDailyMaxSeconds      int64 = 30 * 60
	TeenBedtimeBufferMinutes       = 60
	ChildSessionMaxSeconds   int64 = 10 * 60
)

type RulesEngine struct{}

func NewRulesEngine() *RulesEngine {
	return &RulesEngine{}
}

func sessionDurationFromPayload(event models.Event) (int64, error) {
	if event.EventType != models.EventWatchingSession {
		return 0, nil
	}
	var p struct {
		DurationSeconds int64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal(event.Payload, &p); err != nil {
		return 0, fmt.Errorf("parse payload: %w", err)
	}
	if p.DurationSeconds <= 0 {
		var start, end time.Time
		var p2 struct {
			StartedAt  time.Time `json:"started_at"`
			FinishedAt time.Time `json:"finished_at"`
		}
		if err := json.Unmarshal(event.Payload, &p2); err == nil && !p2.StartedAt.IsZero() && !p2.FinishedAt.IsZero() {
			start = p2.StartedAt
			end = p2.FinishedAt
			dur := int64(end.Sub(start).Seconds())
			if dur > 0 {
				return dur, nil
			}
		}
	}
	return p.DurationSeconds, nil
}

type DaySession struct {
	Events            []models.Event
	TotalDuration     int64
	LongestSession    int64
	SessionCount      int
	CardViews         int
	AttentionSwitches int
	SlowReading       int
	WatchingSessions  int
}

type DayEvaluation struct {
	Date        time.Time
	Aggregate   DaySession
	Violations  []models.RuleViolation
}

func (r *RulesEngine) EvaluateDay(events []models.Event, userDate time.Time, ageGroup models.AgeGroup, bedtimeStr string, tz *time.Location) DayEvaluation {
	day := DaySession{}
	for _, e := range events {
		switch e.EventType {
		case models.EventCardView:
			day.CardViews++
		case models.EventAttentionSwitch:
			day.AttentionSwitches++
		case models.EventSlowReading:
			day.SlowReading++
		case models.EventWatchingSession:
			dur, _ := sessionDurationFromPayload(e)
			day.WatchingSessions++
			day.SessionCount++
			day.TotalDuration += dur
			if dur > day.LongestSession {
				day.LongestSession = dur
			}
			day.Events = append(day.Events, e)
		}
	}

	eval := DayEvaluation{
		Date:      userDate,
		Aggregate: day,
	}

	switch ageGroup {
	case models.AgeGroupAdult:
		if day.TotalDuration > AdultDailyMaxSeconds {
			eval.Violations = append(eval.Violations, models.RuleViolation{
				Type:    "daily_limit_exceeded",
				Rule:    "adult_daily_60min",
				Message: fmt.Sprintf("daily usage %ds exceeds adult limit %ds", day.TotalDuration, AdultDailyMaxSeconds),
			})
		}
		if day.LongestSession > AdultSessionMaxSeconds {
			eval.Violations = append(eval.Violations, models.RuleViolation{
				Type:    "session_limit_exceeded",
				Rule:    "adult_session_15min",
				Message: fmt.Sprintf("session %ds exceeds adult single-session limit %ds", day.LongestSession, AdultSessionMaxSeconds),
			})
		}

	case models.AgeGroupTeen:
		if day.TotalDuration > TeenDailyMaxSeconds {
			eval.Violations = append(eval.Violations, models.RuleViolation{
				Type:    "daily_limit_exceeded",
				Rule:    "teen_daily_30min",
				Message: fmt.Sprintf("daily usage %ds exceeds teen limit %ds", day.TotalDuration, TeenDailyMaxSeconds),
			})
		}
		if bedtimeStr != "" {
			bt, err := time.ParseInLocation("15:04", bedtimeStr, tz)
			if err == nil {
				for _, e := range day.Events {
					occurredLocal := e.OccurredAt.In(tz)
					bedtimeLocal := time.Date(occurredLocal.Year(), occurredLocal.Month(), occurredLocal.Day(),
						bt.Hour(), bt.Minute(), 0, 0, tz)
					cutoff := bedtimeLocal.Add(-time.Duration(TeenBedtimeBufferMinutes) * time.Minute)
					if occurredLocal.After(cutoff) && !occurredLocal.After(bedtimeLocal.Add(2*time.Hour)) {
						eval.Violations = append(eval.Violations, models.RuleViolation{
							Type:      "bedtime_violation",
							Rule:      "teen_no_usage_1h_before_bed",
							Message:   fmt.Sprintf("usage at %s within 1h of bedtime %s", occurredLocal.Format("15:04"), bedtimeLocal.Format("15:04")),
							Timestamp: occurredLocal.Format(time.RFC3339),
						})
					}
				}
			}
		}

	case models.AgeGroupChild:
		if day.LongestSession > ChildSessionMaxSeconds {
			eval.Violations = append(eval.Violations, models.RuleViolation{
				Type:    "session_limit_exceeded",
				Rule:    "child_session_10min",
				Message: fmt.Sprintf("session %ds exceeds child single-session limit %ds", day.LongestSession, ChildSessionMaxSeconds),
			})
		}
	}

	return eval
}
