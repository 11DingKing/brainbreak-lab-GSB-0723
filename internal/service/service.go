package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"brainbreak-lab/focus/internal/clock"
	"brainbreak-lab/focus/internal/model"
	"brainbreak-lab/focus/internal/store"
)

// Service 是业务门面：校验输入、在串行化冲突时重试。
type Service struct {
	DB *store.DB
}

func New(db *store.DB) *Service { return &Service{DB: db} }

type CreateSubjectRequest struct {
	DateOfBirth string `json:"date_of_birth"` // YYYY-MM-DD
	Timezone    string `json:"timezone"`
	Bedtime     string `json:"bedtime,omitempty"` // HH:MM
}

type CreateExperimentRequest struct {
	SubjectID string `json:"subject_id"`
	Label     string `json:"label,omitempty"`
}

type IngestEventsRequest struct {
	IdempotencyKey string                  `json:"idempotency_key,omitempty"`
	Events         []store.IngestEventInput `json:"events"`
}

func (s *Service) CreateSubject(ctx context.Context, req CreateSubjectRequest) (*model.Subject, error) {
	dob, err := time.Parse("2006-01-02", req.DateOfBirth)
	if err != nil {
		return nil, errValidation("invalid date_of_birth")
	}
	loc, err := clock.LoadLocation(req.Timezone)
	if err != nil {
		return nil, errValidation(err.Error())
	}
	in := store.SubjectInput{DateOfBirth: dob, Timezone: loc.String()}
	if req.Bedtime != "" {
		bt, err := clock.ParseBedtime(req.Bedtime)
		if err != nil {
			return nil, errValidation(err.Error())
		}
		in.Bedtime = &bt
	}
	sub, err := s.DB.CreateSubject(ctx, in)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return sub, nil
}

func (s *Service) GetSubject(ctx context.Context, id string) (*model.Subject, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, errValidation("invalid id")
	}
	sub, err := s.DB.GetSubject(ctx, uid)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return sub, nil
}

func (s *Service) WithdrawConsent(ctx context.Context, id string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return errValidation("invalid id")
	}
	return mapStoreError(s.DB.WithdrawConsent(ctx, uid))
}

func (s *Service) DeleteSubject(ctx context.Context, id string) (uuid.UUID, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, errValidation("invalid id")
	}
	token, err := s.DB.DeleteSubjectPermanently(ctx, uid)
	if err != nil {
		return uuid.Nil, mapStoreError(err)
	}
	return token, nil
}

func (s *Service) CreateExperiment(ctx context.Context, req CreateExperimentRequest) (*model.Experiment, error) {
	uid, err := uuid.Parse(req.SubjectID)
	if err != nil {
		return nil, errValidation("invalid subject_id")
	}
	exp, err := s.DB.CreateExperiment(ctx, store.ExperimentInput{SubjectID: uid, Label: req.Label})
	if err != nil {
		return nil, mapStoreError(err)
	}
	return exp, nil
}

func (s *Service) GetExperiment(ctx context.Context, id string) (*model.Experiment, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, errValidation("invalid id")
	}
	exp, err := s.DB.GetExperiment(ctx, uid)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return exp, nil
}

func (s *Service) IngestEvents(ctx context.Context, experimentID string, req IngestEventsRequest) (*store.IngestResult, error) {
	uid, err := uuid.Parse(experimentID)
	if err != nil {
		return nil, errValidation("invalid experiment_id")
	}
	if len(req.Events) == 0 {
		return nil, errValidation("events required")
	}
	// 规范化 payload 与 occurred_at（UTC）。
	evs := make([]store.IngestEventInput, len(req.Events))
	for i, e := range req.Events {
		if !e.EventType.Valid() {
			return nil, errValidation("invalid event_type")
		}
		if e.ClientSeq < 0 {
			return nil, errValidation("invalid client_seq")
		}
		if e.OccurredAt.IsZero() {
			return nil, errValidation("occurred_at required")
		}
		e.OccurredAt = e.OccurredAt.UTC()
		if len(e.Payload) == 0 {
			e.Payload = []byte(`{}`)
		}
		evs[i] = e
	}
	key := req.IdempotencyKey
	if key == "" {
		key = uuid.NewString()
	}
	var result *store.IngestResult
	err = s.DB.RetrySerialize(ctx, 10, func(ctx context.Context) error {
		r, err := s.DB.IngestEvents(ctx, uid, key, evs)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	return result, nil
}

func (s *Service) Recalc(ctx context.Context, experimentID string) (*model.ResultSummary, error) {
	uid, err := uuid.Parse(experimentID)
	if err != nil {
		return nil, errValidation("invalid experiment_id")
	}
	var summary *model.ResultSummary
	err = s.DB.RetrySerialize(ctx, 10, func(ctx context.Context) error {
		r, err := s.DB.Recalc(ctx, uid)
		if err != nil {
			return err
		}
		summary = r
		return nil
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	return summary, nil
}

func (s *Service) GetResult(ctx context.Context, experimentID string, version int64) (*model.ResultSummary, error) {
	uid, err := uuid.Parse(experimentID)
	if err != nil {
		return nil, errValidation("invalid experiment_id")
	}
	r, err := s.DB.GetResult(ctx, uid, version)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return r, nil
}

// 错误类型：对外使用统一 ServiceError，HTTP 层映射为非诊断性消息。
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Message }

func errValidation(msg string) *Error {
	return &Error{Code: "VALIDATION_ERROR", Message: msg, Status: 400}
}

func errNotFound() *Error      { return &Error{Code: "NOT_FOUND", Message: "resource not found", Status: 404} }
func errConflict() *Error      { return &Error{Code: "CONFLICT", Message: "conflict", Status: 409} }
func errConsent() *Error       { return &Error{Code: "CONSENT_WITHDRAWN", Message: "consent withdrawn", Status: 403} }
func errDeleted() *Error       { return &Error{Code: "DELETED", Message: "resource deleted", Status: 410} }
func errInternal() *Error      { return &Error{Code: "INTERNAL", Message: "internal error", Status: 500} }
func errUnavailable() *Error   { return &Error{Code: "UNAVAILABLE", Message: "service unavailable", Status: 503} }

func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		return se
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errNotFound()
	case errors.Is(err, store.ErrConflict):
		return errConflict()
	case errors.Is(err, store.ErrValidation):
		return errValidation("invalid request")
	case errors.Is(err, store.ErrConsent):
		return errConsent()
	case errors.Is(err, store.ErrDeleted):
		return errDeleted()
	case errors.Is(err, store.ErrSerialization):
		return errUnavailable()
	default:
		return errInternal()
	}
}
