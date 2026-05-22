package auth

import (
	"math"
	"testing"
	"time"
)

func TestBuildCodexScoreExplanation_ScoreAvailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(4 * time.Hour)
	lastRefresh := now.Add(-5 * time.Minute)
	auth := &Auth{Provider: "codex", Metadata: map[string]any{"email": "oauth@example.com"}}
	auth.SetCodexQuotaState(CodexQuotaState{
		Weekly: CodexQuotaBucket{
			Remaining: float64Ptr(24),
			Limit:     float64Ptr(100),
			ResetAt:   &weeklyReset,
		},
		LastRefreshAt: &lastRefresh,
		RefreshStatus: "ok",
	})
	auth.SetCodexManualScoreAdjustment(1.5)

	explanation := BuildCodexScoreExplanation(auth, now)
	if !explanation.ScoreAvailable {
		t.Fatalf("ScoreAvailable = false, want true: %#v", explanation)
	}
	if !explanation.RefreshIsFresh {
		t.Fatal("RefreshIsFresh = false, want true")
	}
	if explanation.ComputedScoreLive == nil {
		t.Fatal("ComputedScoreLive = nil, want value")
	}
	if explanation.ExpiryUrgencyBonus == nil || *explanation.ExpiryUrgencyBonus <= 0 {
		t.Fatalf("ExpiryUrgencyBonus = %#v, want positive bonus for near weekly reset", explanation.ExpiryUrgencyBonus)
	}
	wantScore := 7.5 + *explanation.ExpiryUrgencyBonus
	if math.Abs(*explanation.ComputedScoreLive-wantScore) > 0.0001 {
		t.Fatalf("ComputedScoreLive = %v, want %v", *explanation.ComputedScoreLive, wantScore)
	}
	if explanation.FormulaLabel != codexScoreFormulaLabel {
		t.Fatalf("FormulaLabel = %q, want %q", explanation.FormulaLabel, codexScoreFormulaLabel)
	}
	if explanation.DisqualifierReason != "" {
		t.Fatalf("DisqualifierReason = %q, want empty", explanation.DisqualifierReason)
	}
}

func TestBuildCodexScoreExplanation_FallsBackToFiveHourWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(2 * time.Hour)
	lastRefresh := now.Add(-5 * time.Minute)
	auth := &Auth{Provider: "codex", Metadata: map[string]any{"email": "oauth@example.com"}}
	auth.SetCodexQuotaState(CodexQuotaState{
		FiveHour: CodexQuotaBucket{
			Remaining: float64Ptr(50),
			Limit:     float64Ptr(100),
			ResetAt:   &fiveHourReset,
		},
		LastRefreshAt: &lastRefresh,
		RefreshStatus: "ok",
	})

	explanation := BuildCodexScoreExplanation(auth, now)
	if !explanation.ScoreAvailable {
		t.Fatalf("ScoreAvailable = false, want true for five-hour-only quota: %#v", explanation)
	}
	if explanation.ComputedScoreLive == nil {
		t.Fatal("ComputedScoreLive = nil, want value")
	}
	wantScore := 25 + *explanation.ExpiryUrgencyBonus
	if math.Abs(*explanation.ComputedScoreLive-wantScore) > 0.0001 {
		t.Fatalf("ComputedScoreLive = %v, want %v", *explanation.ComputedScoreLive, wantScore)
	}
	if explanation.DisqualifierReason != "" {
		t.Fatalf("DisqualifierReason = %q, want empty", explanation.DisqualifierReason)
	}
}

func TestBuildCodexScoreExplanation_FallsBackToFiveHourWhenWeeklyResetMissing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	fiveHourReset := now.Add(2 * time.Hour)
	lastRefresh := now.Add(-5 * time.Minute)
	auth := &Auth{Provider: "codex", Metadata: map[string]any{"email": "oauth@example.com"}}
	auth.SetCodexQuotaState(CodexQuotaState{
		FiveHour: CodexQuotaBucket{
			Remaining: float64Ptr(50),
			Limit:     float64Ptr(100),
			ResetAt:   &fiveHourReset,
		},
		Weekly: CodexQuotaBucket{
			Remaining: float64Ptr(100),
			Limit:     float64Ptr(100),
		},
		LastRefreshAt: &lastRefresh,
		RefreshStatus: "ok",
	})
	auth.SetCodexManualScoreAdjustment(100)

	explanation := BuildCodexScoreExplanation(auth, now)
	if !explanation.ScoreAvailable {
		t.Fatalf("ScoreAvailable = false, want true with five-hour fallback: %#v", explanation)
	}
	if explanation.DisqualifierReason != "" {
		t.Fatalf("DisqualifierReason = %q, want empty", explanation.DisqualifierReason)
	}
	if explanation.ComputedScoreLive == nil {
		t.Fatal("ComputedScoreLive = nil, want value")
	}
	wantScore := 125 + *explanation.ExpiryUrgencyBonus
	if math.Abs(*explanation.ComputedScoreLive-wantScore) > 0.0001 {
		t.Fatalf("ComputedScoreLive = %v, want %v", *explanation.ComputedScoreLive, wantScore)
	}
}

func TestBuildCodexScoreExplanation_ExplainsUnavailableScore(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(4 * time.Hour)
	staleRefresh := now.Add(-20 * time.Minute)
	auth := &Auth{Provider: "codex", Metadata: map[string]any{"email": "oauth@example.com"}}
	auth.SetCodexQuotaState(CodexQuotaState{
		Weekly: CodexQuotaBucket{
			Remaining: float64Ptr(24),
			Limit:     float64Ptr(100),
			ResetAt:   &weeklyReset,
		},
		LastRefreshAt: &staleRefresh,
		RefreshStatus: "ok",
	})

	explanation := BuildCodexScoreExplanation(auth, now)
	if explanation.ScoreAvailable {
		t.Fatalf("ScoreAvailable = true, want false: %#v", explanation)
	}
	if explanation.RefreshIsFresh {
		t.Fatal("RefreshIsFresh = true, want false")
	}
	if explanation.DisqualifierReason != "stale_refresh" {
		t.Fatalf("DisqualifierReason = %q, want stale_refresh", explanation.DisqualifierReason)
	}
	if explanation.HoursUntilWeeklyReset == nil || math.Abs(*explanation.HoursUntilWeeklyReset-4) > 0.0001 {
		t.Fatalf("HoursUntilWeeklyReset = %#v, want about 4", explanation.HoursUntilWeeklyReset)
	}
}

func TestBuildCodexScoreExplanation_DisabledAuthIsHardIneligible(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(4 * time.Hour)
	lastRefresh := now.Add(-5 * time.Minute)
	auth := &Auth{
		Provider: "codex",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{"email": "oauth@example.com"},
	}
	auth.SetCodexQuotaState(CodexQuotaState{
		Weekly: CodexQuotaBucket{
			Remaining: float64Ptr(24),
			Limit:     float64Ptr(100),
			ResetAt:   &weeklyReset,
		},
		LastRefreshAt: &lastRefresh,
		RefreshStatus: "ok",
	})

	explanation := BuildCodexScoreExplanation(auth, now)
	if explanation.ScoreAvailable {
		t.Fatalf("ScoreAvailable = true, want false: %#v", explanation)
	}
	if explanation.DisqualifierReason != "auth_disabled" {
		t.Fatalf("DisqualifierReason = %q, want auth_disabled", explanation.DisqualifierReason)
	}
	if explanation.ComputedScoreLive != nil {
		t.Fatalf("ComputedScoreLive = %#v, want nil", explanation.ComputedScoreLive)
	}
}

func TestBuildCodexScoreExplanation_CooldownAuthIsHardIneligible(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(4 * time.Hour)
	lastRefresh := now.Add(-5 * time.Minute)
	auth := &Auth{
		Provider:       "codex",
		Unavailable:    true,
		NextRetryAfter: now.Add(30 * time.Minute),
		Metadata:       map[string]any{"email": "oauth@example.com"},
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: now.Add(30 * time.Minute),
		},
	}
	auth.SetCodexQuotaState(CodexQuotaState{
		Weekly: CodexQuotaBucket{
			Remaining: float64Ptr(24),
			Limit:     float64Ptr(100),
			ResetAt:   &weeklyReset,
		},
		LastRefreshAt: &lastRefresh,
		RefreshStatus: "ok",
	})

	explanation := BuildCodexScoreExplanation(auth, now)
	if explanation.ScoreAvailable {
		t.Fatalf("ScoreAvailable = true, want false: %#v", explanation)
	}
	if explanation.DisqualifierReason != "auth_cooldown" {
		t.Fatalf("DisqualifierReason = %q, want auth_cooldown", explanation.DisqualifierReason)
	}
}
