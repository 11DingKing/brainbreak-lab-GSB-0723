package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"brainbreak-lab/internal/models"
	"brainbreak-lab/internal/store"

	"github.com/google/uuid"
)

type EventProcessor struct {
	db          *sql.DB
	events      *store.EventStore
	results     *store.ResultStore
	experiments *store.ExperimentStore
	users       *store.UserStore
	rules       *RulesEngine
}

func NewEventProcessor(db *sql.DB, events *store.EventStore, results *store.ResultStore, experiments *store.ExperimentStore, users *store.UserStore) *EventProcessor {
	return &EventProcessor{
		db:          db,
		events:      events,
		results:     results,
		experiments: experiments,
		users:       users,
		rules:       NewRulesEngine(),
	}
}

func uuidToInt64(id uuid.UUID) int64 {
	b := id[:8]
	return int64(uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7]))
}

type BatchResult struct {
	Accepted  int
	Duplicate int
	Version   int
	HadLate   bool
}

func (p *EventProcessor) IngestBatch(ctx context.Context, userID, experimentID uuid.UUID, inputs []models.EventInput) (*BatchResult, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	lockKey := uuidToInt64(userID) ^ uuidToInt64(experimentID)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	userExists, err := p.users.Exists(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("check user: %w", err)
	}
	if !userExists {
		_ = tx.Rollback()
		return nil, fmt.Errorf("user %s not found", userID)
	}
	expExists, err := p.experiments.Exists(ctx, experimentID)
	if err != nil {
		return nil, fmt.Errorf("check experiment: %w", err)
	}
	if !expExists {
		_ = tx.Rollback()
		return nil, fmt.Errorf("experiment %s not found", experimentID)
	}

	br := &BatchResult{}
	var acceptedEventIDs []uuid.UUID

	for _, in := range inputs {
		eid, err := uuid.Parse(in.EventID)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("invalid event_id %s: %w", in.EventID, err)
		}
		uid, err := uuid.Parse(in.UserID)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("invalid user_id %s: %w", in.UserID, err)
		}
		euid, err := uuid.Parse(in.ExperimentID)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("invalid experiment_id %s: %w", in.ExperimentID, err)
		}
		if uid != userID || euid != experimentID {
			_ = tx.Rollback()
			return nil, fmt.Errorf("event user/experiment mismatch")
		}

		ev := &models.Event{
			ID:           eid,
			UserID:       uid,
			ExperimentID: euid,
			DeviceID:     in.DeviceID,
			ClientSeq:    in.ClientSeq,
			EventType:    in.EventType,
			OccurredAt:   in.OccurredAt.UTC(),
			ReceivedAt:   time.Now().UTC(),
			Payload:      in.Payload,
		}
		if ev.Payload == nil {
			ev.Payload = json.RawMessage(`{}`)
		}

		res, err := p.events.InsertEvent(ctx, tx, ev)
		if err != nil {
			return nil, fmt.Errorf("insert event: %w", err)
		}
		if res.IsDuplicate {
			br.Duplicate++
		} else {
			br.Accepted++
			acceptedEventIDs = append(acceptedEventIDs, eid)
		}
	}

	br.Version, err = p.computeResults(ctx, tx, userID, experimentID)
	if err != nil {
		return nil, fmt.Errorf("compute results: %w", err)
	}

	for _, eid := range acceptedEventIDs {
		if err := p.events.MarkEventVersion(ctx, tx, eid, br.Version); err != nil {
			return nil, fmt.Errorf("mark event version: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return br, nil
}

func (p *EventProcessor) computeResults(ctx context.Context, tx *sql.Tx, userID, experimentID uuid.UUID) (int, error) {
	user, err := p.users.GetByID(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("get user: %w", err)
	}
	exp, err := p.experiments.GetByID(ctx, experimentID)
	if err != nil {
		return 0, fmt.Errorf("get experiment: %w", err)
	}

	tz, err := time.LoadLocation(user.Timezone)
	if err != nil {
		tz = time.UTC
	}

	now := time.Now().UTC()
	ageGroup := AgeGroupFromBirthDate(user.BirthDate, now, tz)

	var bedtimeStr string
	if user.Bedtime != "" {
		bedtimeStr = user.Bedtime
	}

	allEvents, err := p.events.GetEventsForUserExperiment(ctx, tx, userID, experimentID, exp.Version+1000000)
	if err != nil {
		return 0, fmt.Errorf("get events: %w", err)
	}

	days := groupByUserDate(allEvents, tz)

	newVersion := exp.Version + 1
	if len(allEvents) == 0 {
		return newVersion, nil
	}

	latestVersion, err := p.results.GetLatestVersion(ctx, userID, experimentID)
	if err != nil {
		return 0, err
	}
	if latestVersion >= newVersion {
		newVersion = latestVersion + 1
	}

	_, err = tx.ExecContext(ctx, `SELECT 1`)
	if err != nil {
		return 0, err
	}

	var totalDuration int64
	var totalCardViews, totalSwitches, totalSlowReading, totalWatching int
	var totalViolations int
	computedAt := time.Now().UTC()

	for dateStr, dayEvents := range days {
		userDate, _ := time.ParseInLocation("2006-01-02", dateStr, tz)
		userDate = time.Date(userDate.Year(), userDate.Month(), userDate.Day(), 0, 0, 0, 0, tz)

		eval := p.rules.EvaluateDay(dayEvents, userDate, ageGroup, bedtimeStr, tz)
		totalDuration += eval.Aggregate.TotalDuration
		totalCardViews += eval.Aggregate.CardViews
		totalSwitches += eval.Aggregate.AttentionSwitches
		totalSlowReading += eval.Aggregate.SlowReading
		totalWatching += eval.Aggregate.WatchingSessions
		totalViolations += len(eval.Violations)

		violJSON, _ := json.Marshal(eval.Violations)
		agg := &models.DailyAggregate{
			UserID:                userID,
			ExperimentID:          experimentID,
			UserDate:              userDate,
			TotalDurationSeconds:  eval.Aggregate.TotalDuration,
			SessionCount:          eval.Aggregate.SessionCount,
			LongestSessionSeconds: eval.Aggregate.LongestSession,
			CardViewCount:         eval.Aggregate.CardViews,
			AttentionSwitchCount:  eval.Aggregate.AttentionSwitches,
			SlowReadingCount:      eval.Aggregate.SlowReading,
			WatchingSessionCount:  eval.Aggregate.WatchingSessions,
			Violations:            violJSON,
			Version:               newVersion,
			ComputedAt:            computedAt,
		}
		if err := p.results.SaveDailyAggregate(ctx, tx, agg); err != nil {
			return 0, fmt.Errorf("save daily aggregate: %w", err)
		}
	}

	resultJSON, _ := json.Marshal(map[string]interface{}{
		"age_group":             string(ageGroup),
		"age_at_computation":    CalculateAge(user.BirthDate, now, tz),
		"timezone":              user.Timezone,
		"total_days":            len(days),
	})

	res := &models.ExperimentResult{
		UserID:                 userID,
		ExperimentID:           experimentID,
		Version:                newVersion,
		ResultJSON:             resultJSON,
		TotalDurationSeconds:   totalDuration,
		TotalCardViews:         totalCardViews,
		TotalAttentionSwitches: totalSwitches,
		TotalSlowReading:       totalSlowReading,
		TotalWatchingSessions:  totalWatching,
		ViolationCount:         totalViolations,
		ComputedAt:             computedAt,
	}
	if err := p.results.SaveExperimentResult(ctx, tx, res); err != nil {
		return 0, fmt.Errorf("save result: %w", err)
	}

	return newVersion, nil
}

func groupByUserDate(events []models.Event, tz *time.Location) map[string][]models.Event {
	days := make(map[string][]models.Event)
	for _, e := range events {
		local := e.OccurredAt.In(tz)
		key := local.Format("2006-01-02")
		days[key] = append(days[key], e)
	}
	return days
}

func (p *EventProcessor) RecalculateVersion(ctx context.Context, userID, experimentID uuid.UUID, targetVersion int) error {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	user, err := p.users.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	tz, err := time.LoadLocation(user.Timezone)
	if err != nil {
		tz = time.UTC
	}
	now := time.Now().UTC()
	ageGroup := AgeGroupFromBirthDate(user.BirthDate, now, tz)
	bedtimeStr := user.Bedtime

	events, err := p.events.GetEventsForUserExperiment(ctx, tx, userID, experimentID, targetVersion)
	if err != nil {
		return fmt.Errorf("get events: %w", err)
	}

	_ = p.results.DeleteVersion(ctx, tx, userID, experimentID, targetVersion)

	days := groupByUserDate(events, tz)
	computedAt := time.Now().UTC()
	var totalDuration int64
	var totalCardViews, totalSwitches, totalSlowReading, totalWatching int
	var totalViolations int

	for dateStr, dayEvents := range days {
		userDate, _ := time.ParseInLocation("2006-01-02", dateStr, tz)
		userDate = time.Date(userDate.Year(), userDate.Month(), userDate.Day(), 0, 0, 0, 0, tz)
		eval := p.rules.EvaluateDay(dayEvents, userDate, ageGroup, bedtimeStr, tz)
		totalDuration += eval.Aggregate.TotalDuration
		totalCardViews += eval.Aggregate.CardViews
		totalSwitches += eval.Aggregate.AttentionSwitches
		totalSlowReading += eval.Aggregate.SlowReading
		totalWatching += eval.Aggregate.WatchingSessions
		totalViolations += len(eval.Violations)

		violJSON, _ := json.Marshal(eval.Violations)
		agg := &models.DailyAggregate{
			UserID:                userID,
			ExperimentID:          experimentID,
			UserDate:              userDate,
			TotalDurationSeconds:  eval.Aggregate.TotalDuration,
			SessionCount:          eval.Aggregate.SessionCount,
			LongestSessionSeconds: eval.Aggregate.LongestSession,
			CardViewCount:         eval.Aggregate.CardViews,
			AttentionSwitchCount:  eval.Aggregate.AttentionSwitches,
			SlowReadingCount:      eval.Aggregate.SlowReading,
			WatchingSessionCount:  eval.Aggregate.WatchingSessions,
			Violations:            violJSON,
			Version:               targetVersion,
			ComputedAt:            computedAt,
		}
		if err := p.results.SaveDailyAggregate(ctx, tx, agg); err != nil {
			return fmt.Errorf("save daily aggregate: %w", err)
		}
	}

	resultJSON, _ := json.Marshal(map[string]interface{}{
		"age_group":          string(ageGroup),
		"age_at_computation": CalculateAge(user.BirthDate, now, tz),
		"timezone":           user.Timezone,
		"total_days":         len(days),
		"recalculated":       true,
		"target_version":     targetVersion,
	})

	res := &models.ExperimentResult{
		UserID:                 userID,
		ExperimentID:           experimentID,
		Version:                targetVersion,
		ResultJSON:             resultJSON,
		TotalDurationSeconds:   totalDuration,
		TotalCardViews:         totalCardViews,
		TotalAttentionSwitches: totalSwitches,
		TotalSlowReading:       totalSlowReading,
		TotalWatchingSessions:  totalWatching,
		ViolationCount:         totalViolations,
		ComputedAt:             computedAt,
	}
	if err := p.results.SaveExperimentResult(ctx, tx, res); err != nil {
		return fmt.Errorf("save result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (p *EventProcessor) ReplayAll(ctx context.Context, userID, experimentID uuid.UUID) (int, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	user, err := p.users.GetByID(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("get user: %w", err)
	}
	tz, err := time.LoadLocation(user.Timezone)
	if err != nil {
		tz = time.UTC
	}
	now := time.Now().UTC()
	ageGroup := AgeGroupFromBirthDate(user.BirthDate, now, tz)
	bedtimeStr := user.Bedtime

	events, err := p.events.GetAllEventsForReplay(ctx, userID, experimentID)
	if err != nil {
		return 0, fmt.Errorf("get events: %w", err)
	}

	days := groupByUserDate(events, tz)

	latestVersion, err := p.results.GetLatestVersion(ctx, userID, experimentID)
	if err != nil {
		return 0, err
	}
	newVersion := latestVersion + 1

	computedAt := time.Now().UTC()
	var totalDuration int64
	var totalCardViews, totalSwitches, totalSlowReading, totalWatching int
	var totalViolations int

	for dateStr, dayEvents := range days {
		userDate, _ := time.ParseInLocation("2006-01-02", dateStr, tz)
		userDate = time.Date(userDate.Year(), userDate.Month(), userDate.Day(), 0, 0, 0, 0, tz)
		eval := p.rules.EvaluateDay(dayEvents, userDate, ageGroup, bedtimeStr, tz)
		totalDuration += eval.Aggregate.TotalDuration
		totalCardViews += eval.Aggregate.CardViews
		totalSwitches += eval.Aggregate.AttentionSwitches
		totalSlowReading += eval.Aggregate.SlowReading
		totalWatching += eval.Aggregate.WatchingSessions
		totalViolations += len(eval.Violations)

		violJSON, _ := json.Marshal(eval.Violations)
		agg := &models.DailyAggregate{
			UserID:                userID,
			ExperimentID:          experimentID,
			UserDate:              userDate,
			TotalDurationSeconds:  eval.Aggregate.TotalDuration,
			SessionCount:          eval.Aggregate.SessionCount,
			LongestSessionSeconds: eval.Aggregate.LongestSession,
			CardViewCount:         eval.Aggregate.CardViews,
			AttentionSwitchCount:  eval.Aggregate.AttentionSwitches,
			SlowReadingCount:      eval.Aggregate.SlowReading,
			WatchingSessionCount:  eval.Aggregate.WatchingSessions,
			Violations:            violJSON,
			Version:               newVersion,
			ComputedAt:            computedAt,
		}
		if err := p.results.SaveDailyAggregate(ctx, tx, agg); err != nil {
			return 0, err
		}
	}

	resultJSON, _ := json.Marshal(map[string]interface{}{
		"age_group":          string(ageGroup),
		"age_at_computation": CalculateAge(user.BirthDate, now, tz),
		"timezone":           user.Timezone,
		"total_days":         len(days),
		"replayed":           true,
	})

	res := &models.ExperimentResult{
		UserID:                 userID,
		ExperimentID:           experimentID,
		Version:                newVersion,
		ResultJSON:             resultJSON,
		TotalDurationSeconds:   totalDuration,
		TotalCardViews:         totalCardViews,
		TotalAttentionSwitches: totalSwitches,
		TotalSlowReading:       totalSlowReading,
		TotalWatchingSessions:  totalWatching,
		ViolationCount:         totalViolations,
		ComputedAt:             computedAt,
	}
	if err := p.results.SaveExperimentResult(ctx, tx, res); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVersion, nil
}
