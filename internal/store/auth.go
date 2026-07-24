package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

type AuthStore struct {
	db *sql.DB
}

func NewAuthStore(db *sql.DB) *AuthStore {
	return &AuthStore{db: db}
}

func (s *AuthStore) Grant(ctx context.Context, userID, experimentID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO authorization_grants (user_id, experiment_id, granted_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, experiment_id) DO UPDATE SET revoked_at = NULL, granted_at = $3`,
		userID, experimentID, time.Now().UTC(),
	)
	return err
}

func (s *AuthStore) Revoke(ctx context.Context, userID, experimentID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE authorization_grants SET revoked_at = $1 WHERE user_id = $2 AND experiment_id = $3 AND revoked_at IS NULL`,
		time.Now().UTC(), userID, experimentID,
	)
	return err
}

func (s *AuthStore) IsAuthorized(ctx context.Context, userID, experimentID uuid.UUID) (bool, error) {
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT revoked_at FROM authorization_grants WHERE user_id = $1 AND experiment_id = $2`,
		userID, experimentID,
	).Scan(&revokedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !revokedAt.Valid, nil
}

func (s *AuthStore) DeleteAllForUser(ctx context.Context, tx *sql.Tx, userID uuid.UUID) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM authorization_grants WHERE user_id = $1`, userID)
	return err
}

type DeletionStore struct {
	db *sql.DB
}

func NewDeletionStore(db *sql.DB) *DeletionStore {
	return &DeletionStore{db: db}
}

func hashScope(id uuid.UUID) string {
	h := sha256.Sum256([]byte(id.String()))
	return hex.EncodeToString(h[:])
}

func (s *DeletionStore) RecordDeletion(ctx context.Context, tx *sql.Tx, scope string, scopeID uuid.UUID) error {
	id := uuid.New()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO deletion_records (id, deleted_at, scope, scope_hash) VALUES ($1, $2, $3, $4)`,
		id, time.Now().UTC(), scope, hashScope(scopeID),
	)
	return err
}
