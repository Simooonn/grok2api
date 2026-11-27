package system

import (
	"errors"
	"net/http"
	"strings"

	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	upgradeapp "github.com/chenyme/grok2api/backend/internal/application/upgrade"
	"github.com/chenyme/grok2api/backend/internal/domain/admin"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	publicAPIBaseURL func() string
	updates          *updatecheckapp.Service
	upgrades         *upgradeapp.Service
}

func NewHandler(publicAPIBaseURL func() string, updates *updatecheckapp.Service) *Handler {
	if publicAPIBaseURL == nil {
		publicAPIBaseURL = func() string { return "" }
	}
	if updates == nil {
		updates = updatecheckapp.NewService("dev", nil)
	}
	return &Handler{
		publicAPIBaseURL: publicAPIBaseURL,
		updates:          updates,
		upgrades:         upgradeapp.NewService(upgradeapp.Config{}, nil),
	}
}

// WithUpgrades wires the host-agent control directory service for in-app upgrades.
func (h *Handler) WithUpgrades(upgrades *upgradeapp.Service) *Handler {
	if upgrades != nil {
		h.upgrades = upgrades
	}
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/system", h.get)
	router.GET("/system/version", h.version)
	router.POST("/system/update/check", h.checkUpdate)
	router.GET("/system/releases", h.releases)
	router.GET("/system/upgrade/status", h.upgradeStatus)
	router.POST("/system/upgrade", h.upgrade)
}

func (h *Handler) version(c *gin.Context) {
	response.Success(c, http.StatusOK, h.updates.Snapshot())
}

func (h *Handler) checkUpdate(c *gin.Context) {
	response.Success(c, http.StatusOK, h.updates.Check(c.Request.Context()))
}

func (h *Handler) releases(c *gin.Context) {
	force := c.Query("force") == "1" || strings.EqualFold(c.Query("force"), "true")
	list, err := h.upgrades.ListReleases(c.Request.Context(), force)
	if err != nil {
		response.Error(c, http.StatusBadGateway, "release_list_failed", err.Error())
		return
	}
	response.Success(c, http.StatusOK, gin.H{"releases": list})
}

func (h *Handler) upgradeStatus(c *gin.Context) {
	response.Success(c, http.StatusOK, h.upgrades.Status())
}

func (h *Handler) upgrade(c *gin.Context) {
	var body struct {
		Tag string `json:"tag"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Tag) == "" {
		response.Error(c, http.StatusBadRequest, "invalid_tag", "缺少要升级的版本号")
		return
	}
	requestedBy := "admin"
	if value, ok := c.Get(middleware.AdminKey); ok {
		if currentAdmin, ok := value.(admin.Admin); ok && strings.TrimSpace(currentAdmin.Username) != "" {
			requestedBy = currentAdmin.Username
		}
	}
	op, err := h.upgrades.RequestUpgrade(c.Request.Context(), strings.TrimSpace(body.Tag), requestedBy)
	if err != nil {
		switch {
		case errors.Is(err, upgradeapp.ErrInvalidTag):
			response.Error(c, http.StatusBadRequest, "invalid_tag", err.Error())
		case errors.Is(err, upgradeapp.ErrNotConfigured), errors.Is(err, upgradeapp.ErrAgentOffline):
			response.Error(c, http.StatusServiceUnavailable, "upgrade_agent_unavailable", err.Error())
		case errors.Is(err, upgradeapp.ErrUpgradeInProgress):
			response.Error(c, http.StatusConflict, "upgrade_in_progress", err.Error())
		default:
			response.Error(c, http.StatusInternalServerError, "upgrade_failed", err.Error())
		}
		return
	}
	response.Success(c, http.StatusAccepted, op)
}

func (h *Handler) get(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"publicApiBaseURL": strings.TrimRight(strings.TrimSpace(h.publicAPIBaseURL()), "/")})
}
