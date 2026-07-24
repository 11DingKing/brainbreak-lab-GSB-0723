package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"brainbreak-lab/internal/models"

	"github.com/google/uuid"
)

type ExperimentStore struct {
	db *sql.DB
}

func NewExperimentStore(db *sql.DB) *ExperimentStore {
	return &ExperimentStore{db: db}
}

func (s *ExperimentStore) Create(ctx context.Context, name string, config json.RawMessage) (*models.Experiment, error) {
	id := uuid.New()
	now := time.Now().UTC()
	if config == nil {
		config = json.RawMessage(`{}`)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO experiments (id, version, name, config, created_at) VALUES ($1, 1, $2, $3, $4)`,
		id, name, config, now,
	)
	if err != nil {
		return nil, err
	}
	return &models.Experiment{
		ID:        id,
		Version:   1,
		Name:      name,
		Config:    config,
		CreatedAt: now,
	}, nil
}

func (s *ExperimentStore) GetByID(ctx context.Context, id uuid.UUID) (*models.Experiment, error) {
	e := &models.Experiment{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, version, name, config, created_at FROM experiments WHERE id = $1`, id,
	).Scan(&e.ID, &e.Version, &e.Name, &e.Config, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (s *ExperimentStore) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM experiments WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

func (s *ExperimentStore) IncrementVersion(ctx context.Context, tx *sql.Tx, id uuid.UUID) (int, error) {
	var newVersion int
	err := tx.QueryRowContext(ctx,
		`UPDATE experiments SET version = version + 1 WHERE id = $1 RETURNING version`, id,
	).Scan(&newVersion)
	return newVersion, err
}
