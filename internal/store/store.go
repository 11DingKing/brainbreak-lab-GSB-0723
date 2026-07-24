// Package store defines the persistence port used by the service and provides
// an in-memory transactional implementation used for unit, property, concurrency
// and fault-injection tests. The Postgres implementation in pgstore satisfies
// the same interface.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"focuslab/internal/cryptoshred"
	"focuslab/internal/domain"
)

// Sentinel errors returned across all implementations.
var (
	ErrNotFound       = errors.New("store: not found")
	ErrAuthRevoked    = errors.New("store: subject authorization revoked")
	ErrSubjectDeleted = errors.New("store: subject deleted")
	// ErrInjected is returned by fault-injection wrappers to force rollback.
	ErrInjected = errors.New("store: injected fault")
)

// AuthState is the authorization lifecycle of a subject within an experiment.
type AuthState string

const (
	// AuthActive means the subject consents and events may be ingested/computed.
	AuthActive AuthState = "active"
	// AuthRevoked means consent was withdrawn: no new events, no recompute, but
	// existing derived results may be retained until hard delete.
	AuthRevoked AuthState = "revoked"
	// AuthDeleted means the subject's personal data has been crypto-shredded.
	AuthDeleted AuthState = "deleted"
)

// Experiment is the top-level container for a study.
type Experiment struct {
	ID        uuid.UUID
	Name      string
	Version   int64
	CreatedAt time.Time
}

// StoredSubject holds the non-personal projection the domain needs plus the
// sealed personal blob and lifecycle state. Birth/Timezone are considered
// personal and are only ever persisted inside SealedPersonal; the plaintext
// copies here are populated transiently after decryption for folding.
type StoredSubject struct {
	ID             uuid.UUID
	ExperimentID   uuid.UUID
	Auth           AuthState
	SealedPersonal []byte // AES-GCM sealed JSON of PersonalData
	// Transient decrypted view (never persisted in the clear):
	Birth    time.Time
	Timezone string
}

// PersonalData is the plaintext personal payload that gets crypto-shredded.
type PersonalData struct {
	DisplayName string    `json:"display_name"`
	Birth       time.Time `json:"birth"`
	Timezone    string    `json:"timezone"`
}

// StoredResult is a versioned, replayable experiment result for a subject.
type StoredResult struct {
	ExperimentID uuid.UUID
	SubjectID    uuid.UUID
	Version      int64
	Digest       string
	Result       domain.Result
	ComputedAt   time.Time
}

// Tx is a transactional handle. All mutations happen inside a Tx so the store
// can guarantee atomicity: either the whole batch (events + recomputed result)
// commits, or nothing does. Fault injection forces the Tx to roll back to prove
// no partial state leaks.
type Tx interface {
	// UpsertExperiment inserts or no-ops on an existing experiment.
	UpsertExperiment(ctx context.Context, e Experiment) error
	GetExperiment(ctx context.Context, id uuid.UUID) (Experiment, error)
	// SetExperimentVersion updates the experiment's current version.
	SetExperimentVersion(ctx context.Context, id uuid.UUID, version int64) error

	// UpsertSubject stores/updates a subject's sealed personal blob and auth.
	UpsertSubject(ctx context.Context, s StoredSubject) error
	GetSubject(ctx context.Context, experimentID, subjectID uuid.UUID) (StoredSubject, error)
	SetAuth(ctx context.Context, experimentID, subjectID uuid.UUID, state AuthState) error

	// LockSubjectForUpdate acquires a row-level write lock on the subject for the
	// duration of the transaction, serialising concurrent event-write/recompute
	// transactions for the same subject. Without this, two transactions writing
	// DIFFERENT events (distinct idempotency keys, so no row conflict) could each
	// read the event set without the other's still-uncommitted row and then both
	// overwrite the derived result — a lost update that drops events from the
	// replayable result. Callers MUST take this lock before ListEvents+SaveResult
	// so that recomputation observes every committed event and results converge.
	// Returns ErrNotFound if the subject does not exist.
	LockSubjectForUpdate(ctx context.Context, experimentID, subjectID uuid.UUID) error

	// GetDataKey returns the subject's crypto-shred data key using the
	// transaction's OWN connection. Decrypting personal data from inside a
	// transaction MUST go through this rather than the pool-level KeyStore:
	// while a transaction holds a row lock it already occupies a pooled
	// connection, so acquiring a second connection for the key would deadlock
	// the pool once concurrency reaches the pool size. Returns the same
	// cryptoshred sentinel errors (ErrKeyMissing / ErrKeyDestroyed).
	GetDataKey(ctx context.Context, subjectID uuid.UUID) ([]byte, error)

	// InsertEventIfAbsent stores an event idempotently. It returns inserted=false
	// when an event with the same idempotency key already exists, guaranteeing an
	// event is counted at most once regardless of retries or concurrent uploads.
	InsertEventIfAbsent(ctx context.Context, e domain.Event) (inserted bool, err error)

	// ListEvents returns all stored events for a subject in canonical order.
	ListEvents(ctx context.Context, experimentID, subjectID uuid.UUID) ([]domain.Event, error)

	// SaveResult writes the computed result for a version, replacing any prior
	// result at that version (recompute is idempotent per version).
	SaveResult(ctx context.Context, r StoredResult) error
	GetResult(ctx context.Context, experimentID, subjectID uuid.UUID, version int64) (StoredResult, error)
	LatestResult(ctx context.Context, experimentID, subjectID uuid.UUID) (StoredResult, error)

	// PurgeSubjectData removes all rows in derived tables (events, results) for
	// the subject. Combined with key destruction this makes personal data
	// unrecoverable. It does not itself touch the key store.
	PurgeSubjectData(ctx context.Context, experimentID, subjectID uuid.UUID) error
}

// Store is the persistence port. WithTx runs fn inside a transaction, committing
// on nil error and rolling back on any error (including panics).
type Store interface {
	WithTx(ctx context.Context, fn func(tx Tx) error) error
	// KeyStore exposes the crypto-shred key store, transactionally consistent
	// with the rest of the data in the Postgres implementation.
	cryptoshred.KeyStore
}
