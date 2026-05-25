package management

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
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
		"codex-state":        items,
		"summary":            buildCodexStateSummary(auths),
		"routing_strategy":   h.codexRoutingStrategy(),
		"current_selections": buildCodexCurrentSelections(auths),
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
	picked := coreauth.RecalculateCurrentCodexStickyAuth(h.authManager.List(), time.Now().UTC())
	updated, ok := h.authManager.GetByID(targetAuth.ID)
	if !ok {
		updated = targetAuth
	}
	response := gin.H{
		"status":                        "ok",
		"id":                            updated.ID,
		"auth_index":                    updated.EnsureIndex(),
		"name":                          codexStateName(updated),
		"codex_manual_score_adjustment": *req.Value,
	}
	if picked != nil {
		response["on_device"] = gin.H{
			"id":         picked.ID,
			"auth_index": picked.EnsureIndex(),
			"name":       codexStateName(picked),
			"email":      authEmail(picked),
			"account":    buildCodexStateAccountLabel(picked),
		}
	}
	c.JSON(http.StatusOK, response)
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
	now := time.Now().UTC()
	explanation := coreauth.BuildCodexScoreExplanation(auth, now)
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
	if message := strings.TrimSpace(auth.StatusMessage); message != "" {
		entry["status_message"] = message
	}
	if auth.LastError != nil {
		entry["last_error"] = codexStateErrorPayload(auth.LastError)
	}
	if reason := codexStateUnavailableReason(auth); reason != "" {
		entry["unavailable_reason"] = reason
	}
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	if _, account := auth.AccountInfo(); account != "" {
		entry["account"] = account
	}
	if planType := codexPlanType(auth); planType != "" {
		entry["account_type"] = planType
		entry["plan_type"] = planType
	}
	if quota, ok := auth.GetCodexQuotaState(); ok {
		entry[coreauth.CodexQuotaMetadataKey] = codexQuotaStateResponse(sanitizeCodexQuotaStateForDisplay(quota, now))
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

func codexStateErrorPayload(err *coreauth.Error) gin.H {
	if err == nil {
		return nil
	}
	payload := gin.H{
		"message":   strings.TrimSpace(err.Message),
		"retryable": err.Retryable,
	}
	if code := strings.TrimSpace(err.Code); code != "" {
		payload["code"] = code
	}
	if err.HTTPStatus != 0 {
		payload["http_status"] = err.HTTPStatus
	}
	return payload
}

func codexStateUnavailableReason(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.LastError != nil {
		detail := codexFirstNonEmptyString(auth.LastError.Code, auth.StatusMessage, auth.LastError.Message)
		if auth.LastError.HTTPStatus != 0 {
			if detail != "" {
				return fmt.Sprintf("%d %s", auth.LastError.HTTPStatus, detail)
			}
			if statusText := strings.ToLower(http.StatusText(auth.LastError.HTTPStatus)); statusText != "" {
				return fmt.Sprintf("%d %s", auth.LastError.HTTPStatus, statusText)
			}
			return fmt.Sprintf("%d", auth.LastError.HTTPStatus)
		}
		return detail
	}
	if auth.Unavailable {
		return codexFirstNonEmptyString(auth.StatusMessage, auth.Quota.Reason)
	}
	if strings.EqualFold(strings.TrimSpace(string(auth.Status)), string(coreauth.StatusError)) {
		return strings.TrimSpace(auth.StatusMessage)
	}
	return ""
}

func codexFirstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func buildCodexCurrentSelections(auths []*coreauth.Auth) []gin.H {
	stickySelections := coreauth.CurrentCodexStickySelections()
	if len(stickySelections) == 0 {
		return []gin.H{}
	}
	byID := make(map[string]*coreauth.Auth, len(auths))
	for _, auth := range auths {
		if shouldHideCodexStateAuth(auth) {
			continue
		}
		byID[auth.ID] = auth
	}
	models := make([]string, 0, len(stickySelections))
	for model := range stickySelections {
		models = append(models, model)
	}
	sort.Strings(models)
	selections := make([]gin.H, 0, len(models))
	for _, model := range models {
		authID := strings.TrimSpace(stickySelections[model])
		auth := byID[authID]
		if auth == nil {
			continue
		}
		selections = append(selections, gin.H{
			"model":      model,
			"id":         auth.ID,
			"auth_index": auth.EnsureIndex(),
			"name":       codexStateName(auth),
			"email":      authEmail(auth),
			"account":    buildCodexStateAccountLabel(auth),
		})
	}
	return selections
}

func codexPlanType(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := strings.TrimSpace(auth.Attributes["plan_type"]); v != "" {
		return v
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		if v, ok := claims["plan_type"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
	now := time.Now().UTC()
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
		quota = sanitizeCodexQuotaStateForDisplay(quota, now)
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

func sanitizeCodexQuotaStateForDisplay(quota coreauth.CodexQuotaState, now time.Time) coreauth.CodexQuotaState {
	if quota.FiveHour.ResetAt != nil && quota.FiveHour.ResetAt.After(now.Add(6*time.Hour)) {
		quota.FiveHour = coreauth.CodexQuotaBucket{}
	}
	return quota
}

func codexQuotaStateResponse(quota coreauth.CodexQuotaState) gin.H {
	payload := gin.H{}
	if bucket := codexQuotaBucketResponse(quota.FiveHour); bucket != nil {
		payload["five_hour"] = bucket
	}
	if bucket := codexQuotaBucketResponse(quota.Weekly); bucket != nil {
		payload["weekly"] = bucket
	}
	if quota.LastRefreshAt != nil && !quota.LastRefreshAt.IsZero() {
		payload["last_refresh_at"] = quota.LastRefreshAt.UTC()
	}
	if value := strings.TrimSpace(quota.RefreshStatus); value != "" {
		payload["refresh_status"] = value
	}
	if value := strings.TrimSpace(quota.RefreshError); value != "" {
		payload["refresh_error"] = value
	}
	if quota.ProbeResetAt != nil && !quota.ProbeResetAt.IsZero() {
		payload["probe_reset_at"] = quota.ProbeResetAt.UTC()
	}
	if quota.ProbeAt != nil && !quota.ProbeAt.IsZero() {
		payload["probe_at"] = quota.ProbeAt.UTC()
	}
	if quota.ProbeVerifiedAt != nil && !quota.ProbeVerifiedAt.IsZero() {
		payload["probe_verified_at"] = quota.ProbeVerifiedAt.UTC()
	}
	if value := strings.TrimSpace(quota.ProbeStatus); value != "" {
		payload["probe_status"] = value
	}
	if value := strings.TrimSpace(quota.ProbeError); value != "" {
		payload["probe_error"] = value
	}
	if quota.BootstrapProbeAt != nil && !quota.BootstrapProbeAt.IsZero() {
		payload["bootstrap_probe_at"] = quota.BootstrapProbeAt.UTC()
	}
	if quota.BootstrapVerifiedAt != nil && !quota.BootstrapVerifiedAt.IsZero() {
		payload["bootstrap_verified_at"] = quota.BootstrapVerifiedAt.UTC()
	}
	if quota.BootstrapNextAfter != nil && !quota.BootstrapNextAfter.IsZero() {
		payload["bootstrap_next_after"] = quota.BootstrapNextAfter.UTC()
	}
	if value := strings.TrimSpace(quota.BootstrapStatus); value != "" {
		payload["bootstrap_status"] = value
	}
	if value := strings.TrimSpace(quota.BootstrapError); value != "" {
		payload["bootstrap_error"] = value
	}
	if value := strings.TrimSpace(quota.BootstrapReason); value != "" {
		payload["bootstrap_reason"] = value
	}
	if quota.BootstrapAttempts > 0 {
		payload["bootstrap_attempts"] = quota.BootstrapAttempts
	}
	if len(payload) == 0 {
		return nil
	}
	return payload
}

func codexQuotaBucketResponse(bucket coreauth.CodexQuotaBucket) gin.H {
	payload := gin.H{}
	if bucket.Remaining != nil {
		payload["remaining"] = *bucket.Remaining
	}
	if bucket.Limit != nil {
		payload["limit"] = *bucket.Limit
	}
	if bucket.ResetAt != nil && !bucket.ResetAt.IsZero() {
		payload["reset_at"] = bucket.ResetAt.UTC()
	}
	if len(payload) == 0 {
		return nil
	}
	return payload
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
