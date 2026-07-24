package store

import (
	"context"
	"database/sql"
	"time"

	"brainbreak-lab/internal/models"

	"github.com/google/uuid"
)

type UserStore struct {
	db *sql.DB
}

func NewUserStore(db *sql.DB) *UserStore {
	return &UserStore{db: db}
}

func (s *UserStore) Create(ctx context.Context, birthDate time.Time, tz string, bedtime string) (*models.User, error) {
	id := uuid.New()
	now := time.Now().UTC()
	var bedtimeArg interface{}
	if bedtime != "" {
		bedtimeArg = bedtime
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, birth_date, timezone, bedtime, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		id, birthDate, tz, bedtimeArg, now,
	)
	if err != nil {
		return nil, err
	}
	return &models.User{
		ID:        id,
		BirthDate: birthDate,
		Timezone:  tz,
		Bedtime:   bedtime,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (s *UserStore) GetByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
	u := &models.User{}
	var bedtime sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, birth_date, timezone, bedtime, created_at, updated_at FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.BirthDate, &u.Timezone, &bedtime, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if bedtime.Valid {
		u.Bedtime = bedtime.String
	}
	return u, nil
}

func (s *UserStore) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}
