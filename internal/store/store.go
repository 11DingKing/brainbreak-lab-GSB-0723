package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// 导出的哨兵错误。
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrValidation    = errors.New("validation")
	ErrConsent       = errors.New("consent withdrawn")
	ErrDeleted       = errors.New("subject deleted")
	ErrSerialization = errors.New("serialization failure")
)

// DB 封装连接池。
type DB struct {
	pool *pgxpool.Pool
	// testHook 仅在测试中注入；生产为 nil。
	testHook func(ctx context.Context, phase string) error
}

func New(pool *pgxpool.Pool) *DB {
	return &DB{pool: pool}
}

func (db *DB) Pool() *pgxpool.Pool { return db.pool }

func (db *DB) Close() { db.pool.Close() }

// SetTestHook 注入故障钩子，phase 标识事务内的检查点。
func (db *DB) SetTestHook(h func(ctx context.Context, phase string) error) {
	db.testHook = h
}

func (db *DB) hook(ctx context.Context, phase string) error {
	if db.testHook == nil {
		return nil
	}
	return db.testHook(ctx, phase)
}

// Migrate 按词法顺序执行 migrations/*.sql。
func (db *DB) Migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		if _, err := db.pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
	}
	return nil
}

// tx 执行 fn。使用 ReadCommitted 并在写入路径上通过 SELECT ... FOR UPDATE 串行化
// 同实验/同受试者的写入；唯一约束保证事件和幂等键去重。
func (db *DB) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	opts := pgx.TxOptions{IsoLevel: pgx.ReadCommitted}
	tx, err := db.pool.BeginTx(ctx, opts)
	if err != nil {
		return mapError(err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// RetrySerialize 以指数退避（带抖动）重试可序列化冲突。
func (db *DB) RetrySerialize(ctx context.Context, attempts int, fn func(ctx context.Context) error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		if !errors.Is(lastErr, ErrSerialization) {
			return lastErr
		}
		if i == attempts-1 {
			return lastErr
		}
		ms := 5 * (1 << i) // 5,10,20,40,80,160 ms
		if ms > 200 {
			ms = 200
		}
		t := time.NewTimer(time.Duration(ms) * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return lastErr
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505": // unique_violation
			return ErrConflict
		case "23503": // foreign_key_violation
			return ErrValidation
		case "40001", "40P01":
			return ErrSerialization
		}
	}
	return err
}

func isSerialization(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "40001" || pg.Code == "40P01"
	}
	return errors.Is(err, ErrSerialization)
}
