package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexExecutorRefresh_MapsQuotaStateAndCooldown(t *testing.T) {
	t.Parallel()

	blockedUntil := time.Now().Add(12 * time.Minute).UTC().Truncate(time.Second)
	weeklyReset := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	fiveHourReset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":4,"limit":40,"reset_at":"` + fiveHourReset.Format(time.RFC3339) + `"},"weekly":{"remaining":80,"limit":100,"reset_at":"` + weeklyReset.Format(time.RFC3339) + `"},"blocked_until":"` + blockedUntil.Format(time.RFC3339) + `"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(30 * time.Minute),
		Quota: cliproxyauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: time.Now().Add(30 * time.Minute),
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.LastRefreshAt == nil || quota.LastRefreshAt.IsZero() {
		t.Fatal("LastRefreshAt = nil/zero, want set")
	}
	if quota.RefreshStatus != "ok" || quota.RefreshError != "" {
		t.Fatalf("quota refresh metadata = status %q error %q, want ok/empty", quota.RefreshStatus, quota.RefreshError)
	}
	if quota.FiveHour.Remaining == nil || *quota.FiveHour.Remaining != 4 {
		t.Fatalf("FiveHour.Remaining = %#v, want 4", quota.FiveHour.Remaining)
	}
	if quota.Weekly.Remaining == nil || *quota.Weekly.Remaining != 80 {
		t.Fatalf("Weekly.Remaining = %#v, want 80", quota.Weekly.Remaining)
	}
	if !updated.Unavailable || !updated.NextRetryAfter.Equal(blockedUntil) {
		t.Fatalf("auth cooldown = unavailable %v next %s, want true/%s", updated.Unavailable, updated.NextRetryAfter, blockedUntil)
	}
	if !updated.Quota.Exceeded || updated.Quota.Reason != "quota" || !updated.Quota.NextRecoverAt.Equal(blockedUntil) {
		t.Fatalf("auth quota = %#v, want blocked-until propagated", updated.Quota)
	}
	if got := updated.Metadata[cliproxyauth.CodexQuotaRefreshIntervalSecondsKey]; got != int(cliproxyauth.CodexQuotaRefreshInterval/time.Second) {
		t.Fatalf("refresh interval metadata = %#v, want %d", got, int(cliproxyauth.CodexQuotaRefreshInterval/time.Second))
	}
}

func TestCodexExecutorRefresh_UsesUsageEndpointAndParsesUsageWindows(t *testing.T) {
	t.Parallel()

	blockedUntil := time.Now().Add(25 * time.Minute).UTC().Truncate(time.Second)
	fiveHourReset := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	weeklyReset := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	var usageRequests atomic.Int32
	var otherRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			usageRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":19,"reset_at":"` + fiveHourReset.Format(time.RFC3339) + `","blocked_until":"` + blockedUntil.Format(time.RFC3339) + `"},"secondary_window":{"used_percent":42,"reset_after_seconds":518400},"additional_rate_limits":[{"window_name":"weekly","remaining":3,"limit":9,"reset_at":"` + fiveHourReset.Format(time.RFC3339) + `"},{"window_name":"five_hour","remaining":1,"limit":9,"reset_at":"` + weeklyReset.Format(time.RFC3339) + `"}]}}`))
		default:
			otherRequests.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := usageRequests.Load(); got != 1 {
		t.Fatalf("usage endpoint requests = %d, want 1", got)
	}
	if got := otherRequests.Load(); got != 0 {
		t.Fatalf("non-usage endpoint requests = %d, want 0", got)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.Weekly.Remaining == nil || *quota.Weekly.Remaining != 58 {
		t.Fatalf("Weekly.Remaining = %#v, want 58", quota.Weekly.Remaining)
	}
	if quota.Weekly.Limit == nil || *quota.Weekly.Limit != 100 {
		t.Fatalf("Weekly.Limit = %#v, want 100", quota.Weekly.Limit)
	}
	if quota.FiveHour.Remaining == nil || *quota.FiveHour.Remaining != 81 {
		t.Fatalf("FiveHour.Remaining = %#v, want 81", quota.FiveHour.Remaining)
	}
	if quota.FiveHour.Limit == nil || *quota.FiveHour.Limit != 100 {
		t.Fatalf("FiveHour.Limit = %#v, want 100", quota.FiveHour.Limit)
	}
	if quota.Weekly.ResetAt == nil || quota.Weekly.ResetAt.Before(time.Now().Add(143*time.Hour)) || quota.Weekly.ResetAt.After(time.Now().Add(145*time.Hour)) {
		t.Fatalf("Weekly.ResetAt = %v, want about 144h from now via reset_after_seconds", quota.Weekly.ResetAt)
	}
	if quota.FiveHour.ResetAt == nil || !quota.FiveHour.ResetAt.Equal(fiveHourReset) {
		t.Fatalf("FiveHour.ResetAt = %v, want %v", quota.FiveHour.ResetAt, fiveHourReset)
	}
	if !updated.Unavailable || !updated.NextRetryAfter.Equal(blockedUntil) {
		t.Fatalf("auth cooldown = unavailable %v next %s, want true/%s", updated.Unavailable, updated.NextRetryAfter, blockedUntil)
	}
}

func TestCodexExecutorRefresh_UsesAdditionalRateLimitsOnlyAsFallback(t *testing.T) {
	t.Parallel()

	weeklyReset := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	fiveHourReset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"remaining":9,"limit":40,"reset_at":"` + fiveHourReset.Format(time.RFC3339) + `"},"secondary_window":{"remaining":70,"limit":100,"reset_at":"` + weeklyReset.Format(time.RFC3339) + `"},"additional_rate_limits":[{"window_name":"weekly","remaining":1,"limit":2,"reset_at":"` + fiveHourReset.Format(time.RFC3339) + `"},{"window_name":"five_hour","remaining":2,"limit":3,"reset_at":"` + weeklyReset.Format(time.RFC3339) + `"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.FiveHour.Remaining == nil || *quota.FiveHour.Remaining != 9 {
		t.Fatalf("FiveHour.Remaining = %#v, want primary window value 9", quota.FiveHour.Remaining)
	}
	if quota.Weekly.Remaining == nil || *quota.Weekly.Remaining != 70 {
		t.Fatalf("Weekly.Remaining = %#v, want secondary window value 70", quota.Weekly.Remaining)
	}
	if quota.FiveHour.ResetAt == nil || !quota.FiveHour.ResetAt.Equal(fiveHourReset) {
		t.Fatalf("FiveHour.ResetAt = %v, want %v", quota.FiveHour.ResetAt, fiveHourReset)
	}
	if quota.Weekly.ResetAt == nil || !quota.Weekly.ResetAt.Equal(weeklyReset) {
		t.Fatalf("Weekly.ResetAt = %v, want %v", quota.Weekly.ResetAt, weeklyReset)
	}
}

func TestCodexExecutorRefresh_ParsesSecondaryAdditionalRateLimitAsWeekly(t *testing.T) {
	t.Parallel()

	weeklyReset := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	fiveHourReset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"remaining":9,"limit":40,"reset_at":"` + fiveHourReset.Format(time.RFC3339) + `"},"additional_rate_limits":[{"window_name":"secondary","remaining":70,"limit":100,"reset_at":"` + weeklyReset.Format(time.RFC3339) + `"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.FiveHour.Remaining == nil || *quota.FiveHour.Remaining != 9 {
		t.Fatalf("FiveHour.Remaining = %#v, want primary window value 9", quota.FiveHour.Remaining)
	}
	if quota.Weekly.Remaining == nil || *quota.Weekly.Remaining != 70 {
		t.Fatalf("Weekly.Remaining = %#v, want secondary additional rate limit value 70", quota.Weekly.Remaining)
	}
	if quota.Weekly.ResetAt == nil || !quota.Weekly.ResetAt.Equal(weeklyReset) {
		t.Fatalf("Weekly.ResetAt = %v, want %v", quota.Weekly.ResetAt, weeklyReset)
	}
}

func TestCodexExecutorRefresh_UsesWindowDurationToClassifyPrimaryWindow(t *testing.T) {
	t.Parallel()

	weeklyReset := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_after_seconds":604800,"reset_at":"` + weeklyReset.Format(time.RFC3339) + `"},"secondary_window":null}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if codexQuotaBucketHasData(quota.FiveHour) {
		t.Fatalf("FiveHour = %#v, want empty when primary window duration is weekly", quota.FiveHour)
	}
	if quota.Weekly.Remaining == nil || *quota.Weekly.Remaining != 97 {
		t.Fatalf("Weekly.Remaining = %#v, want 97 from used_percent", quota.Weekly.Remaining)
	}
	if quota.Weekly.Limit == nil || *quota.Weekly.Limit != 100 {
		t.Fatalf("Weekly.Limit = %#v, want 100 from used_percent fallback", quota.Weekly.Limit)
	}
	if quota.Weekly.ResetAt == nil || !quota.Weekly.ResetAt.Equal(weeklyReset) {
		t.Fatalf("Weekly.ResetAt = %v, want %v", quota.Weekly.ResetAt, weeklyReset)
	}
}

func TestCodexExecutorRefresh_DoesNotBootstrapProbeWhenWeeklyKnownAndFiveHourMissing(t *testing.T) {
	t.Parallel()

	weeklyReset := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	var usageRequests atomic.Int32
	var probeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			usageRequests.Add(1)
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_after_seconds":604800,"reset_at":"` + weeklyReset.Format(time.RFC3339) + `"},"secondary_window":null}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := usageRequests.Load(); got != 1 {
		t.Fatalf("usage requests = %d, want 1 without bootstrap re-fetch", got)
	}
	if got := probeRequests.Load(); got != 0 {
		t.Fatalf("probe requests = %d, want 0 when weekly is already known", got)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if codexQuotaBucketHasData(quota.FiveHour) {
		t.Fatalf("FiveHour = %#v, want empty when upstream omits five-hour window", quota.FiveHour)
	}
	if quota.Weekly.Remaining == nil || *quota.Weekly.Remaining != 97 {
		t.Fatalf("Weekly.Remaining = %#v, want 97 from weekly window", quota.Weekly.Remaining)
	}
	if quota.BootstrapStatus != "complete" {
		t.Fatalf("BootstrapStatus = %q, want complete", quota.BootstrapStatus)
	}
}

func TestCodexExecutorRefresh_BootstrapProbesWhenWeeklyMissingWithoutImmediateRefetch(t *testing.T) {
	t.Parallel()

	var usageRequests atomic.Int32
	var probeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			usageRequests.Add(1)
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"remaining":8,"limit":40,"limit_window_seconds":18000},"secondary_window":null}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := usageRequests.Load(); got != 1 {
		t.Fatalf("usage requests = %d, want 1 without immediate bootstrap re-fetch", got)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want 1 bootstrap ping", got)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.FiveHour.Remaining == nil || *quota.FiveHour.Remaining != 8 {
		t.Fatalf("FiveHour.Remaining = %#v, want 8 from first usage response", quota.FiveHour.Remaining)
	}
	if codexQuotaBucketHasData(quota.Weekly) {
		t.Fatalf("Weekly = %#v, want empty until a later refresh observes it", quota.Weekly)
	}
	if quota.BootstrapStatus != "pending" {
		t.Fatalf("BootstrapStatus = %q, want pending", quota.BootstrapStatus)
	}
	if quota.BootstrapAttempts != 1 {
		t.Fatalf("BootstrapAttempts = %d, want 1", quota.BootstrapAttempts)
	}
	if quota.BootstrapProbeAt == nil || quota.BootstrapProbeAt.IsZero() {
		t.Fatal("BootstrapProbeAt = nil/zero, want set")
	}
	if quota.BootstrapNextAfter == nil || !quota.BootstrapNextAfter.After(*quota.BootstrapProbeAt) {
		t.Fatalf("BootstrapNextAfter = %v, want after BootstrapProbeAt %v", quota.BootstrapNextAfter, quota.BootstrapProbeAt)
	}
	if quota.BootstrapReason != "weekly_missing" {
		t.Fatalf("BootstrapReason = %q, want weekly_missing", quota.BootstrapReason)
	}
}

func TestCodexExecutorRefresh_ResetRecoveryProbeTakesPriorityOverBootstrap(t *testing.T) {
	t.Parallel()

	nowReset := time.Now().UTC().Add(5*time.Hour + 5*time.Minute).Truncate(time.Second)
	var probeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"remaining":8,"limit":40,"limit_window_seconds":18000,"reset_at":"` + nowReset.Format(time.RFC3339) + `"},"secondary_window":null}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want 1 reset recovery probe", got)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.ProbeStatus != "verified" {
		t.Fatalf("ProbeStatus = %q, want verified", quota.ProbeStatus)
	}
	if quota.ProbeResetAt == nil || !quota.ProbeResetAt.Equal(nowReset) {
		t.Fatalf("ProbeResetAt = %v, want %v", quota.ProbeResetAt, nowReset)
	}
	if quota.BootstrapStatus != "" {
		t.Fatalf("BootstrapStatus = %q, want empty because reset recovery takes priority", quota.BootstrapStatus)
	}
}

func TestCodexExecutorRefresh_BootstrapProbeBacksOffAfterRepeatedAttempts(t *testing.T) {
	t.Parallel()

	var probeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"remaining":8,"limit":40,"limit_window_seconds":18000},"secondary_window":null}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}
	firstQuota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("first GetCodexQuotaState() ok = false, want true")
	}
	if firstQuota.BootstrapAttempts != 1 {
		t.Fatalf("first BootstrapAttempts = %d, want 1", firstQuota.BootstrapAttempts)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests after first refresh = %d, want 1", got)
	}

	secondAt := firstQuota.BootstrapNextAfter.Add(time.Second)
	auth.SetCodexQuotaState(firstQuota)
	quotaAfterSecond, blockedUntil := executor.bootstrapCodexQuotaUsageIfWindowMissing(context.Background(), auth, firstQuota, secondAt)
	if blockedUntil != nil {
		t.Fatalf("blockedUntil = %v, want nil", blockedUntil)
	}
	if quotaAfterSecond.BootstrapAttempts != 2 {
		t.Fatalf("second BootstrapAttempts = %d, want 2", quotaAfterSecond.BootstrapAttempts)
	}
	if got := probeRequests.Load(); got != 2 {
		t.Fatalf("probe requests after second attempt = %d, want 2", got)
	}
	if got := quotaAfterSecond.BootstrapNextAfter.Sub(*quotaAfterSecond.BootstrapProbeAt); got != time.Hour {
		t.Fatalf("second bootstrap backoff = %s, want 1h", got)
	}

	blockedState, _ := executor.bootstrapCodexQuotaUsageIfWindowMissing(context.Background(), auth, quotaAfterSecond, quotaAfterSecond.BootstrapProbeAt.Add(30*time.Minute))
	if blockedState.BootstrapAttempts != 2 {
		t.Fatalf("blocked BootstrapAttempts = %d, want unchanged 2", blockedState.BootstrapAttempts)
	}
	if got := probeRequests.Load(); got != 2 {
		t.Fatalf("probe requests during backoff = %d, want unchanged 2", got)
	}
}

func TestCodexExecutorRefresh_PreservesPriorWindowsOnQuotaFetchFailure(t *testing.T) {
	t.Parallel()

	weeklyReset := time.Now().Add(3 * 24 * time.Hour).UTC().Truncate(time.Second)
	lastRefresh := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}
	auth.SetCodexQuotaState(cliproxyauth.CodexQuotaState{
		Weekly: cliproxyauth.CodexQuotaBucket{
			Remaining: float64Ptr(42),
			Limit:     float64Ptr(100),
			ResetAt:   &weeklyReset,
		},
		LastRefreshAt: &lastRefresh,
		RefreshStatus: "ok",
	})

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.Weekly.Remaining == nil || *quota.Weekly.Remaining != 42 {
		t.Fatalf("Weekly.Remaining = %#v, want preserved 42", quota.Weekly.Remaining)
	}
	if quota.LastRefreshAt == nil || !quota.LastRefreshAt.Equal(lastRefresh) {
		t.Fatalf("LastRefreshAt = %v, want preserved %v on failure", quota.LastRefreshAt, lastRefresh)
	}
	if quota.RefreshStatus != "error" {
		t.Fatalf("RefreshStatus = %q, want error", quota.RefreshStatus)
	}
	if quota.RefreshError == "" {
		t.Fatal("RefreshError = empty, want failure detail")
	}
	if updated.Unavailable {
		t.Fatal("Unavailable = true, want prior quota windows preserved without inventing new cooldown")
	}
}

func TestCodexExecutorRefresh_SkipsPhase3QuotaEnrichmentForAPIKeyAuth(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	originalRetry := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	originalRecover := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email": "",
		},
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/backend-api/codex",
		},
		Unavailable:    true,
		NextRetryAfter: originalRetry,
		Quota: cliproxyauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: originalRecover,
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("quota probe requests = %d, want 0", got)
	}
	if _, ok := updated.GetCodexQuotaState(); ok {
		t.Fatal("GetCodexQuotaState() ok = true, want false for api-key auth")
	}
	if _, ok := updated.Metadata[cliproxyauth.CodexQuotaRefreshIntervalSecondsKey]; ok {
		t.Fatalf("refresh interval metadata present = %#v, want absent", updated.Metadata[cliproxyauth.CodexQuotaRefreshIntervalSecondsKey])
	}
	if !updated.Unavailable || !updated.NextRetryAfter.Equal(originalRetry) {
		t.Fatalf("auth cooldown = unavailable %v next %s, want true/%s", updated.Unavailable, updated.NextRetryAfter, originalRetry)
	}
	if !updated.Quota.Exceeded || updated.Quota.Reason != "quota" || !updated.Quota.NextRecoverAt.Equal(originalRecover) {
		t.Fatalf("auth quota = %#v, want unchanged", updated.Quota)
	}
	if _, ok := updated.Metadata["last_refresh"]; !ok {
		t.Fatal("last_refresh metadata missing, want preserved unrelated refresh behavior")
	}
}

func TestCodexExecutorRefresh_HealthyInWindowSendsProbe(t *testing.T) {
	t.Parallel()

	nowReset := time.Now().UTC().Add(5*time.Hour + 5*time.Minute).Truncate(time.Second)
	var probeRequests atomic.Int32
	var probeBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":12,"limit":40,"reset_at":"` + nowReset.Format(time.RFC3339) + `"},"weekly":{"remaining":88,"limit":100,"reset_at":"` + nowReset.Add(6*24*time.Hour).Format(time.RFC3339) + `"}}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read probe body: %v", err)
			}
			probeBody.Store(string(body))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want 1", got)
	}
	if got, _ := probeBody.Load().(string); got != `{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}` {
		t.Fatalf("probe body = %s, want gpt-5.4-mini probe payload", got)
	}
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("healthy auth should remain available: unavailable=%v next=%s", updated.Unavailable, updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("healthy auth quota cooldown should remain clear: %#v", updated.Quota)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.ProbeResetAt == nil || !quota.ProbeResetAt.Equal(nowReset) {
		t.Fatalf("ProbeResetAt = %v, want %v", quota.ProbeResetAt, nowReset)
	}
	if quota.ProbeStatus != "verified" {
		t.Fatalf("ProbeStatus = %q, want verified", quota.ProbeStatus)
	}
	if quota.ProbeVerifiedAt == nil || quota.ProbeVerifiedAt.IsZero() {
		t.Fatal("ProbeVerifiedAt = nil/zero, want set")
	}
}

func TestCodexExecutorRefresh_UsesConfiguredProbePayload(t *testing.T) {
	t.Parallel()

	nowReset := time.Now().UTC().Add(5*time.Hour + 5*time.Minute).Truncate(time.Second)
	var probeBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":12,"limit":40,"reset_at":"` + nowReset.Format(time.RFC3339) + `"},"weekly":{"remaining":88,"limit":100,"reset_at":"` + nowReset.Add(6*24*time.Hour).Format(time.RFC3339) + `"}}}`))
		case "/backend-api/codex/responses/compact":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read probe body: %v", err)
			}
			probeBody.Store(string(body))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{
		CodexQuotaProbe: config.CodexQuotaProbe{
			Model:  "gpt-5.4",
			Prompt: "quota check",
		},
	})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}

	if _, err := executor.Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	got, _ := probeBody.Load().(string)
	if !strings.Contains(got, `"model":"gpt-5.4"`) || !strings.Contains(got, `"text":"quota check"`) {
		t.Fatalf("probe body = %s, want configured model and prompt", got)
	}
}

func TestCodexExecutorRefresh_ProbeWindowReleasesStickyCodexAuthBeforeProbe(t *testing.T) {
	stickyAuthID := "sticky-probe-auth"
	cliproxyauth.ReleaseCodexStickyAuth(stickyAuthID)
	defer cliproxyauth.ReleaseCodexStickyAuth(stickyAuthID)

	nowReset := time.Now().UTC().Add(5*time.Hour + 5*time.Minute).Truncate(time.Second)
	var stickyDuringProbe atomic.Value
	stickyDuringProbe.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":12,"limit":40,"reset_at":"` + nowReset.Format(time.RFC3339) + `"},"weekly":{"remaining":88,"limit":100,"reset_at":"` + nowReset.Add(6*24*time.Hour).Format(time.RFC3339) + `"}}}`))
		case "/backend-api/codex/responses/compact":
			stickyDuringProbe.Store(cliproxyauth.CurrentCodexStickyAuthID())
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	weeklyReset := nowReset.Add(6 * 24 * time.Hour)
	fiveHourRemaining := 12.0
	weeklyRemaining := 88.0
	weeklyLimit := 100.0
	auth := &cliproxyauth.Auth{
		ID:       stickyAuthID,
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
	}
	auth.SetCodexQuotaState(cliproxyauth.CodexQuotaState{
		FiveHour:      cliproxyauth.CodexQuotaBucket{Remaining: &fiveHourRemaining, Limit: float64Ptr(40), ResetAt: &nowReset},
		Weekly:        cliproxyauth.CodexQuotaBucket{Remaining: &weeklyRemaining, Limit: &weeklyLimit, ResetAt: &weeklyReset},
		LastRefreshAt: func() *time.Time { t := time.Now().UTC().Add(-1 * time.Minute); return &t }(),
		RefreshStatus: "ok",
	})
	cliproxyauth.RecalculateCurrentCodexStickyAuth([]*cliproxyauth.Auth{auth}, time.Now().UTC())
	if got := cliproxyauth.CurrentCodexStickyAuthID(); got != stickyAuthID {
		t.Fatalf("CurrentCodexStickyAuthID() before refresh = %q, want %q", got, stickyAuthID)
	}

	executor := NewCodexExecutor(&config.Config{})
	if _, err := executor.Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got, _ := stickyDuringProbe.Load().(string); got != "" {
		t.Fatalf("CurrentCodexStickyAuthID() during probe = %q, want cleared", got)
	}
}

func TestCodexExecutorRefresh_PreservesCooldownUntilProbeVerifiesRecovery(t *testing.T) {
	t.Parallel()

	nowReset := time.Now().UTC().Add(5*time.Hour + 5*time.Minute).Truncate(time.Second)
	var probeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":10,"limit":40,"reset_at":"` + nowReset.Format(time.RFC3339) + `"},"weekly":{"remaining":80,"limit":100,"reset_at":"` + nowReset.Add(6*24*time.Hour).Format(time.RFC3339) + `"}}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"probe","output":[],"usage":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	originalRetry := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Unavailable:    true,
		NextRetryAfter: originalRetry,
		Quota:          cliproxyauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: originalRetry},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want 1", got)
	}
	if !updated.Unavailable || !updated.NextRetryAfter.Equal(originalRetry) {
		t.Fatalf("cooldown cleared unexpectedly: unavailable=%v next=%s want true/%s", updated.Unavailable, updated.NextRetryAfter, originalRetry)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.ProbeResetAt == nil || !quota.ProbeResetAt.Equal(nowReset) {
		t.Fatalf("ProbeResetAt = %v, want %v", quota.ProbeResetAt, nowReset)
	}
	if quota.ProbeStatus != "failed" {
		t.Fatalf("ProbeStatus = %q, want failed", quota.ProbeStatus)
	}
	if quota.ProbeVerifiedAt != nil {
		t.Fatalf("ProbeVerifiedAt = %v, want nil", quota.ProbeVerifiedAt)
	}
	if quota.ProbeError == "" {
		t.Fatal("ProbeError = empty, want failure detail")
	}
	if quota.RefreshStatus != "ok" {
		t.Fatalf("RefreshStatus = %q, want ok after successful refresh", quota.RefreshStatus)
	}
}

func TestCodexExecutorRefresh_VerifiedProbeClearsCooldownAndReprobesSameResetCycle(t *testing.T) {
	t.Parallel()

	nowReset := time.Now().UTC().Add(5*time.Hour + 5*time.Minute).Truncate(time.Second)
	var probeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":12,"limit":40,"reset_at":"` + nowReset.Format(time.RFC3339) + `"},"weekly":{"remaining":88,"limit":100,"reset_at":"` + nowReset.Add(6*24*time.Hour).Format(time.RFC3339) + `"}}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	originalRetry := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Unavailable:    true,
		NextRetryAfter: originalRetry,
		Quota:          cliproxyauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: originalRetry},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() first error = %v", err)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests after first refresh = %d, want 1", got)
	}
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("cooldown not cleared after verified probe: unavailable=%v next=%s", updated.Unavailable, updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("quota cooldown not cleared after verified probe: %#v", updated.Quota)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.ProbeStatus != "verified" {
		t.Fatalf("ProbeStatus = %q, want verified", quota.ProbeStatus)
	}
	if quota.ProbeVerifiedAt == nil || quota.ProbeVerifiedAt.IsZero() {
		t.Fatal("ProbeVerifiedAt = nil/zero, want set")
	}

	updated.Unavailable = true
	updated.NextRetryAfter = originalRetry
	updated.Quota = cliproxyauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: originalRetry}
	updatedAgain, err := executor.Refresh(context.Background(), updated)
	if err != nil {
		t.Fatalf("Refresh() second error = %v", err)
	}
	if got := probeRequests.Load(); got != 2 {
		t.Fatalf("probe requests after second refresh = %d, want 2 for same reset cycle reprobe", got)
	}
	quotaAgain, ok := updatedAgain.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() second ok = false, want true")
	}
	if quotaAgain.ProbeStatus != "verified" {
		t.Fatalf("ProbeStatus after second refresh = %q, want verified", quotaAgain.ProbeStatus)
	}
	if updatedAgain.Unavailable || !updatedAgain.NextRetryAfter.IsZero() {
		t.Fatalf("same-cycle verified refresh should clear cooldown without a new probe: unavailable=%v next=%s", updatedAgain.Unavailable, updatedAgain.NextRetryAfter)
	}
}

func TestCodexExecutorRefresh_NewResetCycleTriggersNewProbe(t *testing.T) {
	t.Parallel()

	firstReset := time.Now().UTC().Add(5*time.Hour + 5*time.Minute).Truncate(time.Second)
	secondReset := firstReset.Add(7 * time.Minute)
	var probeRequests atomic.Int32
	var usageRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			count := usageRequests.Add(1)
			resetAt := firstReset
			if count > 1 {
				resetAt = secondReset
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":12,"limit":40,"reset_at":"` + resetAt.Format(time.RFC3339) + `"},"weekly":{"remaining":88,"limit":100,"reset_at":"` + resetAt.Add(6*24*time.Hour).Format(time.RFC3339) + `"}}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	originalRetry := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Unavailable:    true,
		NextRetryAfter: originalRetry,
		Quota:          cliproxyauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: originalRetry},
	}

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() first error = %v", err)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests after first refresh = %d, want 1", got)
	}

	updated.Unavailable = true
	updated.NextRetryAfter = originalRetry
	updated.Quota = cliproxyauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: originalRetry}
	updatedAgain, err := executor.Refresh(context.Background(), updated)
	if err != nil {
		t.Fatalf("Refresh() second error = %v", err)
	}
	if got := probeRequests.Load(); got != 2 {
		t.Fatalf("probe requests after second refresh = %d, want 2 for new reset cycle", got)
	}
	quotaAgain, ok := updatedAgain.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() second ok = false, want true")
	}
	if quotaAgain.ProbeResetAt == nil || !quotaAgain.ProbeResetAt.Equal(secondReset) {
		t.Fatalf("ProbeResetAt after second refresh = %v, want %v", quotaAgain.ProbeResetAt, secondReset)
	}
	if updatedAgain.Unavailable || !updatedAgain.NextRetryAfter.IsZero() {
		t.Fatalf("cooldown not cleared after second-cycle verified probe: unavailable=%v next=%s", updatedAgain.Unavailable, updatedAgain.NextRetryAfter)
	}
}

func TestCodexExecutorRefresh_ReprobesNearestEligibleWindowEvenWhenAlreadyProbed(t *testing.T) {
	t.Parallel()

	probedReset := time.Now().UTC().Add(5*time.Hour - 5*time.Minute).Truncate(time.Second)
	otherEligibleReset := time.Now().UTC().Add(5*time.Hour + 10*time.Minute).Truncate(time.Second)
	var probeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"quota":{"five_hour":{"remaining":12,"limit":40,"reset_at":"` + probedReset.Format(time.RFC3339) + `"},"weekly":{"remaining":88,"limit":100,"reset_at":"` + otherEligibleReset.Format(time.RFC3339) + `"}}}`))
		case "/backend-api/codex/responses/compact":
			probeRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	originalRetry := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":        "user@example.com",
			"access_token": "token-123",
		},
		Attributes: map[string]string{
			"base_url": server.URL + "/backend-api/codex",
		},
		Unavailable:    true,
		NextRetryAfter: originalRetry,
		Quota:          cliproxyauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: originalRetry},
	}
	verifiedAt := time.Now().UTC().Truncate(time.Second)
	auth.SetCodexQuotaState(cliproxyauth.CodexQuotaState{
		ProbeResetAt:    &probedReset,
		ProbeVerifiedAt: &verifiedAt,
		ProbeStatus:     "verified",
	})

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got := probeRequests.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want 1 for same-window reprobe", got)
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok {
		t.Fatal("GetCodexQuotaState() ok = false, want true")
	}
	if quota.ProbeResetAt == nil || !quota.ProbeResetAt.Equal(probedReset) {
		t.Fatalf("ProbeResetAt = %v, want %v", quota.ProbeResetAt, probedReset)
	}
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("cooldown not cleared after same-window verified probe: unavailable=%v next=%s", updated.Unavailable, updated.NextRetryAfter)
	}
}

func float64Ptr(v float64) *float64 {
	return &v
}
