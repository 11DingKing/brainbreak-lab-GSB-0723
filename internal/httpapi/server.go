// Package httpapi is the HTTP transport adapter over the service layer. It uses
// only the standard library (net/http with Go 1.22+ method+path routing). All
// error responses are non-diagnostic: they carry a stable machine code and a
// fixed human message, never internal error text, stack traces, SQL, or any
// personal data.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"focuslab/internal/domain"
	"focuslab/internal/service"
)

// Server adapts a service.Service to HTTP.
type Server struct {
	svc *service.Service
}

// NewServer builds the HTTP server adapter.
func NewServer(svc *service.Service) *Server {
	return &Server{svc: svc}
}

// Handler returns the configured http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/experiments", s.createExperiment)
	mux.HandleFunc("POST /v1/experiments/{expID}/subjects/{subID}/events", s.writeEvents)
	mux.HandleFunc("GET /v1/experiments/{expID}/subjects/{subID}/result", s.getResult)
	mux.HandleFunc("POST /v1/experiments/{expID}/subjects/{subID}/recompute", s.recompute)
	mux.HandleFunc("POST /v1/experiments/{expID}/subjects/{subID}/revoke", s.revoke)
	mux.HandleFunc("DELETE /v1/experiments/{expID}/subjects/{subID}", s.deleteSubject)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// ---- error envelope (non-diagnostic) ----

type errorBody struct {
	Error errorDetail `json:"error"`
}
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError maps a service error to a fixed status/code/message. It never
// echoes the underlying error string to the client, preventing leakage of
// internal state or personal data.
func writeError(w http.ResponseWriter, err error) {
	status, code, msg := http.StatusInternalServerError, "internal", "internal error"
	switch {
	case errors.Is(err, service.ErrValidation):
		status, code, msg = http.StatusBadRequest, "invalid_request", "request failed validation"
	case errors.Is(err, service.ErrNotFound):
		status, code, msg = http.StatusNotFound, "not_found", "resource not found"
	case errors.Is(err, service.ErrRevoked):
		status, code, msg = http.StatusConflict, "authorization_revoked", "subject authorization revoked"
	case errors.Is(err, service.ErrDeleted):
		status, code, msg = http.StatusGone, "subject_deleted", "subject data has been deleted"
	case errors.Is(err, service.ErrConflict):
		status, code, msg = http.StatusConflict, "conflict", "conflicting request"
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return service.ErrValidation
	}
	return nil
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, service.ErrValidation
	}
	return id, nil
}

// ---- handlers ----

type createExperimentReq struct {
	Name        string    `json:"name"`
	SubjectID   string    `json:"subject_id,omitempty"`
	DisplayName string    `json:"display_name"`
	Birth       time.Time `json:"birth"`
	Timezone    string    `json:"timezone"`
}
type createExperimentResp struct {
	ExperimentID string `json:"experiment_id"`
	SubjectID    string `json:"subject_id"`
	Version      int64  `json:"version"`
}

func (s *Server) createExperiment(w http.ResponseWriter, r *http.Request) {
	var req createExperimentReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	in := service.CreateExperimentInput{
		Name:        req.Name,
		DisplayName: req.DisplayName,
		Birth:       req.Birth,
		Timezone:    req.Timezone,
	}
	if req.SubjectID != "" {
		sid, err := uuid.Parse(req.SubjectID)
		if err != nil {
			writeError(w, service.ErrValidation)
			return
		}
		in.SubjectID = sid
	}
	out, err := s.svc.CreateExperiment(r.Context(), in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, createExperimentResp{
		ExperimentID: out.ExperimentID.String(),
		SubjectID:    out.SubjectID.String(),
		Version:      out.Version,
	})
}

type eventReq struct {
	DeviceID   string `json:"device_id"`
	ClientSeq  int64  `json:"client_seq"`
	Type       string `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	DurationMS int64  `json:"duration_ms"`
}
type writeEventsReq struct {
	Events []eventReq `json:"events"`
}
type writeEventsResp struct {
	Accepted        int    `json:"accepted"`
	Duplicates      int    `json:"duplicates"`
	ResultVersion   int64  `json:"result_version"`
	ResultDigest    string `json:"result_digest"`
	ResultCorrected bool   `json:"result_corrected"`
}

func (s *Server) writeEvents(w http.ResponseWriter, r *http.Request) {
	expID, err := pathUUID(r, "expID")
	if err != nil {
		writeError(w, err)
		return
	}
	subID, err := pathUUID(r, "subID")
	if err != nil {
		writeError(w, err)
		return
	}
	var req writeEventsReq
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	in := service.WriteEventsInput{ExperimentID: expID, SubjectID: subID}
	for _, e := range req.Events {
		in.Events = append(in.Events, domainEvent(e))
	}
	out, err := s.svc.WriteEvents(r.Context(), in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeEventsResp{
		Accepted:        out.Accepted,
		Duplicates:      out.Duplicates,
		ResultVersion:   out.ResultVersion,
		ResultDigest:    out.ResultDigest,
		ResultCorrected: out.ResultCorrected,
	})
}

func (s *Server) getResult(w http.ResponseWriter, r *http.Request) {
	expID, err := pathUUID(r, "expID")
	if err != nil {
		writeError(w, err)
		return
	}
	subID, err := pathUUID(r, "subID")
	if err != nil {
		writeError(w, err)
		return
	}
	var version int64
	if v := r.URL.Query().Get("version"); v != "" {
		parsed, perr := parseInt64(v)
		if perr != nil {
			writeError(w, service.ErrValidation)
			return
		}
		version = parsed
	}
	res, err := s.svc.GetResult(r.Context(), expID, subID, version)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     res.Version,
		"digest":      res.Digest,
		"computed_at": res.ComputedAt,
		"result":      res.Result,
	})
}

type recomputeReq struct {
	NewVersion bool `json:"new_version"`
}

func (s *Server) recompute(w http.ResponseWriter, r *http.Request) {
	expID, err := pathUUID(r, "expID")
	if err != nil {
		writeError(w, err)
		return
	}
	subID, err := pathUUID(r, "subID")
	if err != nil {
		writeError(w, err)
		return
	}
	var req recomputeReq
	// Body optional; ignore decode errors on empty body.
	_ = json.NewDecoder(r.Body).Decode(&req)
	out, err := s.svc.Recompute(r.Context(), service.RecomputeInput{
		ExperimentID: expID, SubjectID: subID, NewVersion: req.NewVersion,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeEventsResp{
		ResultVersion:   out.ResultVersion,
		ResultDigest:    out.ResultDigest,
		ResultCorrected: out.ResultCorrected,
	})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	expID, err := pathUUID(r, "expID")
	if err != nil {
		writeError(w, err)
		return
	}
	subID, err := pathUUID(r, "subID")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.svc.RevokeAuthorization(r.Context(), expID, subID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) deleteSubject(w http.ResponseWriter, r *http.Request) {
	expID, err := pathUUID(r, "expID")
	if err != nil {
		writeError(w, err)
		return
	}
	subID, err := pathUUID(r, "subID")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.svc.DeleteSubject(r.Context(), expID, subID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func domainEvent(e eventReq) domain.Event {
	return domain.Event{
		DeviceID:   e.DeviceID,
		ClientSeq:  e.ClientSeq,
		Type:       domain.EventType(e.Type),
		OccurredAt: e.OccurredAt,
		DurationMS: e.DurationMS,
	}
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
