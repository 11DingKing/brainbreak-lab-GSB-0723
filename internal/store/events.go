package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"brainbreak-lab/internal/models"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrDuplicateEvent = errors.New("duplicate event")

type EventStore struct {
	db *sql.DB
}

func NewEventStore(db *sql.DB) *EventStore {
	return &EventStore{db: db}
}

type InsertResult struct {
	Accepted bool
	IsDuplicate bool
}

func (s *EventStore) InsertEvent(ctx context.Context, tx *sql.Tx, e *models.Event) (*InsertResult, error) {
	if e.Payload == nil {
		e.Payload = json.RawMessage(`{}`)
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO raw_events (event_id, user_id, experiment_id, device_id, client_seq, event_type, occurred_at, received_at, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (event_id) DO NOTHING
		 RETURNING id`,
		e.ID, e.UserID, e.ExperimentID, e.DeviceID, e.ClientSeq, string(e.EventType), e.OccurredAt, e.ReceivedAt, e.Payload,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &InsertResult{Accepted: false, IsDuplicate: true}, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &InsertResult{Accepted: false, IsDuplicate: true}, nil
		}
		return nil, err
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO event_ingestion_log (event_id, user_id, experiment_id, accepted, version)
		 VALUES ($1, $2, $3, true, 0)
		 ON CONFLICT (event_id) DO UPDATE SET accepted = true`,
		e.ID, e.UserID, e.ExperimentID,
	)
	if err != nil {
		return nil, err
	}
	return &InsertResult{Accepted: true, IsDuplicate: false}, nil
}

func (s *EventStore) MarkEventVersion(ctx context.Context, tx *sql.Tx, eventID uuid.UUID, version int) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE event_ingestion_log SET version = $1 WHERE event_id = $2`,
		version, eventID,
	)
	return err
}

func (s *EventStore) GetEventsForUserExperiment(ctx context.Context, tx *sql.Tx, userID, experimentID uuid.UUID, maxVersion int) ([]models.Event, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.event_id, r.user_id, r.experiment_id, r.device_id, r.client_seq, r.event_type, r.occurred_at, r.received_at, r.payload
		 FROM raw_events r
		 JOIN event_ingestion_log l ON r.event_id = l.event_id
		 WHERE r.user_id = $1 AND r.experiment_id = $2 AND l.version <= $3
		 ORDER BY r.occurred_at ASC, r.client_seq ASC, r.received_at ASC`,
		userID, experimentID, maxVersion,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []models.Event
	for rows.Next() {
		var e models.Event
		var et string
		if err := rows.Scan(&e.ID, &e.UserID, &e.ExperimentID, &e.DeviceID, &e.ClientSeq, &et, &e.OccurredAt, &e.ReceivedAt, &e.Payload); err != nil {
			return nil, err
		}
		e.EventType = models.EventType(et)
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *EventStore) GetAllEventsForReplay(ctx context.Context, userID, experimentID uuid.UUID) ([]models.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, user_id, experiment_id, device_id, client_seq, event_type, occurred_at, received_at, payload
		 FROM raw_events
		 WHERE user_id = $1 AND experiment_id = $2
		 ORDER BY occurred_at ASC, client_seq ASC, received_at ASC`,
		userID, experimentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []models.Event
	for rows.Next() {
		var e models.Event
		var et string
		if err := rows.Scan(&e.ID, &e.UserID, &e.ExperimentID, &e.DeviceID, &e.ClientSeq, &et, &e.OccurredAt, &e.ReceivedAt, &e.Payload); err != nil {
			return nil, err
		}
		e.EventType = models.EventType(et)
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *EventStore) GetUnversionedEvents(ctx context.Context, tx *sql.Tx, userID, experimentID uuid.UUID) ([]models.Event, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.event_id, r.user_id, r.experiment_id, r.device_id, r.client_seq, r.event_type, r.occurred_at, r.received_at, r.payload
		 FROM raw_events r
		 JOIN event_ingestion_log l ON r.event_id = l.event_id
		 WHERE r.user_id = $1 AND r.experiment_id = $2 AND l.version = 0
		 ORDER BY r.occurred_at ASC, r.client_seq ASC, r.received_at ASC`,
		userID, experimentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []models.Event
	for rows.Next() {
		var e models.Event
		var et string
		if err := rows.Scan(&e.ID, &e.UserID, &e.ExperimentID, &e.DeviceID, &e.ClientSeq, &et, &e.OccurredAt, &e.ReceivedAt, &e.Payload); err != nil {
			return nil, err
		}
		e.EventType = models.EventType(et)
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *EventStore) DeleteAllForUser(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM event_ingestion_log WHERE user_id = $1`, userID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM raw_events WHERE user_id = $1`, userID)
	return err
}

func (s *EventStore) DeleteAllForExperiment(ctx context.Context, tx *sql.Tx, experimentID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM event_ingestion_log WHERE experiment_id = $1`, experimentID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM raw_events WHERE experiment_id = $1`, experimentID)
	return err
}
