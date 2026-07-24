package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type AgeGroup string

const (
	AgeChild AgeGroup = "child"
	AgeTeen  AgeGroup = "teen"
	AgeAdult AgeGroup = "adult"
)

type EventType string

const (
	EventCardView        EventType = "card_view"
	EventAttentionSwitch EventType = "attention_switch"
	EventSlowReading     EventType = "slow_reading_answer"
	EventViewingSession  EventType = "viewing_session"
)

func (e EventType) Valid() bool {
	switch e {
	case EventCardView, EventAttentionSwitch, EventSlowReading, EventViewingSession:
		return true
	}
	return false
}

type Subject struct {
	ID          uuid.UUID  `json:"id"`
	DateOfBirth time.Time  `json:"date_of_birth"` // 本地日历日期，存储为 UTC 午夜
	Timezone    string     `json:"timezone"`
	Bedtime     *time.Time `json:"bedtime,omitempty"` // 仅 HH:MM 有意义
	ConsentAt   time.Time  `json:"consent_at"`
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type Experiment struct {
	ID        uuid.UUID `json:"id"`
	SubjectID uuid.UUID `json:"subject_id"`
	Label     string    `json:"label,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}

type Event struct {
	ID           uuid.UUID        `json:"id"`
	BatchID      int64            `json:"batch_id"`
	ExperimentID uuid.UUID        `json:"experiment_id"`
	SubjectID    uuid.UUID        `json:"subject_id"`
	ClientSeq    int64            `json:"client_seq"`
	DeviceID     string           `json:"device_id"`
	EventType    EventType        `json:"event_type"`
	OccurredAt   time.Time        `json:"occurred_at"`
	ReceivedAt   time.Time        `json:"received_at"`
	Payload      json.RawMessage  `json:"payload"`
}

// SessionDuration 返回观看会话时长（秒）；非观看事件返回 0。
func (e *Event) SessionDuration() int64 {
	if e.EventType != EventViewingSession {
		return 0
	}
	var p struct {
		DurationSeconds int64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return 0
	}
	if p.DurationSeconds < 0 {
		return 0
	}
	return p.DurationSeconds
}

type IngestBatch struct {
	ID             int64     `json:"id"`
	ExperimentID   uuid.UUID `json:"experiment_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	ReceivedAt     time.Time `json:"received_at"`
	EventCount     int       `json:"event_count"`
	AcceptedCount  int       `json:"accepted_count"`
}

type Violation struct {
	ID           int64           `json:"id"`
	ExperimentID uuid.UUID       `json:"experiment_id"`
	SubjectID    uuid.UUID       `json:"subject_id"`
	LocalDate    time.Time       `json:"local_date"`
	RuleCode     string          `json:"rule_code"`
	EventID      *uuid.UUID      `json:"event_id,omitempty"`
	Detail       json.RawMessage `json:"detail"`
	EventVersion int64           `json:"event_version"`
	CreatedAt    time.Time       `json:"created_at"`
}

type DailyUsage struct {
	ExperimentID uuid.UUID `json:"experiment_id"`
	SubjectID    uuid.UUID `json:"subject_id"`
	LocalDate    time.Time `json:"local_date"`
	TotalSeconds int64     `json:"total_seconds"`
	SessionCount int       `json:"session_count"`
	EventVersion int64     `json:"event_version"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type DaySummary struct {
	Date           string          `json:"date"`
	AgeGroup       AgeGroup        `json:"age_group"`
	TotalSeconds   int64           `json:"total_seconds"`
	SessionCount   int             `json:"session_count"`
	Violations     []ViolationView `json:"violations"`
}

type ViolationView struct {
	RuleCode string          `json:"rule_code"`
	Detail   json.RawMessage `json:"detail"`
}

type Totals struct {
	TotalSeconds   int64 `json:"total_seconds"`
	SessionCount   int   `json:"session_count"`
	ViolationCount int   `json:"violation_count"`
}

type ResultSummary struct {
	SubjectID      uuid.UUID    `json:"subject_id"`
	ExperimentID   uuid.UUID    `json:"experiment_id"`
	EventVersion   int64        `json:"event_version"`
	SchemaVersion  int          `json:"schema_version"`
	ComputedAt     time.Time    `json:"computed_at"`
	Days           []DaySummary `json:"days"`
	Totals         Totals       `json:"totals"`
}
