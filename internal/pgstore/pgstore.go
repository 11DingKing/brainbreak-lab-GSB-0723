// Package pgstore is the PostgreSQL implementation of store.Store using pgx/v5.
// It upholds the service guarantees at the database level: idempotent event
// insertion via a UNIQUE constraint + ON CONFLICT DO NOTHING, atomic
// batch-write-plus-recompute via a single serializable-capable transaction, and
// crypto-shredding via destruction of the per-subject key plus cascading purge
// of derived rows.
package pgstore

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"focuslab/internal/cryptoshred"
	"focuslab/internal/domain"
	"focuslab/internal/store"
)

//go:embed schema.sql
var schemaSQL string

// PG is a PostgreSQL-backed store.
type PG struct {
	pool *pgxpool.Pool
}

// Open connects to the database at dsn and returns a store.
func Open(ctx context.Context, dsn string) (*PG, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PG{pool: pool}, nil
}

// Close releases the connection pool.
func (p *PG) Close() { p.pool.Close() }

// Migrate applies the schema (idempotent; safe to run on every startup).
func (p *PG) Migrate(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, schemaSQL)
	return err
}

// WithTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic. This is the atomicity boundary for batch write + recompute.
func (p *PG) WithTx(ctx context.Context, fn func(tx store.Tx) error) (err error) {
	pgtx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			_ = pgtx.Rollback(ctx)
			panic(r) // re-raise after rollback
		}
	}()
	t := &pgTx{tx: pgtx, ctx: ctx}
	if e := fn(t); e != nil {
		_ = pgtx.Rollback(ctx)
		return e
	}
	return pgtx.Commit(ctx)
}

// --- key store (crypto-shred) ---

func (p *PG) PutKey(subjectID uuid.UUID, key []byte) error {
	ctx := context.Background()
	var destroyed bool
	err := p.pool.QueryRow(ctx,
		`SELECT destroyed FROM subject_keys WHERE subject_id=$1`, subjectID).Scan(&destroyed)
	if err == nil && destroyed {
		return cryptoshred.ErrKeyDestroyed
	}
	_, err = p.pool.Exec(ctx,
		`INSERT INTO subject_keys (subject_id, key, destroyed) VALUES ($1,$2,false)
		 ON CONFLICT (subject_id) DO UPDATE SET key=EXCLUDED.key
		 WHERE subject_keys.destroyed=false`, subjectID, key)
	return err
}

func (p *PG) GetKey(subjectID uuid.UUID) ([]byte, error) {
	ctx := context.Background()
	var key []byte
	var destroyed bool
	err := p.pool.QueryRow(ctx,
		`SELECT key, destroyed FROM subject_keys WHERE subject_id=$1`, subjectID).Scan(&key, &destroyed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, cryptoshred.ErrKeyMissing
	}
	if err != nil {
		return nil, err
	}
	if destroyed {
		return nil, cryptoshred.ErrKeyDestroyed
	}
	return key, nil
}

func (p *PG) DestroyKey(subjectID uuid.UUID) error {
	ctx := context.Background()
	// Tombstone: null out the key material and mark destroyed irreversibly.
	_, err := p.pool.Exec(ctx,
		`INSERT INTO subject_keys (subject_id, key, destroyed) VALUES ($1,NULL,true)
		 ON CONFLICT (subject_id) DO UPDATE SET key=NULL, destroyed=true`, subjectID)
	return err
}

// pgTx implements store.Tx over a pgx.Tx.
type pgTx struct {
	tx  pgx.Tx
	ctx context.Context
}

func (t *pgTx) UpsertExperiment(ctx context.Context, e store.Experiment) error {
	_, err := t.tx.Exec(ctx,
		`INSERT INTO experiments (id, name, version, created_at) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (id) DO NOTHING`,
		e.ID, e.Name, e.Version, e.CreatedAt)
	return err
}

func (t *pgTx) GetExperiment(ctx context.Context, id uuid.UUID) (store.Experiment, error) {
	var e store.Experiment
	err := t.tx.QueryRow(ctx,
		`SELECT id, name, version, created_at FROM experiments WHERE id=$1`, id).
		Scan(&e.ID, &e.Name, &e.Version, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Experiment{}, store.ErrNotFound
	}
	return e, err
}

func (t *pgTx) SetExperimentVersion(ctx context.Context, id uuid.UUID, version int64) error {
	ct, err := t.tx.Exec(ctx, `UPDATE experiments SET version=$2 WHERE id=$1`, id, version)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *pgTx) UpsertSubject(ctx context.Context, s store.StoredSubject) error {
	// Refuse to resurrect a deleted subject.
	var auth string
	err := t.tx.QueryRow(ctx,
		`SELECT auth FROM subjects WHERE experiment_id=$1 AND id=$2`, s.ExperimentID, s.ID).Scan(&auth)
	if err == nil && auth == string(store.AuthDeleted) {
		return store.ErrSubjectDeleted
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	a := s.Auth
	if a == "" {
		a = store.AuthActive
	}
	_, err = t.tx.Exec(ctx,
		`INSERT INTO subjects (experiment_id, id, auth, sealed_personal) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (experiment_id, id) DO UPDATE SET auth=EXCLUDED.auth, sealed_personal=EXCLUDED.sealed_personal`,
		s.ExperimentID, s.ID, a, s.SealedPersonal)
	return err
}

func (t *pgTx) GetSubject(ctx context.Context, exp, sub uuid.UUID) (store.StoredSubject, error) {
	var s store.StoredSubject
	var auth string
	err := t.tx.QueryRow(ctx,
		`SELECT experiment_id, id, auth, sealed_personal FROM subjects WHERE experiment_id=$1 AND id=$2`,
		exp, sub).Scan(&s.ExperimentID, &s.ID, &auth, &s.SealedPersonal)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.StoredSubject{}, store.ErrNotFound
	}
	if err != nil {
		return store.StoredSubject{}, err
	}
	s.Auth = store.AuthState(auth)
	return s, nil
}

func (t *pgTx) SetAuth(ctx context.Context, exp, sub uuid.UUID, state store.AuthState) error {
	ct, err := t.tx.Exec(ctx,
		`UPDATE subjects SET auth=$3 WHERE experiment_id=$1 AND id=$2`, exp, sub, string(state))
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *pgTx) InsertEventIfAbsent(ctx context.Context, e domain.Event) (bool, error) {
	// Enforce subject lifecycle before ingesting.
	var auth string
	err := t.tx.QueryRow(ctx,
		`SELECT auth FROM subjects WHERE experiment_id=$1 AND id=$2`, e.ExperimentID, e.SubjectID).Scan(&auth)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, store.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	switch store.AuthState(auth) {
	case store.AuthRevoked:
		return false, store.ErrAuthRevoked
	case store.AuthDeleted:
		return false, store.ErrSubjectDeleted
	}

	dur := e.DurationMS
	if dur == 0 && e.Duration != 0 {
		dur = e.Duration.Milliseconds()
	}
	ct, err := t.tx.Exec(ctx,
		`INSERT INTO focus_events
		   (experiment_id, subject_id, device_id, client_seq, event_type, occurred_at, duration_ms)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT ON CONSTRAINT focus_events_idem DO NOTHING`,
		e.ExperimentID, e.SubjectID, e.DeviceID, e.ClientSeq, string(e.Type), e.OccurredAt.UTC(), dur)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() == 1, nil
}

func (t *pgTx) ListEvents(ctx context.Context, exp, sub uuid.UUID) ([]domain.Event, error) {
	rows, err := t.tx.Query(ctx,
		`SELECT experiment_id, subject_id, device_id, client_seq, event_type, occurred_at, duration_ms
		   FROM focus_events WHERE experiment_id=$1 AND subject_id=$2
		 ORDER BY occurred_at, device_id, client_seq, event_type`, exp, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var typ string
		var durMS int64
		if err := rows.Scan(&e.ExperimentID, &e.SubjectID, &e.DeviceID, &e.ClientSeq, &typ, &e.OccurredAt, &durMS); err != nil {
			return nil, err
		}
		e.Type = domain.EventType(typ)
		e.DurationMS = durMS
		out = append(out, e)
	}
	return out, rows.Err()
}

func (t *pgTx) SaveResult(ctx context.Context, r store.StoredResult) error {
	blob, err := json.Marshal(r.Result)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx,
		`INSERT INTO results (experiment_id, subject_id, version, digest, result_json, computed_at)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (experiment_id, subject_id, version)
		 DO UPDATE SET digest=EXCLUDED.digest, result_json=EXCLUDED.result_json, computed_at=EXCLUDED.computed_at`,
		r.ExperimentID, r.SubjectID, r.Version, r.Digest, blob, r.ComputedAt)
	return err
}

func (t *pgTx) GetResult(ctx context.Context, exp, sub uuid.UUID, version int64) (store.StoredResult, error) {
	return t.scanResult(ctx,
		`SELECT experiment_id, subject_id, version, digest, result_json, computed_at
		   FROM results WHERE experiment_id=$1 AND subject_id=$2 AND version=$3`,
		exp, sub, version)
}

func (t *pgTx) LatestResult(ctx context.Context, exp, sub uuid.UUID) (store.StoredResult, error) {
	return t.scanResult(ctx,
		`SELECT experiment_id, subject_id, version, digest, result_json, computed_at
		   FROM results WHERE experiment_id=$1 AND subject_id=$2
		 ORDER BY version DESC LIMIT 1`,
		exp, sub)
}

func (t *pgTx) scanResult(ctx context.Context, q string, args ...any) (store.StoredResult, error) {
	var r store.StoredResult
	var blob []byte
	err := t.tx.QueryRow(ctx, q, args...).
		Scan(&r.ExperimentID, &r.SubjectID, &r.Version, &r.Digest, &blob, &r.ComputedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.StoredResult{}, store.ErrNotFound
	}
	if err != nil {
		return store.StoredResult{}, err
	}
	if err := json.Unmarshal(blob, &r.Result); err != nil {
		return store.StoredResult{}, err
	}
	return r, nil
}

func (t *pgTx) PurgeSubjectData(ctx context.Context, exp, sub uuid.UUID) error {
	// Derived rows first (explicit, though FK cascade would also handle them),
	// then scrub the sealed blob and mark the subject deleted.
	if _, err := t.tx.Exec(ctx, `DELETE FROM focus_events WHERE experiment_id=$1 AND subject_id=$2`, exp, sub); err != nil {
		return err
	}
	if _, err := t.tx.Exec(ctx, `DELETE FROM results WHERE experiment_id=$1 AND subject_id=$2`, exp, sub); err != nil {
		return err
	}
	ct, err := t.tx.Exec(ctx,
		`UPDATE subjects SET sealed_personal=NULL, auth='deleted' WHERE experiment_id=$1 AND id=$2`, exp, sub)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ensure interface satisfaction at compile time.
var _ store.Store = (*PG)(nil)
