package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"brainbreak-lab/focus/internal/model"
)

type ExperimentInput struct {
	SubjectID uuid.UUID
	Label     string
}

func (db *DB) CreateExperiment(ctx context.Context, in ExperimentInput) (*model.Experiment, error) {
	e := &model.Experiment{
		ID:        uuid.New(),
		SubjectID: in.SubjectID,
		Label:     in.Label,
		Status:    "open",
		CreatedAt: time.Now().UTC(),
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO experiments(id,subject_id,label,status,created_at)
		VALUES ($1,$2,$3,$4,$5)`,
		e.ID, e.SubjectID, e.Label, e.Status, e.CreatedAt)
	if err != nil {
		return nil, mapError(err)
	}
	return e, nil
}

func (db *DB) GetExperiment(ctx context.Context, id uuid.UUID) (*model.Experiment, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT id,subject_id,label,status,created_at,closed_at FROM experiments WHERE id=$1`, id)
	var e model.Experiment
	err := row.Scan(&e.ID, &e.SubjectID, &e.Label, &e.Status, &e.CreatedAt, &e.ClosedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &e, nil
}

// lockExperimentForWrite 在事务中锁定实验并同时锁定受试者写入状态。
func lockExperimentForWrite(ctx context.Context, tx pgx.Tx, experimentID uuid.UUID) (*model.Experiment, *model.Subject, *time.Location, error) {
	exp := &model.Experiment{}
	err := tx.QueryRow(ctx, `
		SELECT id,subject_id,label,status,created_at,closed_at FROM experiments WHERE id=$1 FOR UPDATE`, experimentID).
		Scan(&exp.ID, &exp.SubjectID, &exp.Label, &exp.Status, &exp.CreatedAt, &exp.ClosedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil, ErrNotFound
		}
		return nil, nil, nil, err
	}
	sub, loc, err := verifySubjectWritable(ctx, tx, exp.SubjectID)
	if err != nil {
		return nil, nil, nil, err
	}
	return exp, sub, loc, nil
}
