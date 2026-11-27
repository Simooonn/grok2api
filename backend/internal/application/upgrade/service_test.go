package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func setupControlDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeHeartbeat(t *testing.T, dir string, updatedAt time.Time, currentTag string) {
	t.Helper()
	data, err := json.Marshal(agentHeartbeatFile{
		Version:    1,
		ImageRepo:  "ghcr.io/simooonn/grok2api",
		CurrentTag: currentTag,
		UpdatedAt:  updatedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outbox", "agent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListReleasesFiltersForkTagsAndDrafts(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/repos/Simooonn/grok2api/releases" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		body := `[
			{"tag_name":"v3.1.8-roamai.2","name":"v3.1.8-roamai.2","html_url":"https://example.test/2","body":"New","draft":false,"published_at":"2026-11-27T01:00:00Z"},
			{"tag_name":"v3.1.8-roamai.3","name":"draft","html_url":"https://example.test/3","body":"Draft","draft":true,"published_at":"2026-11-27T02:00:00Z"},
			{"tag_name":"v3.1.8","name":"upstream","html_url":"https://example.test/u","body":"Upstream","draft":false,"published_at":"2026-11-20T00:00:00Z"},
			{"tag_name":"v3.1.8-roamai.1","name":"","html_url":"https://example.test/1","body":"Init","draft":false,"published_at":"2026-11-26T01:00:00Z"}
		]`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	svc := NewService(Config{CurrentVersion: "v3.1.8-roamai.1"}, client)
	list, err := svc.ListReleases(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Tag != "v3.1.8-roamai.2" || list[1].Tag != "v3.1.8-roamai.1" || list[1].Name != "v3.1.8-roamai.1" {
		t.Fatalf("list = %#v", list)
	}
	// Second non-forced call hits cache.
	if _, err := svc.ListReleases(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", calls)
	}
	// Forced call refreshes.
	if _, err := svc.ListReleases(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 HTTP calls after force, got %d", calls)
	}
}

func TestRequestUpgradeValidationAndAtomicity(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	// Disabled without control dir.
	disabledSvc := NewService(Config{}, nil)
	if _, err := disabledSvc.RequestUpgrade(context.Background(), "v3.1.8-roamai.1", "admin"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}

	dir := setupControlDir(t)
	svc := NewService(Config{ControlDir: dir, CurrentVersion: "v3.1.8-roamai.1"}, nil)
	svc.now = func() time.Time { return now }

	// Rejects non-fork tags.
	for _, bad := range []string{"latest", "v3.1.8", "v3.1.8-roamai.0", "v3.1.8-roamai.1;rm -rf /", "../bad"} {
		if _, err := svc.RequestUpgrade(context.Background(), bad, "admin"); !errors.Is(err, ErrInvalidTag) {
			t.Fatalf("tag %q: err = %v, want ErrInvalidTag", bad, err)
		}
	}

	// Rejects when heartbeat missing or stale.
	if _, err := svc.RequestUpgrade(context.Background(), "v3.1.8-roamai.2", "admin"); !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("err = %v, want ErrAgentOffline", err)
	}
	writeHeartbeat(t, dir, now.Add(-10*time.Minute), "v3.1.8-roamai.1")
	if _, err := svc.RequestUpgrade(context.Background(), "v3.1.8-roamai.2", "admin"); !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("stale heartbeat: err = %v, want ErrAgentOffline", err)
	}

	// Accepts when heartbeat fresh.
	writeHeartbeat(t, dir, now.Add(-30*time.Second), "v3.1.8-roamai.1")
	op, err := svc.RequestUpgrade(context.Background(), "v3.1.8-roamai.2", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if op.Phase != "queued" || op.Tag != "v3.1.8-roamai.2" || len(op.ID) != 16 {
		t.Fatalf("op = %#v", op)
	}

	// Status immediately reflects queued request, and second request is rejected as in-progress.
	st := svc.Status()
	if !st.AgentOnline || !st.InProgress || st.Operation == nil || st.Operation.Phase != "queued" {
		t.Fatalf("status = %#v", st)
	}
	if _, err := svc.RequestUpgrade(context.Background(), "v3.1.8-roamai.2", "admin"); !errors.Is(err, ErrUpgradeInProgress) {
		t.Fatalf("err = %v, want ErrUpgradeInProgress", err)
	}

	// Verify inbox/request.json content.
	raw, err := os.ReadFile(filepath.Join(dir, "inbox", "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload requestFilePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ID != op.ID || payload.Tag != "v3.1.8-roamai.2" || payload.RequestedBy != "admin" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestStatusReadsOutboxOperationAndStaleRecovery(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	dir := setupControlDir(t)
	writeHeartbeat(t, dir, now.Add(-10*time.Second), "v3.1.8-roamai.1")

	svc := NewService(Config{ControlDir: dir, CurrentVersion: "v3.1.8-roamai.1"}, nil)
	svc.now = func() time.Time { return now }

	// Active recent status -> InProgress=true.
	activeJSON := `{"id":"0123456789abcdef","tag":"v3.1.8-roamai.2","phase":"pulling","message":"Pulling","from_version":"v3.1.8-roamai.1","requested_by":"admin","updated_at":"2026-11-27T11:59:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "outbox", "status.json"), []byte(activeJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := svc.Status(); !st.InProgress || st.Operation == nil || st.Operation.Phase != "pulling" {
		t.Fatalf("status = %#v", st)
	}

	// Stale active status (>15m) -> InProgress=false so admin is not locked out after a host crash.
	staleJSON := `{"id":"0123456789abcdef","tag":"v3.1.8-roamai.2","phase":"pulling","message":"Pulling","from_version":"v3.1.8-roamai.1","requested_by":"admin","updated_at":"2026-11-27T11:30:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "outbox", "status.json"), []byte(staleJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := svc.Status(); st.InProgress {
		t.Fatalf("stale operation should not lock out upgrades: %#v", st)
	}
}
