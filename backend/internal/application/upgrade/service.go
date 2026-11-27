package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	DefaultReleaseRepo      = "Simooonn/grok2api"
	maxResponseSize         = 2 << 20
	maxControlFileSize      = 64 << 10
	maxReleaseNotesRunes    = 2048
	releasesCacheTTL        = 60 * time.Second
	agentHeartbeatMaxAge    = 5 * time.Minute
	activeOperationMaxStale = 15 * time.Minute
)

// ForkTagPattern matches fork release tags like v3.1.8-roamai.1 or v3.1.8-hotfix.1-roamai.1.
// Must stay identical to TAG_PATTERN in deploy/upgrade-agent/grok2api-upgrade-agent.py.
var ForkTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?-roamai\.[1-9][0-9]*$`)

var (
	ErrNotConfigured     = errors.New("未配置升级控制目录，无法在线升级")
	ErrInvalidTag        = errors.New("版本号格式无效，仅支持 vX.Y.Z-roamai.N")
	ErrAgentOffline      = errors.New("宿主机升级代理未在线，请先检查 grok2api-upgrade-agent 服务")
	ErrUpgradeInProgress = errors.New("当前已有升级任务正在执行，请等待完成")
)

type Config struct {
	ControlDir     string
	ReleaseRepo    string
	CurrentVersion string
}

type Release struct {
	Tag         string `json:"tag"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Notes       string `json:"notes"`
	PublishedAt string `json:"publishedAt"`
}

type Operation struct {
	ID          string `json:"id"`
	Tag         string `json:"tag"`
	Phase       string `json:"phase"`
	Message     string `json:"message"`
	FromVersion string `json:"fromVersion"`
	RequestedBy string `json:"requestedBy"`
	UpdatedAt   string `json:"updatedAt"`
}

type Status struct {
	Enabled        bool       `json:"enabled"`
	AgentOnline    bool       `json:"agentOnline"`
	AgentUpdatedAt string     `json:"agentUpdatedAt"`
	ImageRepo      string     `json:"imageRepo"`
	CurrentTag     string     `json:"currentTag"`
	CurrentVersion string     `json:"currentVersion"`
	InProgress     bool       `json:"inProgress"`
	Operation      *Operation `json:"operation"`
}

type agentHeartbeatFile struct {
	Version    int    `json:"version"`
	ImageRepo  string `json:"image_repo"`
	CurrentTag string `json:"current_tag"`
	UpdatedAt  string `json:"updated_at"`
}

type statusFilePayload struct {
	ID          string `json:"id"`
	Tag         string `json:"tag"`
	Phase       string `json:"phase"`
	Message     string `json:"message"`
	FromVersion string `json:"from_version"`
	RequestedBy string `json:"requested_by"`
	UpdatedAt   string `json:"updated_at"`
}

type requestFilePayload struct {
	ID          string `json:"id"`
	Tag         string `json:"tag"`
	FromVersion string `json:"from_version"`
	RequestedBy string `json:"requested_by"`
	RequestedAt string `json:"requested_at"`
}

type Service struct {
	controlDir     string
	releaseRepo    string
	currentVersion string
	client         *http.Client
	now            func() time.Time

	mu              sync.Mutex
	cachedReleases  []Release
	releasesFetched time.Time
}

func NewService(config Config, client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	repo := strings.Trim(strings.TrimSpace(config.ReleaseRepo), "/")
	if strings.Count(repo, "/") != 1 {
		repo = DefaultReleaseRepo
	}
	current := strings.TrimSpace(config.CurrentVersion)
	if current == "" {
		current = "dev"
	}
	return &Service{
		controlDir:     strings.TrimSpace(config.ControlDir),
		releaseRepo:    repo,
		currentVersion: current,
		client:         client,
		now:            time.Now,
	}
}

func (s *Service) Enabled() bool {
	return s.controlDir != ""
}

func (s *Service) inboxDir() string {
	return filepath.Join(s.controlDir, "inbox")
}

func (s *Service) outboxDir() string {
	return filepath.Join(s.controlDir, "outbox")
}

// ListReleases returns recent published fork releases matching ForkTagPattern.
func (s *Service) ListReleases(ctx context.Context, force bool) ([]Release, error) {
	s.mu.Lock()
	if !force && !s.releasesFetched.IsZero() && s.now().Sub(s.releasesFetched) < releasesCacheTTL {
		copied := append([]Release(nil), s.cachedReleases...)
		s.mu.Unlock()
		return copied, nil
	}
	s.mu.Unlock()

	apiURL := "https://api.github.com/repos/" + s.releaseRepo + "/releases?per_page=20"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "grok2api/"+s.currentVersion)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("查询发布版本列表失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询发布版本列表失败（HTTP %d）", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("读取发布版本列表响应: %w", err)
	}
	if len(data) > maxResponseSize {
		return nil, errors.New("发布版本列表响应超过安全上限")
	}
	var payload []struct {
		Tag         string `json:"tag_name"`
		Name        string `json:"name"`
		HTMLURL     string `json:"html_url"`
		Body        string `json:"body"`
		Draft       bool   `json:"draft"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("解析发布版本列表响应: %w", err)
	}
	releases := make([]Release, 0, len(payload))
	for _, item := range payload {
		tag := strings.TrimSpace(item.Tag)
		if item.Draft || !ForkTagPattern.MatchString(tag) {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = tag
		}
		releases = append(releases, Release{
			Tag:         tag,
			Name:        name,
			URL:         strings.TrimSpace(item.HTMLURL),
			Notes:       truncateRunes(strings.TrimSpace(item.Body), maxReleaseNotesRunes),
			PublishedAt: strings.TrimSpace(item.PublishedAt),
		})
	}

	s.mu.Lock()
	s.cachedReleases = append([]Release(nil), releases...)
	s.releasesFetched = s.now()
	s.mu.Unlock()
	return releases, nil
}

// Status reads the host upgrade agent heartbeat and latest operation status from the control dir.
func (s *Service) Status() Status {
	result := Status{
		Enabled:        s.Enabled(),
		CurrentVersion: s.currentVersion,
	}
	if !result.Enabled {
		return result
	}

	now := s.now().UTC()
	if raw, err := readSmallFile(filepath.Join(s.outboxDir(), "agent.json")); err == nil {
		var hb agentHeartbeatFile
		if json.Unmarshal(raw, &hb) == nil {
			result.ImageRepo = strings.TrimSpace(hb.ImageRepo)
			result.CurrentTag = strings.TrimSpace(hb.CurrentTag)
			result.AgentUpdatedAt = strings.TrimSpace(hb.UpdatedAt)
			if parsed, err := time.Parse(time.RFC3339, result.AgentUpdatedAt); err == nil {
				age := now.Sub(parsed)
				if age >= -time.Minute && age <= agentHeartbeatMaxAge {
					result.AgentOnline = true
				}
			}
		}
	}

	// If a request is still sitting in inbox waiting for systemd.path to pick it up, report it as queued.
	if raw, err := readSmallFile(filepath.Join(s.inboxDir(), "request.json")); err == nil {
		var req requestFilePayload
		if json.Unmarshal(raw, &req) == nil && strings.TrimSpace(req.Tag) != "" {
			result.Operation = &Operation{
				ID:          req.ID,
				Tag:         req.Tag,
				Phase:       "queued",
				Message:     "升级请求已提交，等待宿主机代理开始执行",
				FromVersion: req.FromVersion,
				RequestedBy: req.RequestedBy,
				UpdatedAt:   req.RequestedAt,
			}
			result.InProgress = true
			return result
		}
	}

	if raw, err := readSmallFile(filepath.Join(s.outboxDir(), "status.json")); err == nil {
		var st statusFilePayload
		if json.Unmarshal(raw, &st) == nil && strings.TrimSpace(st.Phase) != "" {
			op := &Operation{
				ID:          strings.TrimSpace(st.ID),
				Tag:         strings.TrimSpace(st.Tag),
				Phase:       strings.TrimSpace(st.Phase),
				Message:     strings.TrimSpace(st.Message),
				FromVersion: strings.TrimSpace(st.FromVersion),
				RequestedBy: strings.TrimSpace(st.RequestedBy),
				UpdatedAt:   strings.TrimSpace(st.UpdatedAt),
			}
			result.Operation = op
			result.InProgress = isPhaseActive(op.Phase) && !isOperationStale(op.UpdatedAt, now)
		}
	}

	return result
}

// RequestUpgrade atomically writes a validated upgrade request into inbox/request.json.
func (s *Service) RequestUpgrade(_ context.Context, tag, requestedBy string) (Operation, error) {
	if !s.Enabled() {
		return Operation{}, ErrNotConfigured
	}
	tag = strings.TrimSpace(tag)
	if !ForkTagPattern.MatchString(tag) {
		return Operation{}, ErrInvalidTag
	}
	requestedBy = sanitizeField(requestedBy, 64)
	if requestedBy == "" {
		requestedBy = "admin"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	status := s.Status()
	if !status.AgentOnline {
		return Operation{}, ErrAgentOffline
	}
	if status.InProgress {
		return Operation{}, ErrUpgradeInProgress
	}

	id, err := randomHex(8)
	if err != nil {
		return Operation{}, fmt.Errorf("生成请求 ID 失败: %w", err)
	}
	nowStr := s.now().UTC().Format(time.RFC3339)
	req := requestFilePayload{
		ID:          id,
		Tag:         tag,
		FromVersion: s.currentVersion,
		RequestedBy: requestedBy,
		RequestedAt: nowStr,
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return Operation{}, err
	}

	inbox := s.inboxDir()
	tmpFile, err := os.CreateTemp(inbox, ".req-*.tmp")
	if err != nil {
		return Operation{}, fmt.Errorf("无法写入升级请求目录: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmpFile.Chmod(0o644); err != nil {
		_ = tmpFile.Close()
		return Operation{}, err
	}
	if _, err := tmpFile.Write(encoded); err != nil {
		_ = tmpFile.Close()
		return Operation{}, err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return Operation{}, err
	}
	if err := tmpFile.Close(); err != nil {
		return Operation{}, err
	}
	targetPath := filepath.Join(inbox, "request.json")
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return Operation{}, fmt.Errorf("提交升级请求失败: %w", err)
	}
	cleanup = false

	return Operation{
		ID:          id,
		Tag:         tag,
		Phase:       "queued",
		Message:     "升级请求已提交，等待宿主机代理开始执行",
		FromVersion: s.currentVersion,
		RequestedBy: requestedBy,
		UpdatedAt:   nowStr,
	}, nil
}

func isPhaseActive(phase string) bool {
	switch phase {
	case "queued", "backing_up", "pulling", "recreating", "health_check", "rolling_back":
		return true
	default:
		return false
	}
}

func isOperationStale(updatedAt string, now time.Time) bool {
	parsed, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return true
	}
	return now.Sub(parsed) > activeOperationMaxStale
}

func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxControlFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxControlFileSize {
		return nil, errors.New("control file too large")
	}
	return data, nil
}

func randomHex(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func sanitizeField(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if r >= 0x20 && r != 0x7f && r != '\t' && r != '\n' && r != '\r' {
			b.WriteRune(r)
		}
	}
	return truncateRunes(b.String(), maxLen)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
