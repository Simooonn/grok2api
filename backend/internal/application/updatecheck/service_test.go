package updatecheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCheckFindsLatestRelease(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != latestReleaseAPI || request.Header.Get("User-Agent") != "grok2api/v3.0.0" {
			t.Fatalf("request = %#v", request)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v3.0.1","body":"Release notes"}`)), Header: make(http.Header)}, nil
	})}
	service := NewService("v3.0.0", client)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	snapshot := service.Check(context.Background())
	if snapshot.Status != StatusUpdateAvailable || !snapshot.UpdateAvailable || snapshot.LatestVersion != "v3.0.1" || snapshot.CheckedAt == nil || !snapshot.CheckedAt.Equal(now) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.ReleaseURL != "https://github.com/chenyme/grok2api/releases/tag/v3.0.1" || snapshot.ReleaseNotes != "Release notes" {
		t.Fatalf("release = %#v", snapshot)
	}
}

func TestCheckFailureKeepsLastSuccessfulRelease(t *testing.T) {
	fail := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail {
			return nil, errors.New("network down")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v3.0.0","body":"Stable"}`)), Header: make(http.Header)}, nil
	})}
	service := NewService("v3.0.0", client)
	first := service.Check(context.Background())
	fail = true
	second := service.Check(context.Background())
	if first.Status != StatusUpToDate || second.Status != StatusCheckFailed || second.LatestVersion != "v3.0.0" || second.CheckedAt == nil || second.Error == "" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestSemanticVersionComparison(t *testing.T) {
	stable, ok := parseSemanticVersion("v3.0.1")
	if !ok {
		t.Fatal("stable version was rejected")
	}
	older, _ := parseSemanticVersion("3.0.0")
	prerelease, _ := parseSemanticVersion("v3.0.1-rc.1")
	if compareSemanticVersion(stable, older) <= 0 || compareSemanticVersion(prerelease, stable) >= 0 {
		t.Fatal("semantic version ordering is invalid")
	}
	if _, ok := parseSemanticVersion("dev"); ok {
		t.Fatal("development version was accepted as semver")
	}
	base, _ := parseSemanticVersion("v3.0.8")
	hotfix1, _ := parseSemanticVersion("v3.0.8-hotfix.1")
	hotfix2, _ := parseSemanticVersion("v3.0.8-hotfix.2")
	sameBasePrerelease, _ := parseSemanticVersion("v3.0.8-rc.1")
	next, _ := parseSemanticVersion("v3.0.9")
	if compareSemanticVersion(hotfix1, base) <= 0 || compareSemanticVersion(hotfix2, hotfix1) <= 0 || compareSemanticVersion(next, hotfix2) <= 0 {
		t.Fatal("project hotfix ordering is invalid")
	}
	if compareSemanticVersion(hotfix1, sameBasePrerelease) <= 0 {
		t.Fatal("project hotfix should follow ordinary prereleases")
	}
}

func TestCheckFindsHotfixAfterStableRelease(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v3.0.8-hotfix.1"}`)), Header: make(http.Header)}, nil
	})}
	service := NewService("v3.0.8", client)
	snapshot := service.Check(context.Background())
	if snapshot.Status != StatusUpdateAvailable || !snapshot.UpdateAvailable {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestForkRevisionOrderingAndRepoOverride(t *testing.T) {
	base, ok := parseSemanticVersion("v3.1.8")
	if !ok {
		t.Fatal("base version rejected")
	}
	rev1, ok := parseSemanticVersion("v3.1.8-roamai.1")
	if !ok {
		t.Fatal("fork rev 1 rejected")
	}
	rev2, _ := parseSemanticVersion("v3.1.8-roamai.2")
	rev10, _ := parseSemanticVersion("v3.1.8-roamai.10")
	next, _ := parseSemanticVersion("v3.1.9")
	if compareSemanticVersion(rev1, base) <= 0 || compareSemanticVersion(rev2, rev1) <= 0 || compareSemanticVersion(rev10, rev2) <= 0 || compareSemanticVersion(next, rev10) <= 0 {
		t.Fatal("fork revision ordering is invalid")
	}

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.github.com/repos/Simooonn/grok2api/releases/latest" {
			t.Fatalf("unexpected url: %s", request.URL.String())
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v3.1.8-roamai.2","body":"Fork notes"}`)), Header: make(http.Header)}, nil
	})}
	service := NewService("v3.1.8-roamai.1", client).WithRepo("Simooonn/grok2api")
	snapshot := service.Check(context.Background())
	if snapshot.Status != StatusUpdateAvailable || !snapshot.UpdateAvailable || snapshot.ReleaseURL != "https://github.com/Simooonn/grok2api/releases/tag/v3.1.8-roamai.2" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
