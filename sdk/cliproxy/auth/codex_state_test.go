package auth

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestCodexStateRoundTrip(t *testing.T) {
	t.Parallel()

	fiveHourReset := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	weeklyReset := fiveHourReset.Add(7 * 24 * time.Hour)
	lastRefresh := fiveHourReset.Add(-15 * time.Minute)
	probeAt := fiveHourReset.Add(-2 * time.Minute)
	probeVerifiedAt := fiveHourReset.Add(-1 * time.Minute)
	bootstrapProbeAt := fiveHourReset.Add(-3 * time.Minute)
	bootstrapVerifiedAt := fiveHourReset.Add(-2 * time.Minute)
	bootstrapNextAfter := fiveHourReset.Add(12 * time.Minute)

	a := &Auth{}
	a.SetCodexQuotaState(CodexQuotaState{
		FiveHour: CodexQuotaBucket{
			Remaining: float64Ptr(12),
			Limit:     float64Ptr(40),
			ResetAt:   &fiveHourReset,
		},
		Weekly: CodexQuotaBucket{
			Remaining: float64Ptr(90),
			Limit:     float64Ptr(120),
			ResetAt:   &weeklyReset,
		},
		LastRefreshAt:       &lastRefresh,
		RefreshStatus:       "ok",
		RefreshError:        "",
		ProbeResetAt:        &fiveHourReset,
		ProbeAt:             &probeAt,
		ProbeVerifiedAt:     &probeVerifiedAt,
		ProbeStatus:         "verified",
		BootstrapStatus:     "pending",
		BootstrapProbeAt:    &bootstrapProbeAt,
		BootstrapVerifiedAt: &bootstrapVerifiedAt,
		BootstrapNextAfter:  &bootstrapNextAfter,
		BootstrapAttempts:   2,
		BootstrapReason:     "weekly_missing",
	})
	a.SetCodexManualScoreAdjustment(1.25)
	a.SetCodexComputedScore(7.5)
	a.SetCodexScoreReason("weekly headroom")
	a.SetCodexLastSelectionReason("highest final score")

	quota, ok := a.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	assertFloatPtr(t, quota.FiveHour.Remaining, 12)
	assertFloatPtr(t, quota.FiveHour.Limit, 40)
	assertTimePtr(t, quota.FiveHour.ResetAt, fiveHourReset)
	assertFloatPtr(t, quota.Weekly.Remaining, 90)
	assertFloatPtr(t, quota.Weekly.Limit, 120)
	assertTimePtr(t, quota.Weekly.ResetAt, weeklyReset)
	assertTimePtr(t, quota.LastRefreshAt, lastRefresh)
	assertTimePtr(t, quota.ProbeResetAt, fiveHourReset)
	assertTimePtr(t, quota.ProbeAt, probeAt)
	assertTimePtr(t, quota.ProbeVerifiedAt, probeVerifiedAt)
	if quota.RefreshStatus != "ok" {
		t.Fatalf("RefreshStatus = %q, want %q", quota.RefreshStatus, "ok")
	}
	if quota.RefreshError != "" {
		t.Fatalf("RefreshError = %q, want empty", quota.RefreshError)
	}
	if quota.ProbeStatus != "verified" {
		t.Fatalf("ProbeStatus = %q, want verified", quota.ProbeStatus)
	}
	if quota.BootstrapStatus != "pending" {
		t.Fatalf("BootstrapStatus = %q, want pending", quota.BootstrapStatus)
	}
	assertTimePtr(t, quota.BootstrapProbeAt, bootstrapProbeAt)
	assertTimePtr(t, quota.BootstrapVerifiedAt, bootstrapVerifiedAt)
	assertTimePtr(t, quota.BootstrapNextAfter, bootstrapNextAfter)
	if quota.BootstrapAttempts != 2 {
		t.Fatalf("BootstrapAttempts = %d, want 2", quota.BootstrapAttempts)
	}
	if quota.BootstrapReason != "weekly_missing" {
		t.Fatalf("BootstrapReason = %q, want weekly_missing", quota.BootstrapReason)
	}

	manual, ok := a.CodexManualScoreAdjustment()
	if !ok || manual != 1.25 {
		t.Fatalf("CodexManualScoreAdjustment() = %v, %v; want 1.25, true", manual, ok)
	}
	computed, ok := a.CodexComputedScore()
	if !ok || computed != 7.5 {
		t.Fatalf("CodexComputedScore() = %v, %v; want 7.5, true", computed, ok)
	}
	if got := a.CodexScoreReason(); got != "weekly headroom" {
		t.Fatalf("CodexScoreReason() = %q", got)
	}
	if got := a.CodexLastSelectionReason(); got != "highest final score" {
		t.Fatalf("CodexLastSelectionReason() = %q", got)
	}

	rawQuota, ok := a.Metadata[CodexQuotaMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("metadata[%q] type = %T, want map[string]any", CodexQuotaMetadataKey, a.Metadata[CodexQuotaMetadataKey])
	}
	if _, ok := rawQuota["five_hour"].(map[string]any); !ok {
		t.Fatalf("metadata five_hour type = %T, want map[string]any", rawQuota["five_hour"])
	}
}

func TestCodexStateReadsMalformedMetadataSafely(t *testing.T) {
	t.Parallel()

	a := &Auth{Metadata: map[string]any{
		CodexQuotaMetadataKey: map[string]any{
			"five_hour": map[string]any{
				"remaining": "nope",
				"limit":     json.Number("9"),
				"reset_at":  "bad-time",
			},
			"weekly":          "wrong-type",
			"last_refresh_at": 1710000000,
			"refresh_status":  true,
			"refresh_error":   " timeout ",
		},
		CodexManualScoreAdjustmentKey:       "abc",
		CodexComputedScoreMetadataKey:       json.Number("3.75"),
		CodexScoreReasonMetadataKey:         55,
		CodexLastSelectionReasonMetadataKey: " selected after fallback ",
	}}

	quota, ok := a.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.FiveHour.Remaining != nil {
		t.Fatalf("FiveHour.Remaining = %v, want nil", *quota.FiveHour.Remaining)
	}
	assertFloatPtr(t, quota.FiveHour.Limit, 9)
	if quota.FiveHour.ResetAt != nil {
		t.Fatalf("FiveHour.ResetAt = %v, want nil", quota.FiveHour.ResetAt)
	}
	if quota.Weekly.hasData() {
		t.Fatal("Weekly should be empty for malformed input")
	}
	if quota.LastRefreshAt == nil || quota.LastRefreshAt.IsZero() {
		t.Fatal("LastRefreshAt should parse unix timestamp")
	}
	if quota.RefreshStatus != "" {
		t.Fatalf("RefreshStatus = %q, want empty for non-string input", quota.RefreshStatus)
	}
	if quota.RefreshError != "timeout" {
		t.Fatalf("RefreshError = %q, want %q", quota.RefreshError, "timeout")
	}

	if _, ok := a.CodexManualScoreAdjustment(); ok {
		t.Fatal("CodexManualScoreAdjustment should fail for malformed input")
	}
	computed, ok := a.CodexComputedScore()
	if !ok || computed != 3.75 {
		t.Fatalf("CodexComputedScore() = %v, %v; want 3.75, true", computed, ok)
	}
	if got := a.CodexScoreReason(); got != "" {
		t.Fatalf("CodexScoreReason() = %q, want empty", got)
	}
	if got := a.CodexLastSelectionReason(); got != "selected after fallback" {
		t.Fatalf("CodexLastSelectionReason() = %q", got)
	}
}

func TestAuthCloneDeepCopiesCodexQuotaMetadata(t *testing.T) {
	t.Parallel()

	original := &Auth{Metadata: map[string]any{
		CodexQuotaMetadataKey: map[string]any{
			"five_hour": map[string]any{
				"remaining": 10.0,
			},
		},
	}}

	cloned := original.Clone()
	quotaMap := cloned.Metadata[CodexQuotaMetadataKey].(map[string]any)
	fiveHourMap := quotaMap["five_hour"].(map[string]any)
	fiveHourMap["remaining"] = 2.0

	originalQuota := original.Metadata[CodexQuotaMetadataKey].(map[string]any)
	originalFiveHour := originalQuota["five_hour"].(map[string]any)
	if got := originalFiveHour["remaining"]; got != 10.0 {
		t.Fatalf("original nested metadata mutated to %v, want 10", got)
	}
}

func TestSetCodexFloatIgnoresNaNAndInf(t *testing.T) {
	t.Parallel()

	a := &Auth{}
	a.SetCodexComputedScore(math.NaN())
	a.SetCodexManualScoreAdjustment(math.Inf(1))
	if a.Metadata != nil && len(a.Metadata) != 0 {
		t.Fatalf("Metadata = %#v, want empty after invalid float writes", a.Metadata)
	}
}

func TestCodexStateReadSanitizesNonFiniteNumericMetadata(t *testing.T) {
	t.Parallel()

	a := &Auth{Metadata: map[string]any{
		CodexQuotaMetadataKey: map[string]any{
			"five_hour": map[string]any{
				"remaining": "NaN",
				"limit":     "+Inf",
			},
			"weekly": map[string]any{
				"remaining": float64(math.Inf(1)),
				"limit":     float32(math.Inf(-1)),
			},
			"refresh_error": "still here",
		},
		CodexManualScoreAdjustmentKey: json.Number("NaN"),
		CodexComputedScoreMetadataKey: "-Inf",
	}}

	quota, ok := a.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true because refresh_error remains")
	}
	if quota.FiveHour.hasData() {
		t.Fatal("FiveHour should be empty after non-finite numeric values are rejected")
	}
	if quota.Weekly.hasData() {
		t.Fatal("Weekly should be empty after non-finite numeric values are rejected")
	}
	if quota.RefreshError != "still here" {
		t.Fatalf("RefreshError = %q, want %q", quota.RefreshError, "still here")
	}

	rawQuota, ok := a.Metadata[CodexQuotaMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("metadata[%q] type = %T, want map[string]any", CodexQuotaMetadataKey, a.Metadata[CodexQuotaMetadataKey])
	}
	if _, exists := rawQuota["five_hour"]; exists {
		t.Fatalf("metadata[%q][five_hour] should be removed after sanitization: %#v", CodexQuotaMetadataKey, rawQuota["five_hour"])
	}
	if _, exists := rawQuota["weekly"]; exists {
		t.Fatalf("metadata[%q][weekly] should be removed after sanitization: %#v", CodexQuotaMetadataKey, rawQuota["weekly"])
	}
	if _, ok := a.CodexManualScoreAdjustment(); ok {
		t.Fatal("CodexManualScoreAdjustment should reject sanitized non-finite value")
	}
	if _, ok := a.CodexComputedScore(); ok {
		t.Fatal("CodexComputedScore should reject sanitized non-finite value")
	}
	if _, exists := a.Metadata[CodexManualScoreAdjustmentKey]; exists {
		t.Fatalf("metadata[%q] should be removed after lazy sanitization", CodexManualScoreAdjustmentKey)
	}
	if _, exists := a.Metadata[CodexComputedScoreMetadataKey]; exists {
		t.Fatalf("metadata[%q] should be removed after lazy sanitization", CodexComputedScoreMetadataKey)
	}
}

func TestCodexStateReadRejectsNonFiniteJSONNumbersAndFloats(t *testing.T) {
	t.Parallel()

	a := &Auth{Metadata: map[string]any{
		CodexQuotaMetadataKey: map[string]any{
			"five_hour": map[string]any{
				"remaining": json.Number("1e309"),
				"limit":     float64(math.NaN()),
			},
		},
	}}

	quota, ok := a.GetCodexQuotaState()
	if ok {
		t.Fatalf("GetCodexQuotaState() ok = true with quota=%#v, want false after all non-finite values are removed", quota)
	}
	if _, exists := a.Metadata[CodexQuotaMetadataKey]; exists {
		t.Fatalf("metadata[%q] should be removed when sanitized quota becomes empty", CodexQuotaMetadataKey)
	}
}

func TestEnsureCodexQuotaRefreshMetadata_SkipsAPIKeyAuths(t *testing.T) {
	t.Parallel()

	a := &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	EnsureCodexQuotaRefreshMetadata(a)
	if a.Metadata != nil {
		t.Fatalf("Metadata = %#v, want nil for codex-api-key auth", a.Metadata)
	}
}

func TestApplyCodexQuotaHeaderUpdate_ParsesResponseHeaders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 15, 3, 0, 0, 0, time.UTC)
	a := &Auth{Provider: "codex", Metadata: map[string]any{"email": "user@example.com"}}
	headers := http.Header{}
	headers.Set("x-ratelimit-5h-remaining", "60")
	headers.Set("x-ratelimit-5h-limit", "100")
	headers.Set("x-ratelimit-5h-reset", "2026-05-15T05:00:00Z")
	headers.Set("x-ratelimit-weekly-remaining", "90")
	headers.Set("x-ratelimit-weekly-limit", "100")
	headers.Set("x-ratelimit-weekly-reset", "2026-05-21T05:00:00Z")
	headers.Set("retry-after", "120")
	if !ApplyCodexQuotaHeaderUpdate(a, headers, now) {
		t.Fatal("ApplyCodexQuotaHeaderUpdate() = false, want true")
	}
	quota, ok := a.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	assertFloatPtr(t, quota.FiveHour.Remaining, 60)
	assertFloatPtr(t, quota.FiveHour.Limit, 100)
	assertFloatPtr(t, quota.Weekly.Remaining, 90)
	assertFloatPtr(t, quota.Weekly.Limit, 100)
	if !a.Unavailable || a.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("expected retry-after to propagate cooldown, got unavailable=%v quota=%#v", a.Unavailable, a.Quota)
	}
}

func TestApplyCodexQuotaHeaderUpdate_IgnoresAmbiguousPrimarySecondaryHeaders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 15, 3, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(7 * 24 * time.Hour)
	fiveHourReset := now.Add(5 * time.Hour)
	a := &Auth{Provider: "codex", Metadata: map[string]any{"email": "user@example.com"}}
	a.SetCodexQuotaState(CodexQuotaState{
		FiveHour: CodexQuotaBucket{Remaining: float64Ptr(20), Limit: float64Ptr(40), ResetAt: &fiveHourReset},
		Weekly:   CodexQuotaBucket{Remaining: float64Ptr(80), Limit: float64Ptr(100), ResetAt: &weeklyReset},
	})
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "44")
	headers.Set("x-codex-primary-reset-at", weeklyReset.Format(time.RFC3339))
	headers.Set("x-codex-secondary-used-percent", "0")
	if ApplyCodexQuotaHeaderUpdate(a, headers, now) {
		t.Fatal("ApplyCodexQuotaHeaderUpdate() = true, want false for ambiguous primary/secondary quota headers")
	}
	quota, ok := a.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	assertFloatPtr(t, quota.FiveHour.Remaining, 20)
	assertTimePtr(t, quota.FiveHour.ResetAt, fiveHourReset)
	assertFloatPtr(t, quota.Weekly.Remaining, 80)
	assertTimePtr(t, quota.Weekly.ResetAt, weeklyReset)
}

func TestApplyCodexQuotaBlockedUntil_UpdatesAndClearsCooldownState(t *testing.T) {
	t.Parallel()

	blockedUntil := time.Now().Add(30 * time.Minute).UTC()
	a := &Auth{Provider: "codex"}
	ApplyCodexQuotaBlockedUntil(a, &blockedUntil)
	if !a.Unavailable {
		t.Fatal("Unavailable = false, want true")
	}
	if !a.NextRetryAfter.Equal(blockedUntil) {
		t.Fatalf("NextRetryAfter = %s, want %s", a.NextRetryAfter, blockedUntil)
	}
	if !a.Quota.Exceeded || a.Quota.Reason != "quota" || !a.Quota.NextRecoverAt.Equal(blockedUntil) {
		t.Fatalf("Quota = %#v, want exceeded quota with blocked-until", a.Quota)
	}

	ApplyCodexQuotaBlockedUntil(a, nil)
	if a.Unavailable {
		t.Fatal("Unavailable = true after clear, want false")
	}
	if !a.NextRetryAfter.IsZero() {
		t.Fatalf("NextRetryAfter = %s, want zero", a.NextRetryAfter)
	}
	if a.Quota.Exceeded || a.Quota.Reason != "" || !a.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("Quota = %#v after clear, want empty quota cooldown state", a.Quota)
	}
}

func TestCodexQuotaState_CodexProbeEligibleResetAtUsesAdjustedAnchorWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	withinWindow := now.Add(5*time.Hour - 10*time.Minute)
	farReset := now.Add(2 * time.Hour)
	state := CodexQuotaState{
		FiveHour: CodexQuotaBucket{ResetAt: &withinWindow},
		Weekly:   CodexQuotaBucket{ResetAt: &farReset},
	}

	resetAt, ok := state.CodexProbeEligibleResetAt(now)
	if !ok || resetAt == nil {
		t.Fatal("CodexProbeEligibleResetAt() = nil/false, want eligible reset")
	}
	if !resetAt.Equal(withinWindow) {
		t.Fatalf("eligible reset = %v, want %v", resetAt, withinWindow)
	}

	state.ProbeResetAt = &withinWindow
	resetAt, ok = state.CodexProbeEligibleResetAt(now)
	if !ok || resetAt == nil || !resetAt.Equal(withinWindow) {
		t.Fatalf("CodexProbeEligibleResetAt() = %v, %v, want %v/true after same-cycle probe", resetAt, ok, withinWindow)
	}

	newCycle := now.Add(5*time.Hour + 5*time.Minute)
	state.FiveHour.ResetAt = &newCycle
	resetAt, ok = state.CodexProbeEligibleResetAt(now)
	if !ok || resetAt == nil || !resetAt.Equal(newCycle) {
		t.Fatalf("CodexProbeEligibleResetAt() after reset change = %v, %v, want %v/true", resetAt, ok, newCycle)
	}

	outsideWindow := now.Add(5*time.Hour + 15*time.Minute)
	state.FiveHour.ResetAt = &outsideWindow
	state.ProbeResetAt = nil
	if resetAt, ok := state.CodexProbeEligibleResetAt(now); ok || resetAt != nil {
		t.Fatalf("CodexProbeEligibleResetAt() outside window = %v, %v, want nil/false", resetAt, ok)
	}
}

func TestCodexQuotaState_CodexProbeEligibleResetAtPrefersNearestEligibleBucket(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	probedReset := now.Add(5*time.Hour - 5*time.Minute)
	otherEligibleReset := now.Add(5*time.Hour + 10*time.Minute)
	state := CodexQuotaState{
		FiveHour:     CodexQuotaBucket{ResetAt: &probedReset},
		Weekly:       CodexQuotaBucket{ResetAt: &otherEligibleReset},
		ProbeResetAt: &probedReset,
	}

	resetAt, ok := state.CodexProbeEligibleResetAt(now)
	if !ok || resetAt == nil || !resetAt.Equal(probedReset) {
		t.Fatalf("CodexProbeEligibleResetAt() = %v, %v, want %v/true", resetAt, ok, probedReset)
	}

	windowResetAt, ok := state.CodexProbeWindowResetAt(now)
	if !ok || windowResetAt == nil || !windowResetAt.Equal(probedReset) {
		t.Fatalf("CodexProbeWindowResetAt() = %v, %v, want %v/true", windowResetAt, ok, probedReset)
	}
}

func TestCodexQuotaState_CodexProbeVerifiedForResetRequiresMatchingVerifiedCycle(t *testing.T) {
	t.Parallel()

	resetAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	verifiedAt := resetAt.Add(2 * time.Minute)
	state := CodexQuotaState{
		ProbeResetAt:    &resetAt,
		ProbeVerifiedAt: &verifiedAt,
		ProbeStatus:     "verified",
	}
	if !state.CodexProbeVerifiedForReset(resetAt) {
		t.Fatal("CodexProbeVerifiedForReset() = false, want true")
	}
	otherReset := resetAt.Add(5 * time.Hour)
	if state.CodexProbeVerifiedForReset(otherReset) {
		t.Fatal("CodexProbeVerifiedForReset() = true for different cycle, want false")
	}
	state.ProbeStatus = "failed"
	if state.CodexProbeVerifiedForReset(resetAt) {
		t.Fatal("CodexProbeVerifiedForReset() = true after failed status, want false")
	}
}

func assertFloatPtr(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("float pointer = nil, want %v", want)
	}
	if *got != want {
		t.Fatalf("*float pointer = %v, want %v", *got, want)
	}
}

func assertTimePtr(t *testing.T, got *time.Time, want time.Time) {
	t.Helper()
	if got == nil {
		t.Fatalf("time pointer = nil, want %v", want)
	}
	if !got.Equal(want) {
		t.Fatalf("*time pointer = %v, want %v", *got, want)
	}
}
