package system

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	upgradeapp "github.com/chenyme/grok2api/backend/internal/application/upgrade"
	"github.com/gin-gonic/gin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestHandlerReturnsOnlyPublicFrontendConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(func() string { return "https://api.example.com/" }, updatecheckapp.NewService("v3.0.0", nil)).Register(router.Group("/api/admin/v1"))
	request := httptest.NewRequest(http.MethodGet, "/api/admin/v1/system", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data["publicApiBaseURL"] != "https://api.example.com" {
		t.Fatalf("data = %#v", payload.Data)
	}
}

func TestHandlerReturnsAndChecksVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"tag_name":"v3.0.1","body":"Notes"}`))}, nil
	})}
	router := gin.New()
	updates := updatecheckapp.NewService("v3.0.0", client)
	NewHandler(nil, updates).Register(router.Group("/api/admin/v1"))

	for _, test := range []struct {
		method string
		path   string
		status string
	}{
		{method: http.MethodGet, path: "/api/admin/v1/system/version", status: "unchecked"},
		{method: http.MethodPost, path: "/api/admin/v1/system/update/check", status: "update_available"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s status = %d", test.method, test.path, recorder.Code)
		}
		var payload struct {
			Data struct {
				CurrentVersion string `json:"currentVersion"`
				Status         string `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Data.CurrentVersion != "v3.0.0" || payload.Data.Status != test.status {
			t.Fatalf("%s %s data = %#v", test.method, test.path, payload.Data)
		}
	}
}

func TestHandlerUpgradeEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	hb := `{"version":1,"image_repo":"ghcr.io/simooonn/grok2api","current_tag":"v3.1.8-roamai.1","updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, "outbox", "agent.json"), []byte(hb), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `[{"tag_name":"v3.1.8-roamai.2","name":"v3.1.8-roamai.2","html_url":"https://example.test/2","body":"Fix","draft":false,"published_at":"2026-11-27T00:00:00Z"}]`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	upgrades := upgradeapp.NewService(upgradeapp.Config{ControlDir: dir, CurrentVersion: "v3.1.8-roamai.1"}, client)
	router := gin.New()
	NewHandler(nil, updatecheckapp.NewService("v3.1.8-roamai.1", nil)).WithUpgrades(upgrades).Register(router.Group("/api/admin/v1"))

	// Releases list.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/releases", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v3.1.8-roamai.2") {
		t.Fatalf("releases status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Status reports agent online.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/upgrade/status", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"agentOnline":true`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Invalid tag -> 400.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/v1/system/upgrade", strings.NewReader(`{"tag":"latest"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad tag status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Valid tag -> 202 Accepted.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/v1/system/upgrade", strings.NewReader(`{"tag":"v3.1.8-roamai.2"}`)))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"phase":"queued"`) {
		t.Fatalf("upgrade status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Second concurrent upgrade -> 409 Conflict.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/v1/system/upgrade", strings.NewReader(`{"tag":"v3.1.8-roamai.2"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate upgrade status=%d body=%s", rec.Code, rec.Body.String())
	}
}
