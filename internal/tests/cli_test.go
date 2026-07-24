package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"brainbreak-lab/internal/models"
	"brainbreak-lab/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildBinary(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	binaryPath := filepath.Join(tmpDir, "brainbreak-server")
	projDir := findProjectRoot(t)
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/server/")
	cmd.Dir = projDir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build failed: %s", string(out))
	return binaryPath
}

func findProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (go.mod)")
		}
		dir = parent
	}
}

func waitForHealth(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/health")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("server did not become healthy within %s (%s)", timeout, baseURL)
}

func TestStandaloneBinaryWithEmbeddedMigrations(t *testing.T) {
	bin := buildBinary(t)

	adminDB, err := store.Open("postgres://brainbreak:brainbreak@localhost:5432/postgres?sslmode=disable")
	require.NoError(t, err)
	defer adminDB.Close()
	dbName := "bb_test_standalone_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	adminDB.Exec("DROP DATABASE IF EXISTS " + dbName)
	_, err = adminDB.Exec("CREATE DATABASE " + dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		adminDB, _ := store.Open("postgres://brainbreak:brainbreak@localhost:5432/postgres?sslmode=disable")
		if adminDB != nil {
			adminDB.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
			adminDB.Close()
		}
	})

	workDir := t.TempDir()
	dbURL := fmt.Sprintf("postgres://brainbreak:brainbreak@localhost:5432/%s?sslmode=disable", dbName)
	port := 19000 + (os.Getpid() % 900)
	addr := fmt.Sprintf(":%d", port)

	cmd := exec.Command(bin)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"SERVER_ADDR="+addr,
		"GOTOOLCHAIN=local",
	)
	stderr, _ := cmd.StderrPipe()
	stdout, _ := cmd.StdoutPipe()
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cmd.Process.Kill() })

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	waitForHealth(t, baseURL, 15*time.Second)

	resp, err := http.Post(baseURL+"/api/v1/users", "application/json",
		strings.NewReader(`{"birth_date":"1990-01-01","timezone":"UTC"}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	_ = stderr
	_ = stdout
	_ = cmd.Process.Kill()
}

func TestMIGRATIONS_DIR_Override(t *testing.T) {
	bin := buildBinary(t)

	adminDB, err := store.Open("postgres://brainbreak:brainbreak@localhost:5432/postgres?sslmode=disable")
	require.NoError(t, err)
	defer adminDB.Close()
	dbName := "bb_test_migdir_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	adminDB.Exec("DROP DATABASE IF EXISTS " + dbName)
	_, err = adminDB.Exec("CREATE DATABASE " + dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		adminDB, _ := store.Open("postgres://brainbreak:brainbreak@localhost:5432/postgres?sslmode=disable")
		if adminDB != nil {
			adminDB.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
			adminDB.Close()
		}
	})

	migDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(migDir, "001_init.sql"),
		[]byte(`
CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY,
    birth_date DATE NOT NULL,
    timezone VARCHAR(64) NOT NULL DEFAULT 'UTC',
    bedtime VARCHAR(5) NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS experiments (
    id UUID PRIMARY KEY, version INT NOT NULL DEFAULT 1,
    name VARCHAR(256) NOT NULL, config JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS raw_events (
    id BIGSERIAL PRIMARY KEY, event_id UUID NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id), experiment_id UUID NOT NULL REFERENCES experiments(id),
    device_id VARCHAR(128) NOT NULL, client_seq BIGINT NOT NULL,
    event_type VARCHAR(32) NOT NULL, occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT uq_raw_events_event_id UNIQUE (event_id)
);
CREATE TABLE IF NOT EXISTS event_ingestion_log (
    event_id UUID PRIMARY KEY, user_id UUID NOT NULL, experiment_id UUID NOT NULL,
    accepted BOOLEAN NOT NULL, version INT NOT NULL, ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS authorization_grants (
    user_id UUID NOT NULL, experiment_id UUID NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), revoked_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, experiment_id)
);
CREATE TABLE IF NOT EXISTS daily_aggregates (
    id BIGSERIAL PRIMARY KEY, user_id UUID NOT NULL, experiment_id UUID NOT NULL,
    user_date DATE NOT NULL, total_duration_seconds BIGINT NOT NULL DEFAULT 0,
    session_count INT NOT NULL DEFAULT 0, longest_session_seconds BIGINT NOT NULL DEFAULT 0,
    card_view_count INT NOT NULL DEFAULT 0, attention_switch_count INT NOT NULL DEFAULT 0,
    slow_reading_count INT NOT NULL DEFAULT 0, watching_session_count INT NOT NULL DEFAULT 0,
    violations JSONB NOT NULL DEFAULT '[]'::jsonb, version INT NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS experiment_results (
    id BIGSERIAL PRIMARY KEY, user_id UUID NOT NULL, experiment_id UUID NOT NULL,
    version INT NOT NULL, result_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    total_duration_seconds BIGINT NOT NULL DEFAULT 0, total_card_views INT NOT NULL DEFAULT 0,
    total_attention_switches INT NOT NULL DEFAULT 0, total_slow_reading INT NOT NULL DEFAULT 0,
    total_watching_sessions INT NOT NULL DEFAULT 0, violation_count INT NOT NULL DEFAULT 0,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS deletion_records (
    id UUID PRIMARY KEY, deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    scope VARCHAR(32) NOT NULL, scope_hash VARCHAR(64) NOT NULL
);
`), 0644))

	workDir := t.TempDir()
	dbURL := fmt.Sprintf("postgres://brainbreak:brainbreak@localhost:5432/%s?sslmode=disable", dbName)
	port := 20000 + (os.Getpid() % 900)
	addr := fmt.Sprintf(":%d", port)

	cmd := exec.Command(bin)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"SERVER_ADDR="+addr,
		"MIGRATIONS_DIR="+migDir,
		"GOTOOLCHAIN=local",
	)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cmd.Process.Kill() })

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	waitForHealth(t, baseURL, 15*time.Second)

	resp, err := http.Get(baseURL + "/health")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	cmd.Process.Kill()
}

func TestMigrationFailureAbortsStartup(t *testing.T) {
	bin := buildBinary(t)

	adminDB, err := store.Open("postgres://brainbreak:brainbreak@localhost:5432/postgres?sslmode=disable")
	require.NoError(t, err)
	defer adminDB.Close()
	dbName := "bb_test_migfail_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	adminDB.Exec("DROP DATABASE IF EXISTS " + dbName)
	_, err = adminDB.Exec("CREATE DATABASE " + dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		adminDB, _ := store.Open("postgres://brainbreak:brainbreak@localhost:5432/postgres?sslmode=disable")
		if adminDB != nil {
			adminDB.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
			adminDB.Close()
		}
	})

	badMigDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(badMigDir, "001_bad.sql"),
		[]byte(`THIS IS NOT VALID SQL AT ALL!!! *(&^%$#`), 0644))

	workDir := t.TempDir()
	dbURL := fmt.Sprintf("postgres://brainbreak:brainbreak@localhost:5432/%s?sslmode=disable", dbName)
	port := 21000 + (os.Getpid() % 900)
	addr := fmt.Sprintf(":%d", port)

	cmd := exec.Command(bin)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dbURL,
		"SERVER_ADDR="+addr,
		"MIGRATIONS_DIR="+badMigDir,
		"GOTOOLCHAIN=local",
	)
	output, err := cmd.CombinedOutput()
	require.Error(t, err, "server should exit with error on migration failure")
	assert.Contains(t, string(output), "migration failed",
		"should log migration failure message, got: %s", string(output))

	db, err := store.Open(dbURL)
	if err == nil {
		defer db.Close()
		var tblCount int
		db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tblCount)
		assert.Equal(t, 0, tblCount, "no tables should exist after failed migration")
	}
}

func TestConcurrentIdempotencyAcrossBatches(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "concurrent-batches")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	const numBatches = 10
	const uniqueEventsPerBatch = 5

	var wg sync.WaitGroup
	totalAccepted := 0
	totalDuplicate := 0
	var mu sync.Mutex

	sharedEventID := uuid.New().String()
	sharedEvent := models.EventInput{
		EventID:      sharedEventID,
		UserID:       userID.String(),
		ExperimentID: expID.String(),
		DeviceID:     "dev1",
		ClientSeq:    999,
		EventType:    models.EventCardView,
		OccurredAt:   base,
		Payload:      json.RawMessage(`{}`),
	}

	for i := 0; i < numBatches; i++ {
		wg.Add(1)
		go func(batchIdx int) {
			defer wg.Done()
			evs := []models.EventInput{sharedEvent}
			for j := 0; j < uniqueEventsPerBatch; j++ {
				evs = append(evs, models.EventInput{
					EventID:      uuid.New().String(),
					UserID:       userID.String(),
					ExperimentID: expID.String(),
					DeviceID:     fmt.Sprintf("dev-%d", batchIdx),
					ClientSeq:    int64(j + 1),
					EventType:    models.EventCardView,
					OccurredAt:   base.Add(time.Duration(batchIdx*uniqueEventsPerBatch+j) * time.Minute),
					Payload:      json.RawMessage(`{}`),
				})
			}
			resp := sendEvents(t, srv, userID, expID, evs)
			mu.Lock()
			totalAccepted += resp.Accepted
			totalDuplicate += resp.Duplicate
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	expectedAccepted := numBatches*uniqueEventsPerBatch + 1
	assert.Equal(t, expectedAccepted, totalAccepted,
		"shared event accepted exactly once plus all unique events")
	assert.Equal(t, numBatches-1, totalDuplicate,
		"shared event duplicated numBatches-1 times across concurrent batches")
}

func TestLateEventMultipleLateArrivals(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "multi-late")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

	r1 := sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 10, base.Add(50*time.Minute), 600),
	})
	v10 := r1.Version

	r2 := sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 5, base.Add(20*time.Minute), 400),
	})
	v5 := r2.Version
	assert.True(t, v5 > v10, "late event seq=5 should create new version")

	r3 := sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 300),
	})
	v1 := r3.Version
	assert.True(t, v1 > v5, "very late event seq=1 should create another new version")

	w := httptestNewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), srv)
	var finalResult models.ResultResponse
	json.Unmarshal(w.Body.Bytes(), &finalResult)
	assert.Equal(t, int64(1300), finalResult.Result.TotalDurationSeconds,
		"after all late events, total should be 300+400+600=1300")

	w1 := httptestNewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?version=%d", userID, expID, v10), srv)
	var r1Result models.ResultResponse
	json.Unmarshal(w1.Body.Bytes(), &r1Result)
	assert.Equal(t, int64(600), r1Result.Result.TotalDurationSeconds,
		"version v10 (before late events) should still show 600s")

	w2 := httptestNewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result?version=%d", userID, expID, v5), srv)
	var r2Result models.ResultResponse
	json.Unmarshal(w2.Body.Bytes(), &r2Result)
	assert.Equal(t, int64(1000), r2Result.Result.TotalDurationSeconds,
		"version v5 should show 1000s (600+400)")
}

func httptestNewRequest(method, path string, srv *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(method, path, nil)
	srv.ServeHTTP(w, req)
	return w
}

func TestTransactionRollbackOnPartialFailure(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "rollback-partial")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	badEvents := []models.EventInput{
		makeCardViewEvent(userID, expID, "dev1", 1, base),
		makeWatchingEvent(userID, expID, "dev1", 2, base.Add(time.Minute), 300),
		{
			EventID:      "invalid-uuid-format",
			UserID:       userID.String(),
			ExperimentID: expID.String(),
			DeviceID:     "dev1",
			ClientSeq:    3,
			EventType:    models.EventCardView,
			OccurredAt:   base.Add(2 * time.Minute),
			Payload:      json.RawMessage(`{}`),
		},
	}

	body := map[string]interface{}{"events": badEvents}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("/api/v1/users/%s/experiments/%s/events", userID, expID),
		strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM raw_events WHERE user_id = $1 AND experiment_id = $2", userID, expID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "after rollback, zero events should be persisted")

	err = db.QueryRow("SELECT COUNT(*) FROM experiment_results WHERE user_id = $1 AND experiment_id = $2", userID, expID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no results should exist after rollback")
}

func TestReplayAfterRollbackIsIdempotent(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "replay-rollback")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 600),
		makeCardViewEvent(userID, expID, "dev1", 2, base.Add(time.Minute)),
	})

	w1 := httptestNewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/replay", userID, expID), srv)
	require.Equal(t, http.StatusOK, w1.Code)

	w2 := httptestNewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), srv)
	var r1 models.ResultResponse
	json.Unmarshal(w2.Body.Bytes(), &r1)
	v1 := r1.Result.Version
	dur1 := r1.Result.TotalDurationSeconds

	w3 := httptestNewRequest("POST", fmt.Sprintf("/api/v1/users/%s/experiments/%s/replay", userID, expID), srv)
	require.Equal(t, http.StatusOK, w3.Code)

	w4 := httptestNewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID, expID), srv)
	var r2 models.ResultResponse
	json.Unmarshal(w4.Body.Bytes(), &r2)
	assert.Equal(t, dur1, r2.Result.TotalDurationSeconds, "replay must produce same total duration")
	assert.Greater(t, r2.Result.Version, v1, "each replay creates a new version")
}

func TestHardDeleteThenRecreateUser(t *testing.T) {
	db := setupTestDB(t)
	srv, _ := setupTestServer(t, db)

	userID := createTestUser(t, srv, "1990-01-01", "UTC", "")
	expID := createTestExperiment(t, srv, "delete-recreate")
	grantAuth(t, srv, userID, expID)

	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	sendEvents(t, srv, userID, expID, []models.EventInput{
		makeWatchingEvent(userID, expID, "dev1", 1, base, 600),
	})

	w := httptestNewRequest("DELETE", fmt.Sprintf("/api/v1/users/%s", userID), srv)
	require.Equal(t, http.StatusOK, w.Code)

	userID2 := createTestUser(t, srv, "1990-01-01", "UTC", "")
	grantAuth(t, srv, userID2, expID)
	sendEvents(t, srv, userID2, expID, []models.EventInput{
		makeWatchingEvent(userID2, expID, "dev1", 1, base, 300),
	})

	w2 := httptestNewRequest("GET", fmt.Sprintf("/api/v1/users/%s/experiments/%s/result", userID2, expID), srv)
	var result models.ResultResponse
	json.Unmarshal(w2.Body.Bytes(), &result)
	assert.Equal(t, int64(300), result.Result.TotalDurationSeconds,
		"new user should only see their own events, not deleted user's data")
}
