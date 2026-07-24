package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"brainbreak-lab/focus/internal/model"
)

type SubjectInput struct {
	DateOfBirth time.Time
	Timezone    string
	Bedtime     *time.Time
}

func (db *DB) CreateSubject(ctx context.Context, in SubjectInput) (*model.Subject, error) {
	now := time.Now().UTC()
	s := &model.Subject{
		ID:          uuid.New(),
		DateOfBirth: in.DateOfBirth.UTC(),
		Timezone:    in.Timezone,
		Bedtime:     in.Bedtime,
		ConsentAt:   now,
		CreatedAt:   now,
	}
	var bedtime any
	if in.Bedtime != nil {
		bedtime = in.Bedtime.Format("15:04")
	}
	_, err := db.pool.Exec(ctx, `
		INSERT INTO subjects(id,date_of_birth,timezone,bedtime,consent_at,created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		s.ID, s.DateOfBirth, s.Timezone, bedtime, s.ConsentAt, s.CreatedAt)
	if err != nil {
		return nil, mapError(err)
	}
	return s, nil
}

// GetSubject 取回受试者；若已删除返回 ErrDeleted。
func (db *DB) GetSubject(ctx context.Context, id uuid.UUID) (*model.Subject, error) {
	row := db.pool.QueryRow(ctx, `
		SELECT id,date_of_birth,timezone,bedtime,consent_at,withdrawn_at,deleted_at,created_at
		FROM subjects WHERE id=$1`, id)
	return scanSubject(row.Scan)
}

func scanSubject(scan func(dest ...any) error) (*model.Subject, error) {
	var s model.Subject
	var bedtimeStr *string
	err := scan(&s.ID, &s.DateOfBirth, &s.Timezone, &bedtimeStr, &s.ConsentAt, &s.WithdrawnAt, &s.DeletedAt, &s.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if s.DeletedAt != nil {
		return nil, ErrDeleted
	}
	if bedtimeStr != nil {
		if t, err := time.Parse("15:04", *bedtimeStr); err == nil {
			s.Bedtime = &t
		}
	}
	return &s, nil
}

// WithdrawConsent 标记撤回授权，拒绝后续事件写入。
func (db *DB) WithdrawConsent(ctx context.Context, id uuid.UUID) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE subjects SET withdrawn_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		_ = db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subjects WHERE id=$1)`, id).Scan(&exists)
		if !exists {
			return ErrNotFound
		}
		return ErrDeleted
	}
	return nil
}

// DeleteSubjectPermanently 彻底删除受试者及其全部派生数据。
// 不在任何派生表中保留可识别身份的信息；仅写一条无 subject_id 的审计记录。
func (db *DB) DeleteSubjectPermanently(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	token := uuid.New()
	err := db.tx(ctx, func(tx pgx.Tx) error {
		// 确认存在且未删除；锁定行。
		var deletedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT deleted_at FROM subjects WHERE id=$1 FOR UPDATE`, id).Scan(&deletedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if deletedAt != nil {
			return ErrDeleted
		}
		// ON DELETE CASCADE 会清空 events/ingest_batches/daily_usage/violations/results/experiments。
		if _, err := tx.Exec(ctx, `DELETE FROM subjects WHERE id=$1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO deletion_audit(token,note) VALUES($1,'subject deleted')`, token)
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}
	return token, nil
}

// verifySubjectWritable 在写入事件前确认状态；返回 subject 或敏感错误。
func verifySubjectWritable(ctx context.Context, q pgx.Tx, id uuid.UUID) (*model.Subject, *time.Location, error) {
	row := q.QueryRow(ctx, `
		SELECT id,date_of_birth,timezone,bedtime,consent_at,withdrawn_at,deleted_at,created_at
		FROM subjects WHERE id=$1 FOR UPDATE`, id)
	s, err := scanSubject(row.Scan)
	if err != nil {
		return nil, nil, err
	}
	if s.WithdrawnAt != nil {
		return nil, nil, ErrConsent
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return nil, nil, ErrValidation
	}
	return s, loc, nil
}
