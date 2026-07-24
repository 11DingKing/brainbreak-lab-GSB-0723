package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"focuslab/internal/domain"
	"focuslab/internal/store"
)

// WriteEventsInput is a batch of events to ingest for one subject. Events are
// written idempotently and the subject's result is recomputed within the same
// transaction, so a late or out-of-order event automatically corrects the
// stored result (the fold is over the full canonical set every time).
type WriteEventsInput struct {
	ExperimentID uuid.UUID
	SubjectID    uuid.UUID
	Events       []domain.Event
}

// WriteEventsOutput reports how the batch affected state.
type WriteEventsOutput struct {
	Accepted        int    // events newly inserted (counted once)
	Duplicates      int    // events already present (idempotent no-ops)
	ResultVersion   int64  // version whose result was recomputed
	ResultDigest    string // digest of the canonical event set after write
	ResultCorrected bool   // true if the recompute changed the stored digest
}

// WriteEvents ingests a batch and recomputes the result atomically.
func (s *Service) WriteEvents(ctx context.Context, in WriteEventsInput) (WriteEventsOutput, error) {
	if in.ExperimentID == uuid.Nil || in.SubjectID == uuid.Nil {
		return WriteEventsOutput{}, wrap(ErrValidation, "experiment_id and subject_id required")
	}
	// Build a normalized copy rather than mutating the caller's slice: the same
	// batch may be shared across concurrent goroutines (cross-device retries),
	// so in-place mutation would be a data race and could corrupt a peer's call.
	normalized := make([]domain.Event, len(in.Events))
	for i, e := range in.Events {
		e.ExperimentID = in.ExperimentID
		e.SubjectID = in.SubjectID
		if !e.Type.Valid() {
			return WriteEventsOutput{}, wrap(ErrValidation, "invalid event type")
		}
		if e.DeviceID == "" {
			return WriteEventsOutput{}, wrap(ErrValidation, "device_id required")
		}
		if e.OccurredAt.IsZero() {
			return WriteEventsOutput{}, wrap(ErrValidation, "occurred_at required")
		}
		normalized[i] = e
	}

	var out WriteEventsOutput
	err := s.store.WithTx(ctx, func(tx store.Tx) error {
		// Serialise concurrent writers for this subject so that ListEvents below
		// observes every committed event and results converge (no lost updates).
		if err := tx.LockSubjectForUpdate(ctx, in.ExperimentID, in.SubjectID); err != nil {
			return mapStoreErr(err)
		}
		ss, err := tx.GetSubject(ctx, in.ExperimentID, in.SubjectID)
		if err != nil {
			return mapStoreErr(err)
		}
		switch ss.Auth {
		case store.AuthRevoked:
			return ErrRevoked
		case store.AuthDeleted:
			return ErrDeleted
		}
		subject, err := s.unsealSubjectTx(tx, ctx, ss)
		if err != nil {
			return err
		}

		// Determine the version we recompute against BEFORE the write so a
		// corrected result overwrites the same version deterministically.
		exp, err := tx.GetExperiment(ctx, in.ExperimentID)
		if err != nil {
			return mapStoreErr(err)
		}

		// Prior digest (if any) lets us report whether the result changed.
		var priorDigest string
		if prev, e := tx.GetResult(ctx, in.ExperimentID, in.SubjectID, exp.Version); e == nil {
			priorDigest = prev.Digest
		}

		for _, ev := range normalized {
			inserted, e := tx.InsertEventIfAbsent(ctx, ev)
			if e != nil {
				return mapStoreErr(e)
			}
			if inserted {
				out.Accepted++
			} else {
				out.Duplicates++
			}
		}

		all, err := tx.ListEvents(ctx, in.ExperimentID, in.SubjectID)
		if err != nil {
			return err
		}
		result := domain.Fold(subject, all, domain.FoldConfig{AsOf: s.now()})
		if err := tx.SaveResult(ctx, store.StoredResult{
			ExperimentID: in.ExperimentID,
			SubjectID:    in.SubjectID,
			Version:      exp.Version,
			Digest:       result.Digest,
			Result:       result,
			ComputedAt:   s.now().UTC(),
		}); err != nil {
			return err
		}
		out.ResultVersion = exp.Version
		out.ResultDigest = result.Digest
		out.ResultCorrected = priorDigest != "" && priorDigest != result.Digest
		return nil
	})
	if err != nil {
		return WriteEventsOutput{}, err
	}
	return out, nil
}

// GetResult returns the stored result for a version (0 = latest).
func (s *Service) GetResult(ctx context.Context, experimentID, subjectID uuid.UUID, version int64) (store.StoredResult, error) {
	var res store.StoredResult
	err := s.store.WithTx(ctx, func(tx store.Tx) error {
		var e error
		if version == 0 {
			res, e = tx.LatestResult(ctx, experimentID, subjectID)
		} else {
			res, e = tx.GetResult(ctx, experimentID, subjectID, version)
		}
		return mapStoreErr(e)
	})
	return res, err
}

// RecomputeInput requests recomputation, optionally bumping to a new version.
type RecomputeInput struct {
	ExperimentID uuid.UUID
	SubjectID    uuid.UUID
	// NewVersion, when true, computes under a bumped experiment version so the
	// prior version's result is retained for comparison/audit.
	NewVersion bool
}

// Recompute re-folds the subject's full event set. It is idempotent: recomputing
// the same events at the same version yields the same digest. This backs the
// "按版本重算" (recompute by version) requirement.
func (s *Service) Recompute(ctx context.Context, in RecomputeInput) (WriteEventsOutput, error) {
	var out WriteEventsOutput
	err := s.store.WithTx(ctx, func(tx store.Tx) error {
		// Lock the subject so a concurrent WriteEvents cannot interleave between
		// our ListEvents and SaveResult and clobber the recomputed result.
		if err := tx.LockSubjectForUpdate(ctx, in.ExperimentID, in.SubjectID); err != nil {
			return mapStoreErr(err)
		}
		ss, err := tx.GetSubject(ctx, in.ExperimentID, in.SubjectID)
		if err != nil {
			return mapStoreErr(err)
		}
		if ss.Auth == store.AuthDeleted {
			return ErrDeleted
		}
		if ss.Auth == store.AuthRevoked {
			return ErrRevoked
		}
		subject, err := s.unsealSubjectTx(tx, ctx, ss)
		if err != nil {
			return err
		}
		exp, err := tx.GetExperiment(ctx, in.ExperimentID)
		if err != nil {
			return mapStoreErr(err)
		}
		version := exp.Version
		if in.NewVersion {
			version = exp.Version + 1
			if err := tx.SetExperimentVersion(ctx, in.ExperimentID, version); err != nil {
				return err
			}
		}
		var priorDigest string
		if prev, e := tx.GetResult(ctx, in.ExperimentID, in.SubjectID, version); e == nil {
			priorDigest = prev.Digest
		}
		all, err := tx.ListEvents(ctx, in.ExperimentID, in.SubjectID)
		if err != nil {
			return err
		}
		result := domain.Fold(subject, all, domain.FoldConfig{AsOf: s.now()})
		if err := tx.SaveResult(ctx, store.StoredResult{
			ExperimentID: in.ExperimentID,
			SubjectID:    in.SubjectID,
			Version:      version,
			Digest:       result.Digest,
			Result:       result,
			ComputedAt:   s.now().UTC(),
		}); err != nil {
			return err
		}
		out.ResultVersion = version
		out.ResultDigest = result.Digest
		out.ResultCorrected = priorDigest != "" && priorDigest != result.Digest
		return nil
	})
	return out, err
}

// RevokeAuthorization withdraws consent: no further ingestion or recompute is
// permitted, but derived results are retained until hard delete.
func (s *Service) RevokeAuthorization(ctx context.Context, experimentID, subjectID uuid.UUID) error {
	return s.store.WithTx(ctx, func(tx store.Tx) error {
		ss, err := tx.GetSubject(ctx, experimentID, subjectID)
		if err != nil {
			return mapStoreErr(err)
		}
		if ss.Auth == store.AuthDeleted {
			return ErrDeleted
		}
		return tx.SetAuth(ctx, experimentID, subjectID, store.AuthRevoked)
	})
}

// DeleteSubject performs an irreversible "彻底删除": it purges the subject's
// events and results from the derived tables and destroys the crypto-shred key.
// After this, the sealed personal blob (wherever copied) cannot be decrypted
// and no personal data can be recovered from any derived table.
func (s *Service) DeleteSubject(ctx context.Context, experimentID, subjectID uuid.UUID) error {
	err := s.store.WithTx(ctx, func(tx store.Tx) error {
		if _, err := tx.GetSubject(ctx, experimentID, subjectID); err != nil {
			return mapStoreErr(err)
		}
		return tx.PurgeSubjectData(ctx, experimentID, subjectID)
	})
	if err != nil {
		return err
	}
	// Destroy the key last: even if this runs after the tx, the personal data
	// is already purged; destroying the key guarantees any surviving ciphertext
	// (backups, replicas) is permanently undecryptable.
	return s.cipher.Shred(subjectID)
}

func mapStoreErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrAuthRevoked):
		return ErrRevoked
	case errors.Is(err, store.ErrSubjectDeleted):
		return ErrDeleted
	default:
		return err
	}
}
