package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"brainbreak-lab/focus/internal/service"
)

type envelope struct {
	Error *errBody `json:"error,omitempty"`
	Data  any      `json:"data,omitempty"`
}

type errBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

type Handlers struct {
	Svc *service.Service
}

func New(svc *service.Service) *Handlers { return &Handlers{Svc: svc} }

func rid(c *gin.Context) string {
	v, _ := c.Get("request_id")
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func writeError(c *gin.Context, err error) {
	var se *service.Error
	if !errors.As(err, &se) {
		se = &service.Error{Code: "INTERNAL", Message: "internal error", Status: 500}
	}
	c.JSON(se.Status, envelope{Error: &errBody{Code: se.Code, Message: se.Message, RequestID: rid(c)}})
}

func writeData(c *gin.Context, status int, data any) {
	c.JSON(status, envelope{Data: data})
}

func (h *Handlers) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handlers) CreateSubject(c *gin.Context) {
	var req service.CreateSubjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, &service.Error{Code: "VALIDATION_ERROR", Message: "invalid JSON body", Status: 400})
		return
	}
	sub, err := h.Svc.CreateSubject(c.Request.Context(), req)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusCreated, sub)
}

func (h *Handlers) GetSubject(c *gin.Context) {
	sub, err := h.Svc.GetSubject(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, sub)
}

func (h *Handlers) WithdrawConsent(c *gin.Context) {
	if err := h.Svc.WithdrawConsent(c.Request.Context(), c.Param("id")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handlers) DeleteSubject(c *gin.Context) {
	token, err := h.Svc.DeleteSubject(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, gin.H{"deletion_token": token.String()})
}

func (h *Handlers) CreateExperiment(c *gin.Context) {
	var req service.CreateExperimentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, &service.Error{Code: "VALIDATION_ERROR", Message: "invalid JSON body", Status: 400})
		return
	}
	exp, err := h.Svc.CreateExperiment(c.Request.Context(), req)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusCreated, exp)
}

func (h *Handlers) GetExperiment(c *gin.Context) {
	exp, err := h.Svc.GetExperiment(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, exp)
}

func (h *Handlers) IngestEvents(c *gin.Context) {
	var req service.IngestEventsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, &service.Error{Code: "VALIDATION_ERROR", Message: "invalid JSON body", Status: 400})
		return
	}
	if hdr := c.GetHeader("Idempotency-Key"); hdr != "" && req.IdempotencyKey == "" {
		req.IdempotencyKey = hdr
	}
	res, err := h.Svc.IngestEvents(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, res)
}

func (h *Handlers) GetResult(c *gin.Context) {
	var version int64
	if v := c.Query("version"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(c, &service.Error{Code: "VALIDATION_ERROR", Message: "invalid version", Status: 400})
			return
		}
		version = n
	}
	r, err := h.Svc.GetResult(c.Request.Context(), c.Param("id"), version)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, r)
}

func (h *Handlers) Recalc(c *gin.Context) {
	r, err := h.Svc.Recalc(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, r)
}
