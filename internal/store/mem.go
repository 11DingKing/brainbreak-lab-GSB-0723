package store

import (
	"context"
	"sort"
	"sync"

	"github.com/google/uuid"

	"focuslab/internal/cryptoshred"
	"focuslab/internal/domain"
)

// Mem is an in-memory, transactional Store implementation. It backs unit,
// property, concurrency and fault-injection tests. Transactions operate on a
// staged copy of the data; WithTx commits the staged copy atomically under a
// write lock on success and discards it on any error, faithfully modelling the
// all-or-nothing semantics of the Postgres implementation.
//
// The crypto-shred key store is intentionally kept on a SEPARATE lock from the
// transactional data. This mirrors the Postgres implementation, where key
// operations use their own connection rather than the request transaction, and
// it avoids a re-entrant deadlock: service code decrypts personal data (a key
// lookup) from inside a WithTx closure, so the key store must never contend for
// the same lock WithTx holds.
type Mem struct {
	mu   sync.Mutex
	data *memData

	keyMu sync.Mutex
	keys  map[uuid.UUID]keyEntry
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem {
	return &Mem{data: newMemData(), keys: map[uuid.UUID]keyEntry{}}
}

type memData struct {
	experiments map[uuid.UUID]Experiment
	subjects    map[subjectKey]StoredSubject
	events      map[eventKey]domain.Event
	results     map[resultKey]StoredResult
}

type keyEntry struct {
	key       []byte
	destroyed bool
}

type subjectKey struct{ exp, sub uuid.UUID }
type resultKey struct {
	exp, sub uuid.UUID
	version  int64
}
type eventKey struct {
	exp, sub uuid.UUID
	device   string
	seq      int64
}

func newMemData() *memData {
	return &memData{
		experiments: map[uuid.UUID]Experiment{},
		subjects:    map[subjectKey]StoredSubject{},
		events:      map[eventKey]domain.Event{},
		results:     map[resultKey]StoredResult{},
	}
}

// clone deep-copies the mutable maps so a transaction can stage changes without
// touching committed state until commit.
func (d *memData) clone() *memData {
	n := newMemData()
	for k, v := range d.experiments {
		n.experiments[k] = v
	}
	for k, v := range d.subjects {
		n.subjects[k] = v
	}
	for k, v := range d.events {
		n.events[k] = v
	}
	for k, v := range d.results {
		n.results[k] = v
	}
	return n
}

// WithTx runs fn against a staged copy, committing atomically on success.
func (m *Mem) WithTx(ctx context.Context, fn func(tx Tx) error) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	staged := m.data.clone()
	tx := &memTx{data: staged}

	defer func() {
		if r := recover(); r != nil {
			// Rollback on panic: staged copy is simply discarded.
			err = errFromPanic(r)
		}
	}()

	if e := fn(tx); e != nil {
		return e // rollback: discard staged
	}
	m.data = staged // commit
	return nil
}

// Key store methods use their own lock (keyMu), independent of the tx data
// lock, so key lookups performed from inside a WithTx closure do not deadlock.
func (m *Mem) PutKey(subjectID uuid.UUID, key []byte) error {
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	e, ok := m.keys[subjectID]
	if ok && e.destroyed {
		return cryptoshred.ErrKeyDestroyed
	}
	m.keys[subjectID] = keyEntry{key: append([]byte(nil), key...)}
	return nil
}

func (m *Mem) GetKey(subjectID uuid.UUID) ([]byte, error) {
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	e, ok := m.keys[subjectID]
	if !ok {
		return nil, cryptoshred.ErrKeyMissing
	}
	if e.destroyed {
		return nil, cryptoshred.ErrKeyDestroyed
	}
	return append([]byte(nil), e.key...), nil
}

func (m *Mem) DestroyKey(subjectID uuid.UUID) error {
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	// Tombstone: drop the key material, remember it was destroyed.
	m.keys[subjectID] = keyEntry{destroyed: true}
	return nil
}

// memTx implements Tx against a staged memData.
type memTx struct {
	data *memData
}

func (t *memTx) UpsertExperiment(ctx context.Context, e Experiment) error {
	if _, ok := t.data.experiments[e.ID]; !ok {
		t.data.experiments[e.ID] = e
	}
	return nil
}

func (t *memTx) GetExperiment(ctx context.Context, id uuid.UUID) (Experiment, error) {
	e, ok := t.data.experiments[id]
	if !ok {
		return Experiment{}, ErrNotFound
	}
	return e, nil
}

func (t *memTx) SetExperimentVersion(ctx context.Context, id uuid.UUID, version int64) error {
	e, ok := t.data.experiments[id]
	if !ok {
		return ErrNotFound
	}
	e.Version = version
	t.data.experiments[id] = e
	return nil
}

func (t *memTx) UpsertSubject(ctx context.Context, s StoredSubject) error {
	k := subjectKey{s.ExperimentID, s.ID}
	// Do not resurrect a deleted subject.
	if cur, ok := t.data.subjects[k]; ok && cur.Auth == AuthDeleted {
		return ErrSubjectDeleted
	}
	// Persist only sealed personal + non-personal projection.
	stored := StoredSubject{
		ID:             s.ID,
		ExperimentID:   s.ExperimentID,
		Auth:           s.Auth,
		SealedPersonal: append([]byte(nil), s.SealedPersonal...),
	}
	if stored.Auth == "" {
		stored.Auth = AuthActive
	}
	t.data.subjects[k] = stored
	return nil
}

func (t *memTx) GetSubject(ctx context.Context, exp, sub uuid.UUID) (StoredSubject, error) {
	s, ok := t.data.subjects[subjectKey{exp, sub}]
	if !ok {
		return StoredSubject{}, ErrNotFound
	}
	return s, nil
}

func (t *memTx) SetAuth(ctx context.Context, exp, sub uuid.UUID, state AuthState) error {
	k := subjectKey{exp, sub}
	s, ok := t.data.subjects[k]
	if !ok {
		return ErrNotFound
	}
	s.Auth = state
	t.data.subjects[k] = s
	return nil
}

func (t *memTx) InsertEventIfAbsent(ctx context.Context, e domain.Event) (bool, error) {
	// Enforce subject state: no ingestion once revoked or deleted.
	if s, ok := t.data.subjects[subjectKey{e.ExperimentID, e.SubjectID}]; ok {
		switch s.Auth {
		case AuthRevoked:
			return false, ErrAuthRevoked
		case AuthDeleted:
			return false, ErrSubjectDeleted
		}
	}
	k := eventKey{e.ExperimentID, e.SubjectID, e.DeviceID, e.ClientSeq}
	if _, exists := t.data.events[k]; exists {
		return false, nil // idempotent: already counted
	}
	t.data.events[k] = e
	return true, nil
}

func (t *memTx) ListEvents(ctx context.Context, exp, sub uuid.UUID) ([]domain.Event, error) {
	var out []domain.Event
	for k, e := range t.data.events {
		if k.exp == exp && k.sub == sub {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return domain.Less(out[i], out[j]) })
	return out, nil
}

func (t *memTx) SaveResult(ctx context.Context, r StoredResult) error {
	t.data.results[resultKey{r.ExperimentID, r.SubjectID, r.Version}] = r
	return nil
}

func (t *memTx) GetResult(ctx context.Context, exp, sub uuid.UUID, version int64) (StoredResult, error) {
	r, ok := t.data.results[resultKey{exp, sub, version}]
	if !ok {
		return StoredResult{}, ErrNotFound
	}
	return r, nil
}

func (t *memTx) LatestResult(ctx context.Context, exp, sub uuid.UUID) (StoredResult, error) {
	var best StoredResult
	found := false
	for k, r := range t.data.results {
		if k.exp == exp && k.sub == sub {
			if !found || r.Version > best.Version {
				best = r
				found = true
			}
		}
	}
	if !found {
		return StoredResult{}, ErrNotFound
	}
	return best, nil
}

func (t *memTx) PurgeSubjectData(ctx context.Context, exp, sub uuid.UUID) error {
	for k := range t.data.events {
		if k.exp == exp && k.sub == sub {
			delete(t.data.events, k)
		}
	}
	for k := range t.data.results {
		if k.exp == exp && k.sub == sub {
			delete(t.data.results, k)
		}
	}
	// Scrub sealed personal blob from the subject row and mark deleted.
	sk := subjectKey{exp, sub}
	if s, ok := t.data.subjects[sk]; ok {
		s.SealedPersonal = nil
		s.Auth = AuthDeleted
		t.data.subjects[sk] = s
	}
	return nil
}
