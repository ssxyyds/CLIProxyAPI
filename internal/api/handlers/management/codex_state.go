package management

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	codexManualScoreMin = -100.0
	codexManualScoreMax = 100.0
)

type codexStateRefreshRequest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AuthIndex   string   `json:"auth_index"`
	AuthIndexes []string `json:"auth_indexes"`
	All         bool     `json:"all"`
}

func (h *Handler) PostCodexStateRecalc(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	auths := h.authManager.List()
	picked := coreauth.RecalculateCurrentCodexStickyAuth(auths, time.Now().UTC())
	if picked == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "on_device": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"on_device": gin.H{
			"id":         picked.ID,
			"auth_index": picked.EnsureIndex(),
			"name":       codexStateName(picked),
			"email":      authEmail(picked),
			"account":    buildCodexStateAccountLabel(picked),
		},
	})
}

func (h *Handler) GetCodexState(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	auths := h.authManager.List()
	items := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if entry := buildCodexStateEntry(auth); entry != nil {
			items = append(items, entry)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"codex-state":      items,
		"summary":          buildCodexStateSummary(auths),
		"routing_strategy": h.codexRoutingStrategy(),
	})
}

func (h *Handler) codexRoutingStrategy() string {
	if h == nil || h.cfg == nil {
		return "round-robin"
	}
	if strategy := strings.TrimSpace(h.cfg.Routing.Strategy); strategy != "" {
		return strategy
	}
	return "round-robin"
}

func (h *Handler) PatchCodexStateManualScore(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		AuthIndex string   `json:"auth_index"`
		Value     *float64 `json:"value"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if math.IsNaN(*req.Value) || math.IsInf(*req.Value, 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "value must be a finite number"})
		return
	}
	if *req.Value < codexManualScoreMin || *req.Value > codexManualScoreMax {
		c.JSON(http.StatusBadRequest, gin.H{"error": "value must be between -100 and 100"})
		return
	}
	targetAuth, err := h.findManagedCodexAuth(req.ID, req.Name, req.AuthIndex)
	if err != nil {
		status := http.StatusBadRequest
		switch err.Error() {
		case "auth not found":
			status = http.StatusNotFound
		case "core auth manager unavailable":
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	targetAuth.SetCodexManualScoreAdjustment(*req.Value)
	targetAuth.UpdatedAt = time.Now()
	if _, err := h.authManager.Update(c.Request.Context(), targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}
	updated, ok := h.authManager.GetByID(targetAuth.ID)
	if !ok {
		updated = targetAuth
	}
	c.JSON(http.StatusOK, gin.H{
		"status":                        "ok",
		"id":                            updated.ID,
		"auth_index":                    updated.EnsureIndex(),
		"name":                          codexStateName(updated),
		"codex_manual_score_adjustment": *req.Value,
	})
}

func (h *Handler) PostCodexStateRefresh(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req codexStateRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	ctx := c.Request.Context()
	if req.All {
		refreshed := make([]gin.H, 0)
		for _, auth := range h.authManager.List() {
			if !coreauth.IsCodexOAuthLikeAuth(auth) {
				continue
			}
			if err := h.authManager.RefreshNow(ctx, auth.ID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to refresh auth %s: %v", auth.ID, err)})
				return
			}
			refreshed = append(refreshed, gin.H{"id": auth.ID, "auth_index": auth.EnsureIndex(), "name": codexStateName(auth)})
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "refreshed": refreshed})
		return
	}
	if len(req.AuthIndexes) > 0 {
		refreshed := make([]gin.H, 0, len(req.AuthIndexes))
		for _, authIndex := range req.AuthIndexes {
			targetAuth, err := h.findManagedCodexAuth("", "", authIndex)
			if err != nil {
				status := http.StatusBadRequest
				switch err.Error() {
				case "auth not found":
					status = http.StatusNotFound
				case "core auth manager unavailable":
					status = http.StatusServiceUnavailable
				}
				c.JSON(status, gin.H{"error": err.Error(), "auth_index": authIndex})
				return
			}
			if err := h.authManager.RefreshNow(ctx, targetAuth.ID); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to refresh auth %s: %v", targetAuth.ID, err)})
				return
			}
			refreshed = append(refreshed, gin.H{"id": targetAuth.ID, "auth_index": targetAuth.EnsureIndex(), "name": codexStateName(targetAuth)})
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "refreshed": refreshed})
		return
	}
	targetAuth, err := h.findManagedCodexAuth(req.ID, req.Name, req.AuthIndex)
	if err != nil {
		status := http.StatusBadRequest
		switch err.Error() {
		case "auth not found":
			status = http.StatusNotFound
		case "core auth manager unavailable":
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	if err := h.authManager.RefreshNow(ctx, targetAuth.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to refresh auth: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"refreshed": gin.H{"id": targetAuth.ID, "auth_index": targetAuth.EnsureIndex(), "name": codexStateName(targetAuth)},
	})
}

func (h *Handler) findManagedCodexAuth(id, name, authIndex string) (*coreauth.Auth, error) {
	if h == nil || h.authManager == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	authIndex = strings.TrimSpace(authIndex)
	if id == "" && name == "" && authIndex == "" {
		return nil, fmt.Errorf("id, name, or auth_index is required")
	}
	if id != "" {
		if auth, ok := h.authManager.GetByID(id); ok {
			if !coreauth.IsCodexOAuthLikeAuth(auth) {
				return nil, fmt.Errorf("auth not found")
			}
			return auth, nil
		}
	}
	if authIndex != "" {
		if auth := h.authByIndex(authIndex); auth != nil {
			if !coreauth.IsCodexOAuthLikeAuth(auth) {
				return nil, fmt.Errorf("auth not found")
			}
			return auth, nil
		}
	}
	if name != "" {
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			if auth.ID == name || auth.FileName == name {
				if !coreauth.IsCodexOAuthLikeAuth(auth) {
					return nil, fmt.Errorf("auth not found")
				}
				return auth, nil
			}
		}
	}
	return nil, fmt.Errorf("auth not found")
}

func buildCodexStateEntry(auth *coreauth.Auth) gin.H {
	if shouldHideCodexStateAuth(auth) {
		return nil
	}
	auth.EnsureIndex()
	explanation := coreauth.BuildCodexScoreExplanation(auth, time.Now().UTC())
	stickyAuthID := coreauth.CurrentCodexStickyAuthID()
	entry := gin.H{
		"id":          auth.ID,
		"auth_index":  auth.Index,
		"name":        codexStateName(auth),
		"provider":    strings.TrimSpace(auth.Provider),
		"status":      auth.Status,
		"disabled":    auth.Disabled,
		"unavailable": auth.Unavailable,
		"on_device":   strings.TrimSpace(stickyAuthID) != "" && auth.ID == stickyAuthID,
		coreauth.CodexScoreExplanationMetadataKey: explanation,
	}
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	if accountType, account := auth.AccountInfo(); accountType != "" || account != "" {
		if accountType != "" {
			entry["account_type"] = accountType
		}
		if account != "" {
			entry["account"] = account
		}
	}
	if quota, ok := auth.GetCodexQuotaState(); ok {
		entry[coreauth.CodexQuotaMetadataKey] = quota
	}
	if manual, ok := auth.CodexManualScoreAdjustment(); ok {
		entry[coreauth.CodexManualScoreAdjustmentKey] = manual
	}
	if computed, ok := auth.CodexComputedScore(); ok {
		entry[coreauth.CodexComputedScoreMetadataKey] = computed
	}
	if reason := auth.CodexScoreReason(); reason != "" {
		entry[coreauth.CodexScoreReasonMetadataKey] = reason
	}
	if reason := auth.CodexLastSelectionReason(); reason != "" {
		entry[coreauth.CodexLastSelectionReasonMetadataKey] = reason
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		entry["id_token"] = claims
	}
	return entry
}

type codexQuotaPoolWindowSummary struct {
	Known          int      `json:"known"`
	Limit          *float64 `json:"limit,omitempty"`
	Remaining      *float64 `json:"remaining,omitempty"`
	RemainingRatio *float64 `json:"remaining_ratio,omitempty"`
}

type codexStatePoolSummary struct {
	Accounts      gin.H                       `json:"accounts"`
	Weekly        codexQuotaPoolWindowSummary `json:"weekly"`
	FiveHour      codexQuotaPoolWindowSummary `json:"five_hour"`
	LastRefreshAt *time.Time                  `json:"last_refresh_at,omitempty"`
}

func buildCodexStateSummary(auths []*coreauth.Auth) codexStatePoolSummary {
	summary := codexStatePoolSummary{
		Accounts: gin.H{
			"total":       0,
			"active":      0,
			"cooldown":    0,
			"unavailable": 0,
			"disabled":    0,
		},
	}
	for _, auth := range auths {
		if shouldHideCodexStateAuth(auth) {
			continue
		}
		summary.Accounts["total"] = summary.Accounts["total"].(int) + 1
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			summary.Accounts["disabled"] = summary.Accounts["disabled"].(int) + 1
		} else if auth.Unavailable {
			summary.Accounts["unavailable"] = summary.Accounts["unavailable"].(int) + 1
		} else {
			summary.Accounts["active"] = summary.Accounts["active"].(int) + 1
		}
		if auth.Quota.Exceeded || strings.EqualFold(strings.TrimSpace(auth.Quota.Reason), "quota") || !auth.Quota.NextRecoverAt.IsZero() {
			summary.Accounts["cooldown"] = summary.Accounts["cooldown"].(int) + 1
		}
		quota, ok := auth.GetCodexQuotaState()
		if !ok {
			continue
		}
		addCodexQuotaBucketToSummary(&summary.Weekly, quota.Weekly)
		addCodexQuotaBucketToSummary(&summary.FiveHour, quota.FiveHour)
		if quota.LastRefreshAt != nil && !quota.LastRefreshAt.IsZero() {
			refreshed := quota.LastRefreshAt.UTC()
			if summary.LastRefreshAt == nil || refreshed.After(*summary.LastRefreshAt) {
				summary.LastRefreshAt = &refreshed
			}
		}
	}
	finalizeCodexQuotaPoolWindowSummary(&summary.Weekly)
	finalizeCodexQuotaPoolWindowSummary(&summary.FiveHour)
	return summary
}

func addCodexQuotaBucketToSummary(summary *codexQuotaPoolWindowSummary, bucket coreauth.CodexQuotaBucket) {
	if summary == nil {
		return
	}
	hasData := false
	if bucket.Limit != nil {
		addFloatPtr(&summary.Limit, *bucket.Limit)
		hasData = true
	}
	if bucket.Remaining != nil {
		addFloatPtr(&summary.Remaining, *bucket.Remaining)
		hasData = true
	}
	if hasData {
		summary.Known++
	}
}

func finalizeCodexQuotaPoolWindowSummary(summary *codexQuotaPoolWindowSummary) {
	if summary == nil || summary.Limit == nil || summary.Remaining == nil || *summary.Limit <= 0 {
		return
	}
	ratio := *summary.Remaining / *summary.Limit
	summary.RemainingRatio = &ratio
}

func addFloatPtr(target **float64, value float64) {
	if target == nil {
		return
	}
	if *target == nil {
		v := value
		*target = &v
		return
	}
	**target += value
}

func shouldHideCodexStateAuth(auth *coreauth.Auth) bool {
	if auth == nil || !coreauth.IsCodexOAuthLikeAuth(auth) {
		return true
	}
	if isRuntimeOnlyAuth(auth) {
		return false
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return true
	}
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); os.IsNotExist(err) && strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api") {
		return true
	}
	return false
}

func buildCodexStateAccountLabel(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	_, account := auth.AccountInfo()
	return strings.TrimSpace(account)
}

func codexStateName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = strings.TrimSpace(auth.ID)
	}
	return name
}
