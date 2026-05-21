package auth

import (
	"fmt"
	"strings"
	"time"
)

const CodexScoreExplanationMetadataKey = "codex_score_explanation"

const codexScoreFormulaLabel = "weekly_remaining / max(hours_until_weekly_reset, 1) + expiry_urgency_bonus + manual_adjustment"
const codexScoreExpiryUrgencyWindowHours = 24.0

type CodexScoreExplanation struct {
	ScoreAvailable        bool     `json:"score_available"`
	ComputedScoreLive     *float64 `json:"computed_score_live,omitempty"`
	WeeklyRemaining       *float64 `json:"weekly_remaining,omitempty"`
	WeeklyLimit           *float64 `json:"weekly_limit,omitempty"`
	HoursUntilWeeklyReset *float64 `json:"hours_until_weekly_reset,omitempty"`
	ExpiryUrgencyBonus    *float64 `json:"expiry_urgency_bonus,omitempty"`
	ManualAdjustment      float64  `json:"manual_adjustment"`
	RefreshIsFresh        bool     `json:"refresh_is_fresh"`
	RefreshStatus         string   `json:"refresh_status,omitempty"`
	DisqualifierReason    string   `json:"disqualifier_reason,omitempty"`
	Formula               string   `json:"formula,omitempty"`
	FormulaLabel          string   `json:"formula_label,omitempty"`
}

func BuildCodexScoreExplanation(auth *Auth, now time.Time) CodexScoreExplanation {
	explanation := CodexScoreExplanation{}
	if auth == nil {
		explanation.DisqualifierReason = "missing_auth"
		return explanation
	}
	manualAdjustment, _ := auth.CodexManualScoreAdjustment()
	explanation.ManualAdjustment = manualAdjustment
	if !IsCodexOAuthLikeAuth(auth) {
		explanation.DisqualifierReason = "non_codex_oauth_like_auth"
		return explanation
	}
	if blocked, reason, _ := isAuthBlockedForModel(auth, "", now); blocked {
		explanation.DisqualifierReason = codexEligibilityDisqualifierReason(auth, reason)
		return explanation
	}
	quota, ok := auth.GetCodexQuotaState()
	if !ok {
		explanation.DisqualifierReason = "missing_quota_state"
		return explanation
	}
	if quota.Weekly.Remaining != nil {
		explanation.WeeklyRemaining = float64Ptr(*quota.Weekly.Remaining)
	}
	if quota.Weekly.Limit != nil {
		explanation.WeeklyLimit = float64Ptr(*quota.Weekly.Limit)
	}
	explanation.RefreshStatus = strings.TrimSpace(quota.RefreshStatus)
	explanation.RefreshIsFresh = codexQuotaRefreshStateUsable(quota, now)

	if quota.Weekly.Remaining == nil {
		explanation.DisqualifierReason = "missing_weekly_remaining"
		return explanation
	}
	if quota.Weekly.ResetAt == nil || quota.Weekly.ResetAt.IsZero() {
		explanation.DisqualifierReason = "missing_weekly_reset"
		return explanation
	}
	if !quota.Weekly.ResetAt.After(now) {
		explanation.DisqualifierReason = "weekly_reset_elapsed"
		return explanation
	}
	hoursUntilReset := quota.Weekly.ResetAt.Sub(now).Hours()
	explanation.HoursUntilWeeklyReset = float64Ptr(hoursUntilReset)
	if !explanation.RefreshIsFresh {
		explanation.DisqualifierReason = codexRefreshDisqualifierReason(quota, now)
		return explanation
	}
	if hoursUntilReset < 1 {
		hoursUntilReset = 1
	}
	expiryUrgencyBonus := codexExpiryUrgencyBonus(hoursUntilReset)
	explanation.ExpiryUrgencyBonus = float64Ptr(expiryUrgencyBonus)
	computedScoreLive := (*quota.Weekly.Remaining / hoursUntilReset) + expiryUrgencyBonus + manualAdjustment
	explanation.ScoreAvailable = true
	explanation.ComputedScoreLive = float64Ptr(computedScoreLive)
	explanation.Formula = fmt.Sprintf("final_score = %s", codexScoreFormulaLabel)
	explanation.FormulaLabel = codexScoreFormulaLabel
	return explanation
}

func codexExpiryUrgencyBonus(hoursUntilReset float64) float64 {
	if hoursUntilReset <= 0 {
		return 1
	}
	if hoursUntilReset >= codexScoreExpiryUrgencyWindowHours {
		return 0
	}
	return (codexScoreExpiryUrgencyWindowHours - hoursUntilReset) / codexScoreExpiryUrgencyWindowHours
}

func codexRefreshDisqualifierReason(quota CodexQuotaState, now time.Time) string {
	if quota.LastRefreshAt == nil || quota.LastRefreshAt.IsZero() {
		return "missing_last_refresh_at"
	}
	if quota.LastRefreshAt.Before(now.Add(-codexQuotaScoreFreshnessWindow)) {
		return "stale_refresh"
	}
	status := strings.ToLower(strings.TrimSpace(quota.RefreshStatus))
	if status == "" {
		return "refresh_status_unknown"
	}
	return "refresh_status_" + status
}

func codexEligibilityDisqualifierReason(auth *Auth, reason blockReason) string {
	if auth == nil {
		return "missing_auth"
	}
	switch reason {
	case blockReasonDisabled:
		if auth.Disabled || auth.Status == StatusDisabled {
			return "auth_disabled"
		}
		return "auth_ineligible_disabled"
	case blockReasonCooldown:
		return "auth_cooldown"
	case blockReasonOther:
		if auth.Unavailable {
			return "auth_unavailable"
		}
		return "auth_ineligible"
	default:
		return "auth_ineligible"
	}
}
