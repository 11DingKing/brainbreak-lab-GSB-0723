package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type EventType string

const (
	EventCardView        EventType = "card_view"
	EventAttentionSwitch EventType = "attention_switch"
	EventSlowReading     EventType = "slow_reading_answer"
	EventWatchingSession EventType = "watching_session"
)

type AgeGroup string

const (
	AgeGroupAdult AgeGroup = "adult"
	AgeGroupTeen  AgeGroup = "teen"
	AgeGroupChild AgeGroup = "child"
)

type User struct {
	ID        uuid.UUID  `json:"id"`
	BirthDate time.Time  `json:"birth_date"`
	Timezone  string     `json:"timezone"`
	Bedtime   string     `json:"bedtime,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type Experiment struct {
	ID        uuid.UUID       `json:"id"`
	Version   int             `json:"version"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
}

type Event struct {
	ID           uuid.UUID       `json:"event_id"`
	UserID       uuid.UUID       `json:"user_id"`
	ExperimentID uuid.UUID       `json:"experiment_id"`
	DeviceID     string          `json:"device_id"`
	ClientSeq    int64           `json:"client_seq"`
	EventType    EventType       `json:"event_type"`
	OccurredAt   time.Time       `json:"occurred_at"`
	ReceivedAt   time.Time       `json:"received_at"`
	Payload      json.RawMessage `json:"payload"`
}

type CreateExperimentRequest struct {
	Name   string          `json:"name" binding:"required"`
	Config json.RawMessage `json:"config"`
}

type CreateUserRequest struct {
	BirthDate string `json:"birth_date" binding:"required"`
	Timezone  string `json:"timezone"`
	Bedtime   string `json:"bedtime,omitempty"`
}

type BatchEventsRequest struct {
	Events []EventInput `json:"events" binding:"required"`
}

type EventInput struct {
	EventID      string          `json:"event_id" binding:"required"`
	UserID       string          `json:"user_id" binding:"required"`
	ExperimentID string          `json:"experiment_id" binding:"required"`
	DeviceID     string          `json:"device_id" binding:"required"`
	ClientSeq    int64           `json:"client_seq"`
	EventType    EventType       `json:"event_type" binding:"required"`
	OccurredAt   time.Time       `json:"occurred_at" binding:"required"`
	Payload      json.RawMessage `json:"payload"`
}

type BatchEventsResponse struct {
	Accepted int  `json:"accepted"`
	Duplicate int `json:"duplicate"`
	Version  int  `json:"version"`
}

type RuleViolation struct {
	Type      string `json:"type"`
	Rule      string `json:"rule"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp,omitempty"`
}

type DailyAggregate struct {
	UserID                uuid.UUID       `json:"user_id"`
	ExperimentID          uuid.UUID       `json:"experiment_id"`
	UserDate              time.Time       `json:"user_date"`
	TotalDurationSeconds  int64           `json:"total_duration_seconds"`
	SessionCount          int             `json:"session_count"`
	LongestSessionSeconds int64           `json:"longest_session_seconds"`
	CardViewCount         int             `json:"card_view_count"`
	AttentionSwitchCount  int             `json:"attention_switch_count"`
	SlowReadingCount      int             `json:"slow_reading_count"`
	WatchingSessionCount  int             `json:"watching_session_count"`
	Violations            json.RawMessage `json:"violations"`
	Version               int             `json:"version"`
	ComputedAt            time.Time       `json:"computed_at"`
}

type ExperimentResult struct {
	UserID                  uuid.UUID       `json:"user_id"`
	ExperimentID            uuid.UUID       `json:"experiment_id"`
	Version                 int             `json:"version"`
	ResultJSON              json.RawMessage `json:"result_json"`
	TotalDurationSeconds    int64           `json:"total_duration_seconds"`
	TotalCardViews          int             `json:"total_card_views"`
	TotalAttentionSwitches  int             `json:"total_attention_switches"`
	TotalSlowReading        int             `json:"total_slow_reading"`
	TotalWatchingSessions   int             `json:"total_watching_sessions"`
	ViolationCount          int             `json:"violation_count"`
	ComputedAt              time.Time       `json:"computed_at"`
}

type ResultResponse struct {
	Result ExperimentResult  `json:"result"`
	Daily  []DailyAggregate  `json:"daily,omitempty"`
}
