// Package service orchestrates the focus experiment use cases on top of the
// store port, the pure domain fold and crypto-shredding. It is transport
// agnostic: the HTTP layer is a thin adapter over these methods.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"focuslab/internal/cryptoshred"
	"focuslab/internal/domain"
	"focuslab/internal/store"
)

// Service holds the dependencies for the use cases.
type Service struct {
	store  store.Store
	cipher *cryptoshred.Cipher
	now    func() time.Time
}

// New builds a Service. now may be nil to use time.Now (overridable in tests
// so age/timezone/curfew logic can be exercised deterministically).
func New(s store.Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:  s,
		cipher: cryptoshred.New(s),
		now:    now,
	}
}

// Public errors, mapped to non-diagnostic HTTP responses by the transport.
var (
	ErrValidation = errors.New("validation error")
	ErrNotFound   = errors.New("not found")
	ErrRevoked    = errors.New("authorization revoked")
	ErrDeleted    = errors.New("subject deleted")
	ErrConflict   = errors.New("conflict")
)

// wrap attaches a short, non-diagnostic reason to a sentinel error. The reason
// is a fixed, caller-supplied string (never derived from personal data or raw
// input), so it is safe to surface to clients.
func wrap(sentinel error, reason string) error {
	return fmt.Errorf("%w: %s", sentinel, reason)
}

// CreateExperimentInput describes a new experiment plus its subject. Personal
// data (name, birth, timezone) is sealed immediately and never stored in clear.
type CreateExperimentInput struct {
	Name        string
	SubjectID   uuid.UUID // optional; generated if nil
	DisplayName string
	Birth       time.Time
	Timezone    string
}

// CreateExperimentOutput returns the created identifiers.
type CreateExperimentOutput struct {
	ExperimentID uuid.UUID
	SubjectID    uuid.UUID
	Version      int64
}

// CreateExperiment creates an experiment and its subject, generating and
// storing a per-subject data key and sealing the personal payload under it.
func (s *Service) CreateExperiment(ctx context.Context, in CreateExperimentInput) (CreateExperimentOutput, error) {
	if in.Name == "" {
		return CreateExperimentOutput{}, wrap(ErrValidation, "name required")
	}
	if in.Timezone != "" {
		if _, err := time.LoadLocation(in.Timezone); err != nil {
			return CreateExperimentOutput{}, wrap(ErrValidation, "invalid timezone")
		}
	}
	if in.Birth.IsZero() {
		return CreateExperimentOutput{}, wrap(ErrValidation, "birth required")
	}
	expID := uuid.New()
	subID := in.SubjectID
	if subID == uuid.Nil {
		subID = uuid.New()
	}

	// Create the data key outside the tx (key store is its own atomic unit),
	// then seal. If the tx later fails, the orphan key is harmless: it protects
	// no data and will be overwritten on retry.
	if err := s.cipher.EnsureKey(subID); err != nil {
		return CreateExperimentOutput{}, err
	}
	sealed, err := s.sealPersonal(subID, in)
	if err != nil {
		return CreateExperimentOutput{}, err
	}

	out := CreateExperimentOutput{ExperimentID: expID, SubjectID: subID, Version: 1}
	err = s.store.WithTx(ctx, func(tx store.Tx) error {
		if err := tx.UpsertExperiment(ctx, store.Experiment{
			ID: expID, Name: in.Name, Version: 1, CreatedAt: s.now().UTC(),
		}); err != nil {
			return err
		}
		return tx.UpsertSubject(ctx, store.StoredSubject{
			ID: subID, ExperimentID: expID, Auth: store.AuthActive, SealedPersonal: sealed,
		})
	})
	if err != nil {
		return CreateExperimentOutput{}, err
	}
	return out, nil
}

func (s *Service) sealPersonal(subID uuid.UUID, in CreateExperimentInput) ([]byte, error) {
	pd := store.PersonalData{DisplayName: in.DisplayName, Birth: in.Birth.UTC(), Timezone: in.Timezone}
	raw, err := json.Marshal(pd)
	if err != nil {
		return nil, err
	}
	return s.cipher.SealFor(subID, raw)
}

// unsealSubject decrypts a subject's personal blob into the transient fields the
// domain fold needs. Returns ErrDeleted if the key was shredded.
func (s *Service) unsealSubject(ss store.StoredSubject) (domain.Subject, error) {
	if ss.Auth == store.AuthDeleted || len(ss.SealedPersonal) == 0 {
		return domain.Subject{}, ErrDeleted
	}
	raw, err := s.cipher.OpenFor(ss.ID, ss.SealedPersonal)
	if err != nil {
		if errors.Is(err, cryptoshred.ErrKeyDestroyed) {
			return domain.Subject{}, ErrDeleted
		}
		return domain.Subject{}, err
	}
	var pd store.PersonalData
	if err := json.Unmarshal(raw, &pd); err != nil {
		return domain.Subject{}, err
	}
	return domain.Subject{ID: ss.ID, Birth: pd.Birth, Timezone: pd.Timezone}, nil
}
