package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"brainbreak-lab/internal/handler"
	"brainbreak-lab/internal/models"
	"brainbreak-lab/internal/service"
	"brainbreak-lab/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBURL = "postgres://brainbreak:brainbreak@localhost:5432/brainbreak_test?sslmode=disable"

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	adminDB, err := store.Open("postgres://brainbreak:brainbreak@localhost:5432/postgres?sslmode=disable")
	require.NoError(t, err)
	defer adminDB.Close()

	_, err = adminDB.Exec("DROP DATABASE IF EXISTS brainbreak_test WITH (FORCE)")
	require.NoError(t, err)
	_, err = adminDB.Exec("CREATE DATABASE brainbreak_test")
	require.NoError(t, err)

	db, err := store.Open(testDBURL)
	require.NoError(t, err)

	migrationsDir := findMigrationsDir(t)
	err = store.RunMigrations(context.Background(), db, migrationsDir)
	require.NoError(t, err)

	t.Cleanup(func() {
		db.Close()
	})
	return db
}

func findMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		for _, sub := range []string{"migrations", filepath.Join("internal", "migrations")} {
			candidate := filepath.Join(dir, sub)
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find migrations directory")
		}
		dir = parent
	}
}

func setupTestServer(t *testing.T, db *sql.DB) (*gin.Engine, *handler.Handler) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := handler.NewHandler(db)
	handler.RegisterRoutes(r, h)
	return r, h
}

func createTestUser(t *testing.T, srv *gin.Engine, birthDate, tz, bedtime string) uuid.UUID {
	body := map[string]string{
		"birth_date": birthDate,
		"timezone":   tz,
	}
	if bedtime != "" {
		body["bedtime"] = bedtime
	}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/users", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	id, err := uuid.Parse(resp["id"])
	require.NoError(t, err)
	return id
}

func createTestExperiment(t *testing.T, srv *gin.Engine, name string) uuid.UUID {
	body := map[string]interface{}{"name": name}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/experiments", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	id, err := uuid.Parse(resp["id"])
	require.NoError(t, err)
	return id
}

func grantAuth(t *testing.T, srv *gin.Engine, userID, expID uuid.UUID) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/authorize", userID, expID), nil)
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func sendEvents(t *testing.T, srv *gin.Engine, userID, expID uuid.UUID, events []models.EventInput) *models.BatchEventsResponse {
	body := map[string]interface{}{"events": events}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/events", userID, expID), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp models.BatchEventsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	return &resp
}

func makeWatchingEvent(userID, expID uuid.UUID, deviceID string, seq int64, start time.Time, durationSec int64) models.EventInput {
	return models.EventInput{
		EventID:      uuid.New().String(),
		UserID:       userID.String(),
		ExperimentID: expID.String(),
		DeviceID:     deviceID,
		ClientSeq:    seq,
		EventType:    models.EventWatchingSession,
		OccurredAt:   start,
		Payload:      json.RawMessage(fmt.Sprintf(`{"duration_seconds":%d}`, durationSec)),
	}
}

func makeCardViewEvent(userID, expID uuid.UUID, deviceID string, seq int64, at time.Time) models.EventInput {
	return models.EventInput{
		EventID:      uuid.New().String(),
		UserID:       userID.String(),
		ExperimentID: expID.String(),
		DeviceID:     deviceID,
		ClientSeq:    seq,
		EventType:    models.EventCardView,
		OccurredAt:   at,
		Payload:      json.RawMessage(`{}`),
	}
}

func TestCreateUserAndExperiment(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "2000-01-01", "Asia/Shanghai", "22:00")
	expID := createTestExperiment(t, srv, "focus-test")
	grantAuth(t, srv, userID, expID)
	assert.NotEqual(t, uuid.Nil, userID)
	assert.NotEqual(t, uuid.Nil, expID)
}

func TestAgeClassification(t *testing.T) {
	tests := []struct {
		name      string
		birthDate string
		now       time.Time
		tz        string
		expected  models.AgeGroup
	}{
		{"adult", "1990-01-01", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "UTC", models.AgeGroupAdult},
		{"teen-17", "2009-06-15", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "UTC", models.AgeGroupTeen},
		{"teen-13", "2013-01-01", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "UTC", models.AgeGroupTeen},
		{"child-12", "2014-01-01", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "UTC", models.AgeGroupChild},
		{"adult-birthday-today", "2008-07-24", time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), "UTC", models.AgeGroupAdult},
		{"teen-day-before-birthday", "2008-07-24", time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC), "UTC", models.AgeGroupTeen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tz, _ := time.LoadLocation(tt.tz)
			bd, _ := time.Parse("2006-01-02", tt.birthDate)
			ag := service.AgeGroupFromBirthDate(bd, tt.now, tz)
			assert.Equal(t, tt.expected, ag)
		})
	}
}

func TestTimezoneAgeCalculation(t *testing.T) {
	tz, _ := time.LoadLocation("Asia/Shanghai")
	bd := time.Date(2008, 7, 24, 0, 0, 0, 0, time.UTC)

	utc230amOn24th := time.Date(2026, 7, 23, 16, 0, 0, 0, time.UTC)
	age := service.CalculateAge(bd, utc230amOn24th, tz)
	assert.Equal(t, 18, age, "in Shanghai timezone, 16:00 UTC on 23rd is 00:00 on 24th - birthday just started, age 18")

	utc3pmOn23rd := time.Date(2026, 7, 23, 7, 0, 0, 0, time.UTC)
	age2 := service.CalculateAge(bd, utc3pmOn23rd, tz)
	assert.Equal(t, 17, age2, "in Shanghai timezone, 07:00 UTC on 23rd is 15:00 on 23rd, day before birthday, age 17")
}

func TestIdempotentEventIngestion(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "idempotent-test")
	grantAuth(t, srv, userID, expID)

	eventID := uuid.New().String()
	occurred := time.Now().UTC().Add(-1 * time.Hour)
	ev := models.EventInput{
		EventID:      eventID,
		UserID:       userID.String(),
		ExperimentID: expID.String(),
		DeviceID:     "dev1",
		ClientSeq:    1,
		EventType:    models.EventCardView,
		OccurredAt:   occurred,
		Payload:      json.RawMessage(`{}`),
	}

	resp1 := sendEvents(t, srv, userID, expID, []models.EventInput{ev})
	assert.Equal(t, 1, resp1.Accepted)
	assert.Equal(t, 0, resp1.Duplicate)

	resp2 := sendEvents(t, srv, userID, expID, []models.EventInput{ev})
	assert.Equal(t, 0, resp2.Accepted)
	assert.Equal(t, 1, resp2.Duplicate)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	assert.Equal(t, 1, result.Result.TotalCardViews)
}

func TestOutOfOrderAndLateEvents(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "ooo-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	events := []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 3, base.Add(20*time.Minute), 600),
		makeCardViewEvent(userID, expID, "dev1", 1, base),
		makeWatchingEvent(userID, expID, "dev1", 2, base.Add(10*time.Minute), 300),
	}

	resp := sendEvents(t, srv, userID, expID, events)
	assert.Equal(t, 3, resp.Accepted)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?daily=true", userID, expID), nil)
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	assert.Equal(t, 900, int(result.Result.TotalDurationSeconds))
	assert.Equal(t, 2, result.Result.TotalWatchingSessions)
	assert.Equal(t, 1, result.Result.TotalCardViews)
	assert.Len(t, result.Daily, 1)
}

func TestLateEventTriggersRecalculation(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "late-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	resp1 := sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 600),
	})
	v1 := resp1.Version
	assert.Equal(t, 1, resp1.Accepted)

	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w1, req1)
	var r1 models.ResultResponse
	json.Unmarshal(w1.Body.Bytes(), &r1)
	assert.Equal(t, int64(600), r1.Result.TotalDurationSeconds)

	resp2 := sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 0, base.Add(-5*time.Minute), 400),
	})
	v2 := resp2.Version
	assert.True(t, v2 > v1, "late event should produce new version")

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w2, req2)
	var r2 models.ResultResponse
	json.Unmarshal(w2.Body.Bytes(), &r2)
	assert.Equal(t, int64(1000), r2.Result.TotalDurationSeconds)

	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?version=%d", userID, expID, v1), nil)
	srv.ServeHTTP(w3, req3)
	var r3 models.ResultResponse
	json.Unmarshal(w3.Body.Bytes(), &r3)
	assert.Equal(t, int64(600), r3.Result.TotalDurationSeconds, "version v1 should remain unchanged")
}

func TestConcurrentEventUpload(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "concurrent-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	sharedEventID := uuid.New().String()
	sharedEvent := models.EventInput{
		EventID:      sharedEventID,
		UserID:       userID.String(),
		ExperimentID: expID.String(),
		DeviceID:     "dev1",
		ClientSeq:    1,
		EventType:    models.EventCardView,
		OccurredAt:   base,
		Payload:      json.RawMessage(`{}`),
	}

	var wg sync.WaitGroup
	acceptedCount := 0
	duplicateCount := 0
	var mu sync.Mutex

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := sendEvents(t, srv, userID, expID, []models.EventInput{sharedEvent})
			mu.Lock()
			acceptedCount += resp.Accepted
			duplicateCount += resp.Duplicate
			mu.Unlock()
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, acceptedCount, "exactly one concurrent insert should succeed")
	assert.Equal(t, 19, duplicateCount, "all others should be duplicates")
}

func TestCrossDeviceConcurrentUpload(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "cross-device-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	const numDevices = 5
	const eventsPerDevice = 10

	var wg sync.WaitGroup
	totalAccepted := 0
	var mu sync.Mutex

	for d := 0; d < numDevices; d++ {
		wg.Add(1)
		go func(deviceIdx int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("device-%d", deviceIdx)
			var evs []models.EventInput
			for i := 0; i < eventsPerDevice; i++ {
				evs = append(evs, makeCardViewEvent(userID, expID, deviceID, int64(i+1), base.Add(time.Duration(i)*time.Minute)))
			}
			resp := sendEvents(t, srv, userID, expID, evs)
			mu.Lock()
			totalAccepted += resp.Accepted
			mu.Unlock()
		}(d)
	}
	wg.Wait()

	assert.Equal(t, numDevices*eventsPerDevice, totalAccepted)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	assert.Equal(t, numDevices*eventsPerDevice, result.Result.TotalCardViews)
}

func TestAdultDailyLimit(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "adult-limit")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	events := []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 3600),
		makeWatchingEvent(userID, expID, "dev1", 2, base.Add(time.Hour), 601),
	}
	sendEvents(t, srv, userID, expID, events)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?daily=true", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)

	assert.GreaterOrEqual(t, result.Result.ViolationCount, 1)
	assert.Len(t, result.Daily, 1)
	var violations []models.RuleViolation
	json.Unmarshal(result.Daily[0].Violations, &violations)
	foundDaily := false
	foundSession := false
	for _, v := range violations {
		if v.Rule == "adult_daily_60min" {
			foundDaily = true
		}
		if v.Rule == "adult_session_15min" {
			foundSession = true
		}
	}
	assert.True(t, foundDaily, "should flag daily limit exceeded: total=4201")
	assert.True(t, foundSession, "should flag session 601s > 900s? wait 601 < 900, so no session violation; but 3600 > 900!")
}

func TestAdultSessionLimit(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "adult-session")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 901),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?daily=true", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	var violations []models.RuleViolation
	json.Unmarshal(result.Daily[0].Violations, &violations)
	found := false
	for _, v := range violations {
		if v.Rule == "adult_session_15min" {
			found = true
		}
	}
	assert.True(t, found, "session of 901s should exceed 900s (15min)")
}

func TestChildSessionLimit(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "2018-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "child-session")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 601),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?daily=true", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	var violations []models.RuleViolation
	json.Unmarshal(result.Daily[0].Violations, &violations)
	found := false
	for _, v := range violations {
		if v.Rule == "child_session_10min" {
			found = true
		}
	}
	assert.True(t, found, "session of 601s should exceed 600s (10min) for child")
}

func TestTeenDailyLimit(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "2010-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "teen-daily")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 1801),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?daily=true", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	var violations []models.RuleViolation
	json.Unmarshal(result.Daily[0].Violations, &violations)
	found := false
	for _, v := range violations {
		if v.Rule == "teen_daily_30min" {
			found = true
		}
	}
	assert.True(t, found, "daily 1801s should exceed teen limit 1800s")
}

func TestTeenBedtimeViolation(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "2010-01-01", "UTC", "22:00")
	expID := createTestExperiment(t, srv, "teen-bedtime")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 21, 30, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 600),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?daily=true", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	var violations []models.RuleViolation
	json.Unmarshal(result.Daily[0].Violations, &violations)
	found := false
	for _, v := range violations {
		if v.Rule == "teen_no_usage_1h_before_bed" {
			found = true
		}
	}
	assert.True(t, found, "usage at 21:30 with bedtime 22:00 should be within 1h buffer")
}

func TestTimezoneCrossDay(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "Asia/Shanghai", "")
	expID := createTestExperiment(t, srv, "tz-crossday")
	grantAuth(t, srv, userID, expID)

	eventTime := time.Date(2026, 7, 19, 16, 30, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, eventTime, 1800),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?daily=true", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	require.Len(t, result.Daily, 1)
	assert.Equal(t, "2026-07-20", result.Daily[0].UserDate.Format("2006-01-02"),
		"16:30 UTC = 00:30 CST on July 20, should be in July 20 bucket")
}

func TestReplayConsistency(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "replay-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	var allEvents []models.EventInput
	for i := 0; i < 10; i++ {
		allEvents = append(allEvents, makeWatchingEvent(userID, expID, "dev1", int64(i+1), base.Add(time.Duration(i)*time.Minute), 300))
	}

	shuffled := make([]models.EventInput, len(allEvents))
	copy(shuffled, allEvents)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	firstBatch := shuffled[:5]
	secondBatch := shuffled[5:]

	sendEvents(t, srv, userID, expID, firstBatch)
	sendEvents(t, srv, userID, expID, secondBatch)

	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w1, req1)
	var incremental models.ResultResponse
	json.Unmarshal(w1.Body.Bytes(), &incremental)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/replay", userID, expID), nil)
	srv.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w3, req3)
	var replayed models.ResultResponse
	json.Unmarshal(w3.Body.Bytes(), &replayed)

	assert.Equal(t, incremental.Result.TotalDurationSeconds, replayed.Result.TotalDurationSeconds,
		"replay must produce same total duration")
	assert.Equal(t, incremental.Result.TotalCardViews, replayed.Result.TotalCardViews)
	assert.Equal(t, incremental.Result.TotalWatchingSessions, replayed.Result.TotalWatchingSessions)
	assert.Equal(t, incremental.Result.ViolationCount, replayed.Result.ViolationCount)
}

func TestVersionRecalculation(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "recalc-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	resp1 := sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 300),
	})
	v1 := resp1.Version

	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 2, base.Add(10*time.Minute), 300),
	})

	wRecalc := httptest.NewRecorder()
	reqRecalc, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/recalculate/%d", userID, expID, v1), nil)
	srv.ServeHTTP(wRecalc, reqRecalc)
	require.Equal(t, http.StatusOK, wRecalc.Code, wRecalc.Body.String())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?version=%d", userID, expID, v1), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	assert.Equal(t, int64(300), result.Result.TotalDurationSeconds, "recalculated v1 should reflect only first event")
}

func TestRevokeAuthorization(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "revoke-test")
	grantAuth(t, srv, userID, expID)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/revoke", userID, expID), nil)
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	body := map[string]interface{}{
		"events": []models.EventInput{makeCardViewEvent(userID, expID, "dev1", 1, base)},
	}
	b, _ := json.Marshal(body)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/events", userID, expID), bytes.NewReader(b))
	req2.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusForbidden, w2.Code, "after revoke, events should be forbidden")
}

func TestHardDeleteUser(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "delete-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 600),
		makeCardViewEvent(userID, expID, "dev1", 2, base),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/api/v1/users/%s", userID), nil)
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM raw_events WHERE user_id = $1", userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "raw_events should be deleted")

	err = db.QueryRow("SELECT COUNT(*) FROM experiment_results WHERE user_id = $1", userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "experiment_results should be deleted")

	err = db.QueryRow("SELECT COUNT(*) FROM daily_aggregates WHERE user_id = $1", userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "daily_aggregates should be deleted")

	err = db.QueryRow("SELECT COUNT(*) FROM event_ingestion_log WHERE user_id = $1", userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "event_ingestion_log should be deleted")

	err = db.QueryRow("SELECT COUNT(*) FROM users WHERE id = $1", userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "user record should be deleted")

	err = db.QueryRow("SELECT COUNT(*) FROM authorization_grants WHERE user_id = $1", userID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "auth grants should be deleted")

	var delCount int
	err = db.QueryRow("SELECT COUNT(*) FROM deletion_records WHERE scope = 'user'").Scan(&delCount)
	require.NoError(t, err)
	assert.Equal(t, 1, delCount, "deletion record should exist with hashed scope")
}

func TestHardDeleteExperiment(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "delete-exp-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 600),
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/api/v1/experiments/%s", expID), nil)
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM raw_events WHERE experiment_id = $1", expID).Scan(&count)
	assert.Equal(t, 0, count)
	db.QueryRow("SELECT COUNT(*) FROM experiments WHERE id = $1", expID).Scan(&count)
	assert.Equal(t, 0, count)
	db.QueryRow("SELECT COUNT(*) FROM deletion_records WHERE scope = 'experiment'").Scan(&count)
	assert.Equal(t, 1, count)
}

func TestTransactionRollback(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "rollback-test")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	invalidEvents := []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 300),
		{
			EventID:      "not-a-uuid",
			UserID:       userID.String(),
			ExperimentID: expID.String(),
			DeviceID:     "dev1",
			ClientSeq:    2,
			EventType:    models.EventCardView,
			OccurredAt:   base,
			Payload:      json.RawMessage(`{}`),
		},
	}

	body := map[string]interface{}{"events": invalidEvents}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/events", userID, expID), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM raw_events WHERE user_id = $1 AND experiment_id = $2", userID, expID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "after rollback, no events should be persisted")
}

func TestPropertyReplayDeterminism(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "prop-determinism")
	grantAuth(t, srv, userID, expID)

	rng := rand.New(rand.NewSource(42))
	base := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)

	var allEvents []models.EventInput
	for i := 0; i < 20; i++ {
		et := models.EventCardView
		if rng.Intn(3) == 0 {
			et = models.EventWatchingSession
		}
		dur := int64(0)
		payload := json.RawMessage(`{}`)
		if et == models.EventWatchingSession {
			dur = int64(rng.Intn(1200))
			payload = json.RawMessage(fmt.Sprintf(`{"duration_seconds":%d}`, dur))
		}
		allEvents = append(allEvents, models.EventInput{
			EventID:      uuid.New().String(),
			UserID:       userID.String(),
			ExperimentID: expID.String(),
			DeviceID:     "dev1",
			ClientSeq:    int64(i + 1),
			EventType:    et,
			OccurredAt:   base.Add(time.Duration(rng.Intn(480)) * time.Minute),
			Payload:      payload,
		})
	}

	sorted := make([]models.EventInput, len(allEvents))
	copy(sorted, allEvents)

	for i := 0; i < 3; i++ {
		shuffled := make([]models.EventInput, len(allEvents))
		copy(shuffled, allEvents)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		sendEvents(t, srv, userID, expID, shuffled)
	}

	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/replay", userID, expID), nil)
	srv.ServeHTTP(w1, req1)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w2, req2)
	var result1 models.ResultResponse
	json.Unmarshal(w2.Body.Bytes(), &result1)

	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/replay", userID, expID), nil)
	srv.ServeHTTP(w3, req3)

	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w4, req4)
	var result2 models.ResultResponse
	json.Unmarshal(w4.Body.Bytes(), &result2)

	assert.Equal(t, result1.Result.TotalDurationSeconds, result2.Result.TotalDurationSeconds)
	assert.Equal(t, result1.Result.ViolationCount, result2.Result.ViolationCount)
}

func TestFaultInjectionContextCancel(t *testing.T) {
	db := setupTestDB(t)

	us := store.NewUserStore(db)
	es := store.NewExperimentStore(db)
	evs := store.NewEventStore(db)
	rs := store.NewResultStore(db)
	as := store.NewAuthStore(db)
	ds := store.NewDeletionStore(db)
	proc := service.NewEventProcessor(db, evs, rs, es, us)
	_ = as
	_ = ds

	bd := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	user, err := us.Create(context.Background(), bd, "UTC", "")
	require.NoError(t, err)
	exp, err := es.Create(context.Background(), "fault-test", nil)
	require.NoError(t, err)
	as.Grant(context.Background(), user.ID, exp.ID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	_, err = proc.IngestBatch(ctx, user.ID, exp.ID, []models.EventInput{
		makeWatchingEvent(user.ID, exp.ID, "dev1", 1, base, 300),
	})
	assert.Error(t, err, "cancelled context should cause error")

	var count int
	db.QueryRow("SELECT COUNT(*) FROM raw_events WHERE user_id = $1", user.ID).Scan(&count)
	assert.Equal(t, 0, count, "no events should be persisted after cancelled context")
}

func TestNonDiagnosticErrorOutput(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	b := []byte(`{"events": "not-an-array"}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/users/00000000-0000-0000-0000-000000000000/experiments/00000000-0000-0000-0000-000000000000/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(w, req)

	assert.NotContains(t, w.Body.String(), "pgx", "should not leak driver info")
	assert.NotContains(t, w.Body.String(), "database", "should not leak database internals")
}

func TestAllEventTypes(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "all-types")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	events := []models.EventInput{
		{EventID: uuid.New().String(), UserID: userID.String(), ExperimentID: expID.String(), DeviceID: "dev1", ClientSeq: 1, EventType: models.EventCardView, OccurredAt: base, Payload: json.RawMessage(`{}`)},
		{EventID: uuid.New().String(), UserID: userID.String(), ExperimentID: expID.String(), DeviceID: "dev1", ClientSeq: 2, EventType: models.EventAttentionSwitch, OccurredAt: base.Add(time.Minute), Payload: json.RawMessage(`{"from":"card1","to":"card2"}`)},
		{EventID: uuid.New().String(), UserID: userID.String(), ExperimentID: expID.String(), DeviceID: "dev1", ClientSeq: 3, EventType: models.EventSlowReading, OccurredAt: base.Add(2*time.Minute), Payload: json.RawMessage(`{"question_id":"q1","answer":"A","response_time_ms":5000}`)},
		{EventID: uuid.New().String(), UserID: userID.String(), ExperimentID: expID.String(), DeviceID: "dev1", ClientSeq: 4, EventType: models.EventWatchingSession, OccurredAt: base.Add(3*time.Minute), Payload: json.RawMessage(`{"duration_seconds":600}`)},
	}
	sendEvents(t, srv, userID, expID, events)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), nil)
	srv.ServeHTTP(w, req)
	var result models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &result)
	assert.Equal(t, 1, result.Result.TotalCardViews)
	assert.Equal(t, 1, result.Result.TotalAttentionSwitches)
	assert.Equal(t, 1, result.Result.TotalSlowReading)
	assert.Equal(t, 1, result.Result.TotalWatchingSessions)
	assert.Equal(t, int64(600), result.Result.TotalDurationSeconds)
}
