package service

import (
	"context"
	"database/sql"
	"fmt"

	"brainbreak-lab/internal/store"

	"github.com/google/uuid"
)

type DeletionService struct {
	db       *sql.DB
	events   *store.EventStore
	results  *store.ResultStore
	auth     *store.AuthStore
	deletion *store.DeletionStore
}

func NewDeletionService(db *sql.DB, events *store.EventStore, results *store.ResultStore, auth *store.AuthStore, deletion *store.DeletionStore) *DeletionService {
	return &DeletionService{db: db, events: events, results: results, auth: auth, deletion: deletion}
}

func (s *DeletionService) HardDeleteUser(ctx context.Context, userID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err := s.results.DeleteAllForUser(ctx, tx, userID); err != nil {
		return fmt.Errorf("delete results: %w", err)
	}
	if err := s.events.DeleteAllForUser(ctx, tx, userID); err != nil {
		return fmt.Errorf("delete events: %w", err)
	}
	if err := s.auth.DeleteAllForUser(ctx, tx, userID); err != nil {
		return fmt.Errorf("delete auth: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if err := s.deletion.RecordDeletion(ctx, tx, "user", userID); err != nil {
		return fmt.Errorf("record deletion: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *DeletionService) HardDeleteExperiment(ctx context.Context, experimentID uuid.UUID) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err := s.results.DeleteAllForExperiment(ctx, tx, experimentID); err != nil {
		return fmt.Errorf("delete results: %w", err)
	}
	if err := s.events.DeleteAllForExperiment(ctx, tx, experimentID); err != nil {
		return fmt.Errorf("delete events: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM authorization_grants WHERE experiment_id = $1`, experimentID); err != nil {
		return fmt.Errorf("delete auth grants: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM experiments WHERE id = $1`, experimentID); err != nil {
		return fmt.Errorf("delete experiment: %w", err)
	}
	if err := s.deletion.RecordDeletion(ctx, tx, "experiment", experimentID); err != nil {
		return fmt.Errorf("record deletion: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *DeletionService) RevokeAuthorization(ctx context.Context, userID, experimentID uuid.UUID) error {
	return s.auth.Revoke(ctx, userID, experimentID)
}
