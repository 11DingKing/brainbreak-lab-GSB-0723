package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"focuslab/internal/service"
	"focuslab/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	fixed := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := service.New(store.NewMem(), func() time.Time { return fixed })
	srv := httptest.NewServer(NewServer(svc).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, method, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

// TestEndToEnd_HappyPath exercises create → write → result over HTTP.
func TestEndToEnd_HappyPath(t *testing.T) {
	srv := newTestServer(t)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/experiments", map[string]any{
		"name":         "study",
		"display_name": "Alice",
		"birth":        "1990-01-01T00:00:00Z",
		"timezone":     "Asia/Shanghai",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	expID := body["experiment_id"].(string)
	subID := body["subject_id"].(string)

	base := "/v1/experiments/" + expID + "/subjects/" + subID
	resp, body = doJSON(t, http.MethodPost, srv.URL+base+"/events", map[string]any{
		"events": []map[string]any{
			{"device_id": "A", "client_seq": 1, "type": "card_view", "occurred_at": "2026-06-01T09:00:00Z", "duration_ms": 300000},
			{"device_id": "A", "client_seq": 2, "type": "card_view", "occurred_at": "2026-06-01T09:05:00Z", "duration_ms": 300000},
		},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, float64(2), body["accepted"])

	resp, body = doJSON(t, http.MethodGet, srv.URL+base+"/result", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := body["result"].(map[string]any)
	require.Equal(t, float64(600000), result["total_engaged_ms"])
	require.Equal(t, "adult", result["band"])
}

// TestNonDiagnosticErrorOutput asserts that error responses carry only a stable
// machine code + fixed message, never internal error text, SQL, stack traces,
// or personal data.
func TestNonDiagnosticErrorOutput(t *testing.T) {
	srv := newTestServer(t)

	// Unknown subject/experiment → not_found, generic message only.
	resp, body := doJSON(t, http.MethodGet,
		srv.URL+"/v1/experiments/11111111-1111-1111-1111-111111111111/subjects/22222222-2222-2222-2222-222222222222/result", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	errObj := body["error"].(map[string]any)
	require.Equal(t, "not_found", errObj["code"])
	require.Equal(t, "resource not found", errObj["message"])

	// The response must contain ONLY the code and message keys under error.
	require.Len(t, errObj, 2)

	// Malformed body → invalid_request, no echo of the offending input.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/experiments", strings.NewReader(`{"name": 12345, "secret_field":"leak"}`))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp2.Body)
	resp2.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp2.StatusCode)
	// The raw response must not leak the caller's field names/values or any
	// internal detail.
	require.NotContains(t, raw.String(), "secret_field")
	require.NotContains(t, raw.String(), "leak")
	require.NotContains(t, raw.String(), "json")
}

// TestDeleteThenQueryOverHTTP confirms deletion yields a non-recoverable state.
func TestDeleteThenQueryOverHTTP(t *testing.T) {
	srv := newTestServer(t)
	_, body := doJSON(t, http.MethodPost, srv.URL+"/v1/experiments", map[string]any{
		"name": "study", "display_name": "Bob", "birth": "2015-01-01T00:00:00Z", "timezone": "UTC",
	})
	expID := body["experiment_id"].(string)
	subID := body["subject_id"].(string)
	base := "/v1/experiments/" + expID + "/subjects/" + subID

	_, _ = doJSON(t, http.MethodPost, srv.URL+base+"/events", map[string]any{
		"events": []map[string]any{
			{"device_id": "A", "client_seq": 1, "type": "card_view", "occurred_at": "2026-06-01T09:00:00Z", "duration_ms": 60000},
		},
	})

	resp, _ := doJSON(t, http.MethodDelete, srv.URL+base, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Writing again is refused with a non-diagnostic gone/conflict status.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+base+"/events", map[string]any{
		"events": []map[string]any{
			{"device_id": "A", "client_seq": 2, "type": "card_view", "occurred_at": "2026-06-01T09:10:00Z", "duration_ms": 60000},
		},
	})
	require.Equal(t, http.StatusGone, resp.StatusCode)
}
