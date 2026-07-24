package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"brainbreak-lab/internal/models"
	"brainbreak-lab/internal/service"
	"brainbreak-lab/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Handler struct {
	users       *store.UserStore
	experiments *store.ExperimentStore
	events      *store.EventStore
	results     *store.ResultStore
	auth        *store.AuthStore
	processor   *service.EventProcessor
	deletion    *service.DeletionService
}

func NewHandler(db *sql.DB) *Handler {
	us := store.NewUserStore(db)
	es := store.NewExperimentStore(db)
	evs := store.NewEventStore(db)
	rs := store.NewResultStore(db)
	as := store.NewAuthStore(db)
	ds := store.NewDeletionStore(db)
	return &Handler{
		users:       us,
		experiments: es,
		events:      evs,
		results:     rs,
		auth:        as,
		processor:   service.NewEventProcessor(db, evs, rs, es, us),
		deletion:    service.NewDeletionService(db, evs, rs, as, ds),
	}
}

func parseUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	s := c.Param(name)
	id, err := uuid.Parse(s)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) CreateUser(c *gin.Context) {
	var req models.CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	birthDate, err := time.Parse("2006-01-02", req.BirthDate)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid birth_date format, expected YYYY-MM-DD"})
		return
	}
	tz := req.Timezone
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid timezone"})
		return
	}
	var bedtime string
	if req.Bedtime != "" {
		if _, err := time.Parse("15:04", req.Bedtime); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bedtime format, expected HH:MM"})
			return
		}
		bedtime = req.Bedtime
	}
	user, err := h.users.Create(c.Request.Context(), birthDate, tz, bedtime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, user)
}

func (h *Handler) CreateExperiment(c *gin.Context) {
	var req models.CreateExperimentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Config == nil {
		req.Config = json.RawMessage(`{}`)
	}
	exp, err := h.experiments.Create(c.Request.Context(), req.Name, req.Config)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, exp)
}

func (h *Handler) GrantAuthorization(c *gin.Context) {
	userID, ok := parseUUID(c, "userId")
	if !ok {
		return
	}
	expID, ok := parseUUID(c, "expId")
	if !ok {
		return
	}
	ue, _ := h.users.Exists(c.Request.Context(), userID)
	ee, _ := h.experiments.Exists(c.Request.Context(), expID)
	if !ue {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if !ee {
		c.JSON(http.StatusNotFound, gin.H{"error": "experiment not found"})
		return
	}
	if err := h.auth.Grant(c.Request.Context(), userID, expID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "granted"})
}

func (h *Handler) RevokeAuthorization(c *gin.Context) {
	userID, ok := parseUUID(c, "userId")
	if !ok {
		return
	}
	expID, ok := parseUUID(c, "expId")
	if !ok {
		return
	}
	if err := h.deletion.RevokeAuthorization(c.Request.Context(), userID, expID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "revoked"})
}

func (h *Handler) IngestEvents(c *gin.Context) {
	userID, ok := parseUUID(c, "userId")
	if !ok {
		return
	}
	expID, ok := parseUUID(c, "expId")
	if !ok {
		return
	}

	authorized, err := h.auth.IsAuthorized(c.Request.Context(), userID, expID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	if !authorized {
		c.JSON(http.StatusForbidden, gin.H{"error": "not authorized"})
		return
	}

	var req models.BatchEventsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Events) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no events"})
		return
	}

	br, err := h.processor.IngestBatch(c.Request.Context(), userID, expID, req.Events)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, models.BatchEventsResponse{
		Accepted:  br.Accepted,
		Duplicate: br.Duplicate,
		Version:   br.Version,
	})
}

func (h *Handler) GetResult(c *gin.Context) {
	userID, ok := parseUUID(c, "userId")
	if !ok {
		return
	}
	expID, ok := parseUUID(c, "expId")
	if !ok {
		return
	}
	versionStr := c.Query("version")
	var version int
	if versionStr != "" {
		v, err := strconv.Atoi(versionStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
			return
		}
		version = v
	}

	result, err := h.results.GetResult(c.Request.Context(), userID, expID, version)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "result not found"})
		return
	}

	includeDaily := c.Query("daily") == "true" || c.Query("daily") == "1"
	resp := models.ResultResponse{Result: *result}
	if includeDaily {
		daily, err := h.results.GetDailyAggregates(c.Request.Context(), userID, expID, result.Version)
		if err == nil {
			resp.Daily = daily
		}
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) Recalculate(c *gin.Context) {
	userID, ok := parseUUID(c, "userId")
	if !ok {
		return
	}
	expID, ok := parseUUID(c, "expId")
	if !ok {
		return
	}
	versionStr := c.Param("version")
	version, err := strconv.Atoi(versionStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version"})
		return
	}
	if err := h.processor.RecalculateVersion(c.Request.Context(), userID, expID, version); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "recalculated", "version": version})
}

func (h *Handler) Replay(c *gin.Context) {
	userID, ok := parseUUID(c, "userId")
	if !ok {
		return
	}
	expID, ok := parseUUID(c, "expId")
	if !ok {
		return
	}
	newVersion, err := h.processor.ReplayAll(c.Request.Context(), userID, expID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "replayed", "version": newVersion})
}

func (h *Handler) HardDeleteUser(c *gin.Context) {
	userID, ok := parseUUID(c, "userId")
	if !ok {
		return
	}
	if err := h.deletion.HardDeleteUser(c.Request.Context(), userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "scope": "user"})
}

func (h *Handler) HardDeleteExperiment(c *gin.Context) {
	expID, ok := parseUUID(c, "expId")
	if !ok {
		return
	}
	if err := h.deletion.HardDeleteExperiment(c.Request.Context(), expID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "scope": "experiment"})
}

func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func RegisterRoutes(r *gin.Engine, h *Handler) {
	r.GET("/health", h.Health)

	api := r.Group("/api/v1")
	{
		api.POST("/users", h.CreateUser)
		api.DELETE("/users/:userId", h.HardDeleteUser)

		api.POST("/experiments", h.CreateExperiment)
		api.DELETE("/experiments/:expId", h.HardDeleteExperiment)

		api.POST("/users/:userId/experiments/:expId/authorize", h.GrantAuthorization)
		api.POST("/users/:userId/experiments/:expId/revoke", h.RevokeAuthorization)

		api.POST("/users/:userId/experiments/:expId/events", h.IngestEvents)
		api.GET("/users/:userId/experiments/:expId/result", h.GetResult)
		api.POST("/users/:userId/experiments/:expId/recalculate/:version", h.Recalculate)
		api.POST("/users/:userId/experiments/:expId/replay", h.Replay)
	}
}
