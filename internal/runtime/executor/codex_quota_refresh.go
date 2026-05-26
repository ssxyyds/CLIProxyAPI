package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func (e *CodexExecutor) verifyCodexQuotaRecovery(ctx context.Context, auth *cliproxyauth.Auth, state cliproxyauth.CodexQuotaState, now, resetAt time.Time) (cliproxyauth.CodexQuotaState, bool) {
	resetAt = resetAt.UTC()
	probeAt := now.UTC()
	state.ProbeResetAt = &resetAt
	state.ProbeAt = &probeAt
	state.ProbeVerifiedAt = nil
	state.ProbeStatus = "failed"
	state.ProbeError = ""
	if auth != nil {
		cliproxyauth.ReleaseCodexStickyAuth(auth.ID)
	}

	if err := e.runCodexQuotaRecoveryProbe(ctx, auth); err != nil {
		state.ProbeStatus = "failed"
		state.ProbeError = strings.TrimSpace(err.Error())
		return state, false
	}

	state.ProbeVerifiedAt = &probeAt
	state.ProbeStatus = "verified"
	state.ProbeError = ""
	return state, state.CodexProbeVerifiedForReset(resetAt)
}

func (e *CodexExecutor) bootstrapCodexQuotaUsageIfWindowMissing(ctx context.Context, auth *cliproxyauth.Auth, state cliproxyauth.CodexQuotaState, now time.Time) (cliproxyauth.CodexQuotaState, *time.Time) {
	if codexQuotaBucketHasData(state.Weekly) {
		return markCodexQuotaBootstrapComplete(state), nil
	}
	if codexQuotaBootstrapProbeBackoffActive(state, now) {
		return state, nil
	}

	probeAt := now.UTC()
	attempts := state.BootstrapAttempts + 1
	nextAfter := probeAt.Add(codexQuotaBootstrapBackoff(attempts))
	state.BootstrapProbeAt = &probeAt
	state.BootstrapVerifiedAt = nil
	state.BootstrapNextAfter = &nextAfter
	state.BootstrapAttempts = attempts
	state.BootstrapStatus = "failed"
	state.BootstrapReason = "weekly_missing"
	state.BootstrapError = ""
	if err := e.runCodexQuotaRecoveryProbe(ctx, auth); err != nil {
		state.BootstrapError = strings.TrimSpace(err.Error())
		return state, nil
	}

	state.BootstrapVerifiedAt = &probeAt
	state.BootstrapStatus = "pending"
	state.BootstrapError = ""
	return state, nil
}

func markCodexQuotaBootstrapComplete(state cliproxyauth.CodexQuotaState) cliproxyauth.CodexQuotaState {
	state.BootstrapStatus = "complete"
	state.BootstrapError = ""
	state.BootstrapReason = ""
	state.BootstrapNextAfter = nil
	return state
}

func codexQuotaBootstrapProbeBackoffActive(state cliproxyauth.CodexQuotaState, now time.Time) bool {
	if state.BootstrapNextAfter == nil || state.BootstrapNextAfter.IsZero() {
		return false
	}
	return now.UTC().Before(state.BootstrapNextAfter.UTC())
}

func codexQuotaBootstrapBackoff(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return cliproxyauth.CodexQuotaRefreshInterval
	case attempts == 2:
		return time.Hour
	case attempts == 3:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func (e *CodexExecutor) runCodexQuotaRecoveryProbe(ctx context.Context, auth *cliproxyauth.Auth) error {
	token, baseURL := codexCreds(auth)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("codex recovery probe: access token missing")
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	probeURL := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	body := codexQuotaProbePayload(e.cfg)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, probeURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("codex recovery probe: build request failed: %w", err)
	}
	applyCodexHeaders(httpReq, auth, token, false, e.cfg)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
	if err != nil {
		return fmt.Errorf("codex recovery probe: request failed: %w", err)
	}
	defer closeHTTPResponseBody(httpResp, "codex recovery probe: close response body error")
	responseBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("codex recovery probe: read response failed: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("codex recovery probe: %s returned %d: %s", probeURL, httpResp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if !codexProbeUsageEvidence(responseBody) {
		return fmt.Errorf("codex recovery probe: no usage evidence in successful response")
	}
	return nil
}

func codexQuotaProbePayload(cfg *config.Config) []byte {
	model := "gpt-5.4-mini"
	prompt := "ping"
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.CodexQuotaProbe.Model); configured != "" {
			model = configured
		}
		if configured := strings.TrimSpace(cfg.CodexQuotaProbe.Prompt); configured != "" {
			prompt = configured
		}
	}
	payload := struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}{
		Model:        model,
		Instructions: "",
		Input: []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{
			{
				Type: "message",
				Role: "user",
				Content: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{
					{Type: "input_text", Text: prompt},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	}
	return body
}

func codexProbeUsageEvidence(body []byte) bool {
	paths := []string{
		"usage.total_tokens",
		"usage.prompt_tokens",
		"usage.input_tokens",
		"usage.completion_tokens",
		"usage.output_tokens",
		"response.usage.total_tokens",
		"response.usage.prompt_tokens",
		"response.usage.input_tokens",
		"response.usage.completion_tokens",
		"response.usage.output_tokens",
	}
	for _, path := range paths {
		if gjson.GetBytes(body, path).Int() > 0 {
			return true
		}
	}
	return false
}

type codexQuotaRefreshPayload struct {
	state        cliproxyauth.CodexQuotaState
	blockedUntil *time.Time
}

func (e *CodexExecutor) refreshCodexQuotaState(ctx context.Context, auth *cliproxyauth.Auth, now time.Time) (cliproxyauth.CodexQuotaState, *time.Time, error) {
	token, baseURL := codexCreds(auth)
	if strings.TrimSpace(token) == "" {
		return cliproxyauth.CodexQuotaState{}, nil, fmt.Errorf("codex quota refresh: access token missing")
	}
	urls := codexQuotaRefreshURLs(baseURL)
	previous, _ := auth.GetCodexQuotaState()
	merged := cloneCodexQuotaState(previous)
	var blockedUntil *time.Time
	var errs []string
	hadData := false
	for _, endpoint := range urls {
		body, err := e.fetchCodexQuotaRefreshDocument(ctx, auth, token, endpoint)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		payload, ok := parseCodexQuotaRefreshPayload(body)
		if !ok {
			errs = append(errs, fmt.Sprintf("codex quota refresh: no quota data at %s", endpoint))
			continue
		}
		hadData = true
		if codexQuotaBucketHasData(payload.state.FiveHour) {
			merged.FiveHour = payload.state.FiveHour
		}
		if codexQuotaBucketHasData(payload.state.Weekly) {
			merged.Weekly = payload.state.Weekly
		}
		if !codexQuotaBucketHasData(payload.state.FiveHour) && codexQuotaFiveHourResetOutsideWindow(merged.FiveHour, now) {
			merged.FiveHour = cliproxyauth.CodexQuotaBucket{}
		}
		if payload.blockedUntil != nil && !payload.blockedUntil.IsZero() {
			until := payload.blockedUntil.UTC()
			blockedUntil = &until
		}
		if codexQuotaBucketHasData(payload.state.FiveHour) && codexQuotaBucketHasData(payload.state.Weekly) {
			break
		}
	}
	if !hadData {
		if len(errs) == 0 {
			return cliproxyauth.CodexQuotaState{}, nil, fmt.Errorf("codex quota refresh: no usable quota response")
		}
		return cliproxyauth.CodexQuotaState{}, nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	merged.LastRefreshAt = &now
	merged.RefreshStatus = "ok"
	merged.RefreshError = ""
	return merged, blockedUntil, nil
}

func (e *CodexExecutor) fetchCodexQuotaRefreshDocument(ctx context.Context, auth *cliproxyauth.Auth, token, rawURL string) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("codex quota refresh: build request %s: %w", rawURL, err)
	}
	applyCodexHeaders(httpReq, auth, token, false, e.cfg)
	httpReq.Header.Set("Accept", "application/json")
	httpResp, err := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex quota refresh: request %s failed: %w", rawURL, err)
	}
	defer closeHTTPResponseBody(httpResp, "codex quota refresh: close response body error")
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codex quota refresh: read %s failed: %w", rawURL, err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, codexQuotaRefreshHTTPError(httpResp.StatusCode, httpResp.Header.Get("Content-Type"), body)
	}
	return body, nil
}

func codexQuotaRefreshHTTPError(statusCode int, contentType string, body []byte) error {
	message := strings.TrimSpace(helps.SummarizeErrorBody(contentType, body))
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return fmt.Errorf("codex quota refresh: usage returned %d: %s", statusCode, message)
}

func codexQuotaRefreshURLs(baseURL string) []string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		trimmed = "https://chatgpt.com/backend-api/codex"
	}
	candidates := make([]string, 0, 2)
	if whamUsageURL := codexQuotaWhamUsageURL(trimmed); whamUsageURL != "" {
		candidates = append(candidates, whamUsageURL)
	}
	candidates = append(candidates, trimmed+"/usage")
	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func codexQuotaWhamUsageURL(baseURL string) string {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/backend-api/codex"):
		parsed.Path = strings.TrimSuffix(path, "/codex") + "/wham/usage"
	case strings.HasSuffix(path, "/backend-api/wham"):
		parsed.Path = path + "/usage"
	case strings.HasSuffix(path, "/backend-api/wham/usage"):
		parsed.Path = path
	default:
		return ""
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func parseCodexQuotaRefreshPayload(body []byte) (codexQuotaRefreshPayload, bool) {
	payload := codexQuotaRefreshPayload{}
	payload.state.FiveHour = parseCodexQuotaBucket(body,
		"quota.five_hour", "quota.fiveHour", "five_hour", "fiveHour",
		"usage.five_hour", "usage.fiveHour", "ratelimits.five_hour", "ratelimits.fiveHour",
	)
	payload.state.Weekly = parseCodexQuotaBucket(body,
		"quota.weekly", "quota.weekly_window", "weekly", "weekly_window",
		"usage.weekly", "usage.weekly_window", "ratelimits.weekly", "ratelimits.weekly_window",
	)
	mergeCodexRateLimitWindowBucket(body, &payload.state, "rate_limit.primary_window", "five_hour")
	mergeCodexRateLimitWindowBucket(body, &payload.state, "rate_limit.secondary_window", "weekly")
	mergeCodexAdditionalRateLimitBuckets(body, &payload.state)
	if blockedUntil, ok := firstTimePath(body,
		"quota_blocked_until", "quota.blocked_until", "quota.blockedUntil",
		"blocked_until", "blockedUntil", "ratelimits.blocked_until", "ratelimits.blockedUntil",
		"error.resets_at", "rate_limit.blocked_until", "rate_limit.blockedUntil",
		"rate_limit.primary_window.blocked_until", "rate_limit.primary_window.blockedUntil",
		"rate_limit.secondary_window.blocked_until", "rate_limit.secondary_window.blockedUntil",
	); ok {
		payload.blockedUntil = &blockedUntil
	}
	return payload, codexQuotaBucketHasData(payload.state.FiveHour) || codexQuotaBucketHasData(payload.state.Weekly) || payload.blockedUntil != nil
}

func parseCodexQuotaBucket(body []byte, prefixes ...string) cliproxyauth.CodexQuotaBucket {
	return parseCodexQuotaBucketAt(body, time.Now().UTC(), prefixes...)
}

func parseCodexQuotaBucketAt(body []byte, now time.Time, prefixes ...string) cliproxyauth.CodexQuotaBucket {
	for _, prefix := range prefixes {
		bucket := cliproxyauth.CodexQuotaBucket{}
		if remaining, ok := firstFloatPath(body,
			codexQuotaFieldPath(prefix, "remaining"),
			codexQuotaFieldPath(prefix, "remaining_quota"),
			codexQuotaFieldPath(prefix, "available"),
			codexQuotaFieldPath(prefix, "left"),
		); ok {
			bucket.Remaining = &remaining
		}
		if limit, ok := firstFloatPath(body,
			codexQuotaFieldPath(prefix, "limit"),
			codexQuotaFieldPath(prefix, "quota"),
			codexQuotaFieldPath(prefix, "total"),
			codexQuotaFieldPath(prefix, "max"),
		); ok {
			bucket.Limit = &limit
		}
		if resetAt, ok := firstQuotaResetPath(body, now,
			codexQuotaFieldPath(prefix, "reset_at"),
			codexQuotaFieldPath(prefix, "resetAt"),
			codexQuotaFieldPath(prefix, "resets_at"),
			codexQuotaFieldPath(prefix, "resetsAt"),
			codexQuotaFieldPath(prefix, "next_reset_at"),
			codexQuotaFieldPath(prefix, "reset_after_seconds"),
			codexQuotaFieldPath(prefix, "resetAfterSeconds"),
			codexQuotaFieldPath(prefix, "resets_after_seconds"),
			codexQuotaFieldPath(prefix, "resetsAfterSeconds"),
		); ok {
			bucket.ResetAt = &resetAt
		}
		if bucket.Remaining == nil {
			if usedPercent, ok := firstFloatPath(body,
				codexQuotaFieldPath(prefix, "used_percent"),
				codexQuotaFieldPath(prefix, "usedPercent"),
			); ok {
				limit := 100.0
				remaining := limit - usedPercent
				if remaining < 0 {
					remaining = 0
				}
				if bucket.Limit == nil {
					bucket.Limit = &limit
				}
				bucket.Remaining = &remaining
			}
		}
		if codexQuotaBucketHasData(bucket) {
			return bucket
		}
	}
	return cliproxyauth.CodexQuotaBucket{}
}

func mergeCodexRateLimitWindowBucket(body []byte, state *cliproxyauth.CodexQuotaState, path, fallbackKind string) {
	if state == nil {
		return
	}
	result := gjson.GetBytes(body, path)
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	bucket := parseCodexQuotaBucketAt(body, time.Now().UTC(), path)
	if !codexQuotaBucketHasData(bucket) {
		return
	}
	kind := codexQuotaBucketWindowKind([]byte(result.Raw))
	if kind == "" {
		kind = fallbackKind
	}
	switch kind {
	case "weekly":
		if !codexQuotaBucketHasData(state.Weekly) {
			state.Weekly = bucket
		}
	case "five_hour":
		if !codexQuotaBucketHasData(state.FiveHour) {
			state.FiveHour = bucket
		}
	}
}

func codexQuotaFieldPath(prefix, field string) string {
	prefix = strings.TrimSpace(prefix)
	field = strings.TrimSpace(field)
	if prefix == "" {
		return field
	}
	if field == "" {
		return prefix
	}
	return prefix + "." + field
}

func mergeCodexAdditionalRateLimitBuckets(body []byte, state *cliproxyauth.CodexQuotaState) {
	if state == nil {
		return
	}
	for _, path := range []string{"rate_limit.additional_rate_limits", "additional_rate_limits"} {
		result := gjson.GetBytes(body, path)
		if !result.Exists() {
			continue
		}
		for _, item := range result.Array() {
			bucket := parseCodexQuotaBucketAt([]byte(item.Raw), time.Now().UTC(), "")
			if !codexQuotaBucketHasData(bucket) {
				continue
			}
			switch codexQuotaBucketWindowKind([]byte(item.Raw)) {
			case "weekly":
				if !codexQuotaBucketHasData(state.Weekly) {
					state.Weekly = bucket
				}
			case "five_hour":
				if !codexQuotaBucketHasData(state.FiveHour) {
					state.FiveHour = bucket
				}
			}
		}
	}
}

func codexQuotaBucketWindowKind(body []byte) string {
	label, _ := firstStringPath(body,
		"name", "key", "id", "window", "window_name", "windowName", "label", "slug",
	)
	label = strings.ToLower(strings.TrimSpace(label))
	if strings.Contains(label, "week") {
		return "weekly"
	}
	if (strings.Contains(label, "five") || strings.Contains(label, "5")) && strings.Contains(label, "hour") {
		return "five_hour"
	}
	if seconds, ok := firstFloatPath(body,
		"window_seconds", "duration_seconds", "interval_seconds", "reset_interval_seconds", "limit_window_seconds", "limitWindowSeconds",
	); ok {
		switch {
		case seconds >= 6*24*60*60:
			return "weekly"
		case seconds >= 4*60*60 && seconds <= 6*60*60:
			return "five_hour"
		}
	}
	if strings.Contains(label, "secondary") {
		return "weekly"
	}
	if strings.Contains(label, "primary") {
		return "five_hour"
	}
	return ""
}

func codexQuotaBucketHasData(bucket cliproxyauth.CodexQuotaBucket) bool {
	return bucket.Remaining != nil || bucket.Limit != nil || bucket.ResetAt != nil
}

func codexQuotaFiveHourResetOutsideWindow(bucket cliproxyauth.CodexQuotaBucket, now time.Time) bool {
	if bucket.ResetAt == nil || bucket.ResetAt.IsZero() {
		return false
	}
	return bucket.ResetAt.UTC().After(now.UTC().Add(6 * time.Hour))
}

func cloneCodexQuotaState(state cliproxyauth.CodexQuotaState) cliproxyauth.CodexQuotaState {
	cloned := cliproxyauth.CodexQuotaState{
		RefreshStatus:     state.RefreshStatus,
		RefreshError:      state.RefreshError,
		ProbeStatus:       state.ProbeStatus,
		ProbeError:        state.ProbeError,
		BootstrapStatus:   state.BootstrapStatus,
		BootstrapError:    state.BootstrapError,
		BootstrapReason:   state.BootstrapReason,
		BootstrapAttempts: state.BootstrapAttempts,
	}
	if state.FiveHour.Remaining != nil {
		value := *state.FiveHour.Remaining
		cloned.FiveHour.Remaining = &value
	}
	if state.FiveHour.Limit != nil {
		value := *state.FiveHour.Limit
		cloned.FiveHour.Limit = &value
	}
	if state.FiveHour.ResetAt != nil {
		value := state.FiveHour.ResetAt.UTC()
		cloned.FiveHour.ResetAt = &value
	}
	if state.Weekly.Remaining != nil {
		value := *state.Weekly.Remaining
		cloned.Weekly.Remaining = &value
	}
	if state.Weekly.Limit != nil {
		value := *state.Weekly.Limit
		cloned.Weekly.Limit = &value
	}
	if state.Weekly.ResetAt != nil {
		value := state.Weekly.ResetAt.UTC()
		cloned.Weekly.ResetAt = &value
	}
	if state.LastRefreshAt != nil {
		value := state.LastRefreshAt.UTC()
		cloned.LastRefreshAt = &value
	}
	if state.ProbeResetAt != nil {
		value := state.ProbeResetAt.UTC()
		cloned.ProbeResetAt = &value
	}
	if state.ProbeAt != nil {
		value := state.ProbeAt.UTC()
		cloned.ProbeAt = &value
	}
	if state.ProbeVerifiedAt != nil {
		value := state.ProbeVerifiedAt.UTC()
		cloned.ProbeVerifiedAt = &value
	}
	if state.BootstrapProbeAt != nil {
		value := state.BootstrapProbeAt.UTC()
		cloned.BootstrapProbeAt = &value
	}
	if state.BootstrapVerifiedAt != nil {
		value := state.BootstrapVerifiedAt.UTC()
		cloned.BootstrapVerifiedAt = &value
	}
	if state.BootstrapNextAfter != nil {
		value := state.BootstrapNextAfter.UTC()
		cloned.BootstrapNextAfter = &value
	}
	return cloned
}

func firstFloatPath(body []byte, paths ...string) (float64, bool) {
	for _, path := range paths {
		result := gjson.GetBytes(body, path)
		if !result.Exists() {
			continue
		}
		switch result.Type {
		case gjson.Number:
			return result.Float(), true
		case gjson.String:
			if value, ok := cliproxyauthFloatString(result.String()); ok {
				return value, true
			}
		}
	}
	return 0, false
}

func firstTimePath(body []byte, paths ...string) (time.Time, bool) {
	for _, path := range paths {
		result := gjson.GetBytes(body, path)
		if !result.Exists() {
			continue
		}
		switch result.Type {
		case gjson.Number:
			unix := result.Int()
			if unix > 0 {
				return time.Unix(unix, 0).UTC(), true
			}
		case gjson.String:
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(result.String())); err == nil {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func firstQuotaResetPath(body []byte, now time.Time, paths ...string) (time.Time, bool) {
	absolutePaths := make([]string, 0, len(paths))
	for _, path := range paths {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasSuffix(lower, "seconds") {
			result := gjson.GetBytes(body, trimmed)
			if !result.Exists() {
				continue
			}
			seconds := 0.0
			switch result.Type {
			case gjson.Number:
				seconds = result.Float()
			case gjson.String:
				if value, ok := cliproxyauthFloatString(result.String()); ok {
					seconds = value
				}
			}
			if seconds > 0 {
				return now.Add(time.Duration(seconds * float64(time.Second))).UTC(), true
			}
			continue
		}
		absolutePaths = append(absolutePaths, trimmed)
	}
	return firstTimePath(body, absolutePaths...)
}

func firstStringPath(body []byte, paths ...string) (string, bool) {
	for _, path := range paths {
		result := gjson.GetBytes(body, path)
		if !result.Exists() || result.Type != gjson.String {
			continue
		}
		value := strings.TrimSpace(result.String())
		if value != "" {
			return value, true
		}
	}
	return "", false
}

func cliproxyauthFloatString(raw string) (float64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	result := gjson.Parse(trimmed)
	if result.Type != gjson.Number {
		return 0, false
	}
	return result.Float(), true
}
