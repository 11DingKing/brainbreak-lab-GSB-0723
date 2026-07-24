package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"brainbreak-lab/internal/models"

	"github.com/google/uuid"
)

type ResultStore struct {
	db *sql.DB
}

func NewResultStore(db *sql.DB) *ResultStore {
	return &ResultStore{db: db}
}

func (s *ResultStore) SaveDailyAggregate(ctx context.Context, tx *sql.Tx, agg *models.DailyAggregate) error {
	if agg.Violations == nil {
		agg.Violations = json.RawMessage(`[]`)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO daily_aggregates
		   (user_id, experiment_id, user_date, total_duration_seconds, session_count,
		    longest_session_seconds, card_view_count, attention_switch_count, slow_reading_count,
		    watching_session_count, violations, version, computed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		 ON CONFLICT (user_id, experiment_id, user_date, version) DO UPDATE SET
		   total_duration_seconds = EXCLUDED.total_duration_seconds,
		   session_count = EXCLUDED.session_count,
		   longest_session_seconds = EXCLUDED.longest_session_seconds,
		   card_view_count = EXCLUDED.card_view_count,
		   attention_switch_count = EXCLUDED.attention_switch_count,
		   slow_reading_count = EXCLUDED.slow_reading_count,
		   watching_session_count = EXCLUDED.watching_session_count,
		   violations = EXCLUDED.violations,
		   computed_at = EXCLUDED.computed_at`,
		agg.UserID, agg.ExperimentID, agg.UserDate, agg.TotalDurationSeconds, agg.SessionCount,
		agg.LongestSessionSeconds, agg.CardViewCount, agg.AttentionSwitchCount, agg.SlowReadingCount,
		agg.WatchingSessionCount, agg.Violations, agg.Version, agg.ComputedAt,
	)
	return err
}

func (s *ResultStore) SaveExperimentResult(ctx context.Context, tx *sql.Tx, r *models.ExperimentResult) error {
	if r.ResultJSON == nil {
		r.ResultJSON = json.RawMessage(`{}`)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO experiment_results
		   (user_id, experiment_id, version, result_json, total_duration_seconds,
		    total_card_views, total_attention_switches, total_slow_reading, total_watching_sessions,
		    violation_count, computed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (user_id, experiment_id, version) DO UPDATE SET
		   result_json = EXCLUDED.result_json,
		   total_duration_seconds = EXCLUDED.total_duration_seconds,
		   total_card_views = EXCLUDED.total_card_views,
		   total_attention_switches = EXCLUDED.total_attention_switches,
		   total_slow_reading = EXCLUDED.total_slow_reading,
		   total_watching_sessions = EXCLUDED.total_watching_sessions,
		   violation_count = EXCLUDED.violation_count,
		   computed_at = EXCLUDED.computed_at`,
		r.UserID, r.ExperimentID, r.Version, r.ResultJSON, r.TotalDurationSeconds,
		r.TotalCardViews, r.TotalAttentionSwitches, r.TotalSlowReading, r.TotalWatchingSessions,
		r.ViolationCount, r.ComputedAt,
	)
	return err
}

func (s *ResultStore) GetResult(ctx context.Context, userID, experimentID uuid.UUID, version int) (*models.ExperimentResult, error) {
	r := &models.ExperimentResult{}
	var err error
	if version <= 0 {
		err = s.db.QueryRowContext(ctx,
			`SELECT user_id, experiment_id, version, result_json, total_duration_seconds,
			        total_card_views, total_attention_switches, total_slow_reading, total_watching_sessions,
			        violation_count, computed_at
			 FROM experiment_results
			 WHERE user_id = $1 AND experiment_id = $2
			 ORDER BY version DESC LIMIT 1`,
			userID, experimentID,
		).Scan(&r.UserID, &r.ExperimentID, &r.Version, &r.ResultJSON, &r.TotalDurationSeconds,
			&r.TotalCardViews, &r.TotalAttentionSwitches, &r.TotalSlowReading, &r.TotalWatchingSessions,
			&r.ViolationCount, &r.ComputedAt)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT user_id, experiment_id, version, result_json, total_duration_seconds,
			        total_card_views, total_attention_switches, total_slow_reading, total_watching_sessions,
			        violation_count, computed_at
			 FROM experiment_results
			 WHERE user_id = $1 AND experiment_id = $2 AND version = $3`,
			userID, experimentID, version,
		).Scan(&r.UserID, &r.ExperimentID, &r.Version, &r.ResultJSON, &r.TotalDurationSeconds,
			&r.TotalCardViews, &r.TotalAttentionSwitches, &r.TotalSlowReading, &r.TotalWatchingSessions,
			&r.ViolationCount, &r.ComputedAt)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s *ResultStore) GetDailyAggregates(ctx context.Context, userID, experimentID uuid.UUID, version int) ([]models.DailyAggregate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, experiment_id, user_date, total_duration_seconds, session_count,
		        longest_session_seconds, card_view_count, attention_switch_count, slow_reading_count,
		        watching_session_count, violations, version, computed_at
		 FROM daily_aggregates
		 WHERE user_id = $1 AND experiment_id = $2 AND version = $3
		 ORDER BY user_date ASC`,
		userID, experimentID, version,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var aggs []models.DailyAggregate
	for rows.Next() {
		var a models.DailyAggregate
		if err := rows.Scan(&a.UserID, &a.ExperimentID, &a.UserDate, &a.TotalDurationSeconds, &a.SessionCount,
			&a.LongestSessionSeconds, &a.CardViewCount, &a.AttentionSwitchCount, &a.SlowReadingCount,
			&a.WatchingSessionCount, &a.Violations, &a.Version, &a.ComputedAt); err != nil {
			return nil, err
		}
		aggs = append(aggs, a)
	}
	return aggs, rows.Err()
}

func (s *ResultStore) GetLatestVersion(ctx context.Context, userID, experimentID uuid.UUID) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM experiment_results WHERE user_id = $1 AND experiment_id = $2`,
		userID, experimentID,
	).Scan(&version)
	return version, err
}

func (s *ResultStore) DeleteAllForUser(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM daily_aggregates WHERE user_id = $1`, userID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM experiment_results WHERE user_id = $1`, userID)
	return err
}

func (s *ResultStore) DeleteAllForExperiment(ctx context.Context, tx *sql.Tx, experimentID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM daily_aggregates WHERE experiment_id = $1`, experimentID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM experiment_results WHERE experiment_id = $1`, experimentID)
	return err
}

func (s *ResultStore) DeleteVersion(ctx context.Context, tx *sql.Tx, userID, experimentID uuid.UUID, version int) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM daily_aggregates WHERE user_id = $1 AND experiment_id = $2 AND version >= $3`,
		userID, experimentID, version); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM experiment_results WHERE user_id = $1 AND experiment_id = $2 AND version >= $3`,
		userID, experimentID, version)
	return err
}
