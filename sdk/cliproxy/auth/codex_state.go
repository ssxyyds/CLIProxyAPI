package auth

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	CodexQuotaMetadataKey               = "codex_quota"
	CodexManualScoreAdjustmentKey       = "codex_manual_score_adjustment"
	CodexComputedScoreMetadataKey       = "codex_computed_score"
	CodexScoreReasonMetadataKey         = "codex_score_reason"
	CodexLastSelectionReasonMetadataKey = "codex_last_selection_reason"
	CodexQuotaRefreshIntervalSecondsKey = "refresh_interval_seconds"
)

const CodexQuotaRefreshInterval = 15 * time.Minute
const CodexQuotaResetProbeWindow = 14 * time.Minute

type CodexQuotaBucket struct {
	Remaining *float64   `json:"remaining,omitempty"`
	Limit     *float64   `json:"limit,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

type CodexQuotaState struct {
	FiveHour        CodexQuotaBucket `json:"five_hour,omitempty"`
	Weekly          CodexQuotaBucket `json:"weekly,omitempty"`
	LastRefreshAt   *time.Time       `json:"last_refresh_at,omitempty"`
	RefreshStatus   string           `json:"refresh_status,omitempty"`
	RefreshError    string           `json:"refresh_error,omitempty"`
	ProbeResetAt    *time.Time       `json:"probe_reset_at,omitempty"`
	ProbeAt         *time.Time       `json:"probe_at,omitempty"`
	ProbeVerifiedAt *time.Time       `json:"probe_verified_at,omitempty"`
	ProbeStatus     string           `json:"probe_status,omitempty"`
	ProbeError      string           `json:"probe_error,omitempty"`
}

func (b CodexQuotaBucket) clone() CodexQuotaBucket {
	cloned := CodexQuotaBucket{}
	if b.Remaining != nil {
		cloned.Remaining = float64Ptr(*b.Remaining)
	}
	if b.Limit != nil {
		cloned.Limit = float64Ptr(*b.Limit)
	}
	if b.ResetAt != nil {
		resetAt := b.ResetAt.UTC()
		cloned.ResetAt = &resetAt
	}
	return cloned
}

func (s CodexQuotaState) clone() CodexQuotaState {
	cloned := CodexQuotaState{
		FiveHour:      s.FiveHour.clone(),
		Weekly:        s.Weekly.clone(),
		RefreshStatus: s.RefreshStatus,
		RefreshError:  s.RefreshError,
		ProbeStatus:   s.ProbeStatus,
		ProbeError:    s.ProbeError,
	}
	if s.LastRefreshAt != nil {
		lastRefresh := s.LastRefreshAt.UTC()
		cloned.LastRefreshAt = &lastRefresh
	}
	if s.ProbeResetAt != nil {
		probeResetAt := s.ProbeResetAt.UTC()
		cloned.ProbeResetAt = &probeResetAt
	}
	if s.ProbeAt != nil {
		probeAt := s.ProbeAt.UTC()
		cloned.ProbeAt = &probeAt
	}
	if s.ProbeVerifiedAt != nil {
		probeVerifiedAt := s.ProbeVerifiedAt.UTC()
		cloned.ProbeVerifiedAt = &probeVerifiedAt
	}
	return cloned
}

func (a *Auth) GetCodexQuotaState() (CodexQuotaState, bool) {
	if a == nil || a.Metadata == nil {
		return CodexQuotaState{}, false
	}
	state, ok := codexQuotaStateFromAny(a.Metadata[CodexQuotaMetadataKey])
	if ok {
		a.Metadata[CodexQuotaMetadataKey] = state.metadataValue()
	} else {
		delete(a.Metadata, CodexQuotaMetadataKey)
	}
	return state, ok
}

func (a *Auth) SetCodexQuotaState(state CodexQuotaState) {
	if a == nil {
		return
	}
	a.ensureMetadata()
	a.Metadata[CodexQuotaMetadataKey] = state.metadataValue()
	if a.Metadata[CodexQuotaMetadataKey] == nil {
		delete(a.Metadata, CodexQuotaMetadataKey)
	}
}

func (a *Auth) CodexManualScoreAdjustment() (float64, bool) {
	return a.readCodexFloat(CodexManualScoreAdjustmentKey)
}

func (a *Auth) SetCodexManualScoreAdjustment(value float64) {
	a.writeCodexFloat(CodexManualScoreAdjustmentKey, value)
}

func (a *Auth) CodexComputedScore() (float64, bool) {
	return a.readCodexFloat(CodexComputedScoreMetadataKey)
}

func (a *Auth) SetCodexComputedScore(value float64) {
	a.writeCodexFloat(CodexComputedScoreMetadataKey, value)
}

func (a *Auth) CodexScoreReason() string {
	return a.readCodexString(CodexScoreReasonMetadataKey)
}

func (a *Auth) SetCodexScoreReason(reason string) {
	a.writeCodexString(CodexScoreReasonMetadataKey, reason)
}

func (a *Auth) CodexLastSelectionReason() string {
	return a.readCodexString(CodexLastSelectionReasonMetadataKey)
}

func (a *Auth) SetCodexLastSelectionReason(reason string) {
	a.writeCodexString(CodexLastSelectionReasonMetadataKey, reason)
}

func IsCodexOAuthLikeAuth(a *Auth) bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		return false
	}
	kind, _ := a.AccountInfo()
	return !strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func EnsureCodexQuotaRefreshMetadata(a *Auth) {
	if !IsCodexOAuthLikeAuth(a) {
		return
	}
	a.ensureMetadata()
	if authPreferredInterval(a) <= 0 {
		a.Metadata[CodexQuotaRefreshIntervalSecondsKey] = int(CodexQuotaRefreshInterval / time.Second)
	}
}

func ApplyCodexQuotaBlockedUntil(a *Auth, blockedUntil *time.Time) {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		return
	}
	if blockedUntil != nil && !blockedUntil.IsZero() {
		until := blockedUntil.UTC()
		a.Unavailable = true
		a.NextRetryAfter = until
		a.Quota.Exceeded = true
		a.Quota.Reason = "quota"
		a.Quota.NextRecoverAt = until
		return
	}
	shouldClearCooldown := a.Quota.Exceeded || strings.EqualFold(strings.TrimSpace(a.Quota.Reason), "quota") || !a.Quota.NextRecoverAt.IsZero()
	if shouldClearCooldown {
		a.Quota.Exceeded = false
		a.Quota.Reason = ""
		a.Quota.NextRecoverAt = time.Time{}
		a.Quota.BackoffLevel = 0
	}
	if shouldClearCooldown && a.Unavailable && !a.NextRetryAfter.IsZero() {
		a.Unavailable = false
		a.NextRetryAfter = time.Time{}
	}
}

func (a *Auth) ensureMetadata() {
	if a != nil && a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
}

func (a *Auth) readCodexFloat(key string) (float64, bool) {
	if a == nil || a.Metadata == nil {
		return 0, false
	}
	parsed, ok := parseFloatAny(a.Metadata[key])
	if !ok {
		delete(a.Metadata, key)
		return 0, false
	}
	a.Metadata[key] = parsed
	return parsed, true
}

func (a *Auth) writeCodexFloat(key string, value float64) {
	if a == nil {
		return
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	a.ensureMetadata()
	a.Metadata[key] = value
}

func (a *Auth) readCodexString(key string) string {
	if a == nil || a.Metadata == nil {
		return ""
	}
	val, _ := a.Metadata[key].(string)
	return strings.TrimSpace(val)
}

func (a *Auth) writeCodexString(key, value string) {
	if a == nil {
		return
	}
	trimmed := strings.TrimSpace(value)
	a.ensureMetadata()
	if trimmed == "" {
		delete(a.Metadata, key)
		return
	}
	a.Metadata[key] = trimmed
}

func codexQuotaStateFromAny(raw any) (CodexQuotaState, bool) {
	switch typed := raw.(type) {
	case nil:
		return CodexQuotaState{}, false
	case CodexQuotaState:
		return typed, typed.hasData()
	case *CodexQuotaState:
		if typed == nil {
			return CodexQuotaState{}, false
		}
		return *typed, typed.hasData()
	case map[string]any:
		return codexQuotaStateFromMap(typed)
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for k, v := range typed {
			converted[k] = v
		}
		return codexQuotaStateFromMap(converted)
	default:
		return CodexQuotaState{}, false
	}
}

func codexQuotaStateFromMap(raw map[string]any) (CodexQuotaState, bool) {
	if raw == nil {
		return CodexQuotaState{}, false
	}
	var state CodexQuotaState
	state.FiveHour = codexQuotaBucketFromAny(raw["five_hour"])
	state.Weekly = codexQuotaBucketFromAny(raw["weekly"])
	if ts, ok := parseTimeValue(raw["last_refresh_at"]); ok && !ts.IsZero() {
		state.LastRefreshAt = &ts
	}
	if status, ok := raw["refresh_status"].(string); ok {
		state.RefreshStatus = strings.TrimSpace(status)
	}
	if refreshErr, ok := raw["refresh_error"].(string); ok {
		state.RefreshError = strings.TrimSpace(refreshErr)
	}
	if ts, ok := parseTimeValue(raw["probe_reset_at"]); ok && !ts.IsZero() {
		state.ProbeResetAt = &ts
	}
	if ts, ok := parseTimeValue(raw["probe_at"]); ok && !ts.IsZero() {
		state.ProbeAt = &ts
	}
	if ts, ok := parseTimeValue(raw["probe_verified_at"]); ok && !ts.IsZero() {
		state.ProbeVerifiedAt = &ts
	}
	if status, ok := raw["probe_status"].(string); ok {
		state.ProbeStatus = strings.TrimSpace(status)
	}
	if probeErr, ok := raw["probe_error"].(string); ok {
		state.ProbeError = strings.TrimSpace(probeErr)
	}
	return state, state.hasData()
}

func codexQuotaBucketFromAny(raw any) CodexQuotaBucket {
	switch typed := raw.(type) {
	case CodexQuotaBucket:
		return typed
	case *CodexQuotaBucket:
		if typed == nil {
			return CodexQuotaBucket{}
		}
		return *typed
	case map[string]any:
		return codexQuotaBucketFromMap(typed)
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for k, v := range typed {
			converted[k] = v
		}
		return codexQuotaBucketFromMap(converted)
	default:
		return CodexQuotaBucket{}
	}
}

func codexQuotaBucketFromMap(raw map[string]any) CodexQuotaBucket {
	var bucket CodexQuotaBucket
	if raw == nil {
		return bucket
	}
	if remaining, ok := parseFloatAny(raw["remaining"]); ok {
		bucket.Remaining = float64Ptr(remaining)
	}
	if limit, ok := parseFloatAny(raw["limit"]); ok {
		bucket.Limit = float64Ptr(limit)
	}
	if ts, ok := parseTimeValue(raw["reset_at"]); ok && !ts.IsZero() {
		bucket.ResetAt = &ts
	}
	return bucket
}

func (s CodexQuotaState) hasData() bool {
	return s.FiveHour.hasData() || s.Weekly.hasData() || s.LastRefreshAt != nil || s.RefreshStatus != "" || s.RefreshError != "" || s.ProbeResetAt != nil || s.ProbeAt != nil || s.ProbeVerifiedAt != nil || s.ProbeStatus != "" || s.ProbeError != ""
}

func (b CodexQuotaBucket) hasData() bool {
	return b.Remaining != nil || b.Limit != nil || b.ResetAt != nil
}

func (s CodexQuotaState) metadataValue() any {
	if !s.hasData() {
		return nil
	}
	out := map[string]any{}
	if bucket := s.FiveHour.metadataValue(); bucket != nil {
		out["five_hour"] = bucket
	}
	if bucket := s.Weekly.metadataValue(); bucket != nil {
		out["weekly"] = bucket
	}
	if s.LastRefreshAt != nil && !s.LastRefreshAt.IsZero() {
		out["last_refresh_at"] = s.LastRefreshAt.UTC().Format(time.RFC3339)
	}
	if trimmed := strings.TrimSpace(s.RefreshStatus); trimmed != "" {
		out["refresh_status"] = trimmed
	}
	if trimmed := strings.TrimSpace(s.RefreshError); trimmed != "" {
		out["refresh_error"] = trimmed
	}
	if s.ProbeResetAt != nil && !s.ProbeResetAt.IsZero() {
		out["probe_reset_at"] = s.ProbeResetAt.UTC().Format(time.RFC3339)
	}
	if s.ProbeAt != nil && !s.ProbeAt.IsZero() {
		out["probe_at"] = s.ProbeAt.UTC().Format(time.RFC3339)
	}
	if s.ProbeVerifiedAt != nil && !s.ProbeVerifiedAt.IsZero() {
		out["probe_verified_at"] = s.ProbeVerifiedAt.UTC().Format(time.RFC3339)
	}
	if trimmed := strings.TrimSpace(s.ProbeStatus); trimmed != "" {
		out["probe_status"] = trimmed
	}
	if trimmed := strings.TrimSpace(s.ProbeError); trimmed != "" {
		out["probe_error"] = trimmed
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s CodexQuotaState) CodexProbeEligibleResetAt(now time.Time) (*time.Time, bool) {
	for _, resetAt := range s.codexProbeCandidateResetAts(now) {
		if codexProbeAnchorDistance(now, resetAt) > CodexQuotaResetProbeWindow {
			continue
		}
		resetAtCopy := resetAt
		return &resetAtCopy, true
	}
	return nil, false
}

func (s CodexQuotaState) CodexProbeWindowResetAt(now time.Time) (*time.Time, bool) {
	for _, resetAt := range s.codexProbeCandidateResetAts(now) {
		if codexProbeAnchorDistance(now, resetAt) > CodexQuotaResetProbeWindow {
			continue
		}
		resetAtCopy := resetAt
		return &resetAtCopy, true
	}
	return nil, false
}

func (s CodexQuotaState) CodexProbeVerifiedForReset(resetAt time.Time) bool {
	status := strings.ToLower(strings.TrimSpace(s.ProbeStatus))
	if status != "verified" && status != "success" && status != "ok" {
		return false
	}
	if s.ProbeResetAt == nil || s.ProbeVerifiedAt == nil || s.ProbeResetAt.IsZero() || s.ProbeVerifiedAt.IsZero() {
		return false
	}
	return s.ProbeResetAt.UTC().Equal(resetAt.UTC())
}

func (s CodexQuotaState) codexProbeCandidateResetAts(now time.Time) []time.Time {
	candidates := make([]time.Time, 0, 2)
	for _, resetAt := range []*time.Time{s.FiveHour.ResetAt, s.Weekly.ResetAt} {
		if resetAt == nil || resetAt.IsZero() {
			continue
		}
		candidates = append(candidates, resetAt.UTC())
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		di := codexProbeAnchorDistance(now, candidates[i])
		dj := codexProbeAnchorDistance(now, candidates[j])
		if di == dj {
			return candidates[i].Before(candidates[j])
		}
		return di < dj
	})
	return candidates
}

func codexProbeAnchorDistance(now, resetAt time.Time) time.Duration {
	return absDuration(now.Sub(codexProbeAnchorTime(resetAt)))
}

func codexProbeAnchorTime(resetAt time.Time) time.Time {
	return resetAt.UTC().Add(-5 * time.Hour)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (b CodexQuotaBucket) metadataValue() map[string]any {
	out := map[string]any{}
	if b.Remaining != nil {
		out["remaining"] = *b.Remaining
	}
	if b.Limit != nil {
		out["limit"] = *b.Limit
	}
	if b.ResetAt != nil && !b.ResetAt.IsZero() {
		out["reset_at"] = b.ResetAt.UTC().Format(time.RFC3339)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseFloatAny(val any) (float64, bool) {
	switch typed := val.(type) {
	case float64:
		return sanitizeFiniteFloat(typed)
	case float32:
		return sanitizeFiniteFloat(float64(typed))
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return sanitizeFiniteFloat(parsed)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, false
		}
		return sanitizeFiniteFloat(parsed)
	default:
		return 0, false
	}
}

func sanitizeFiniteFloat(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func float64Ptr(v float64) *float64 {
	return &v
}

func ApplyCodexQuotaHeaderUpdate(auth *Auth, headers http.Header, now time.Time) bool {
	if auth == nil || !IsCodexOAuthLikeAuth(auth) || headers == nil {
		return false
	}
	update := extractCodexQuotaHeaderUpdate(headers, now)
	if !update.hasAnyData() {
		return false
	}
	state, _ := auth.GetCodexQuotaState()
	if update.FiveHour.hasData() {
		state.FiveHour = update.FiveHour
	}
	if update.Weekly.hasData() {
		state.Weekly = update.Weekly
	}
	state.LastRefreshAt = &now
	state.RefreshStatus = "ok"
	state.RefreshError = ""
	auth.SetCodexQuotaState(state)
	if !update.BlockedUntil.IsZero() {
		blockedUntil := update.BlockedUntil.UTC()
		ApplyCodexQuotaBlockedUntil(auth, &blockedUntil)
	}
	return true
}

type codexQuotaHeaderUpdate struct {
	FiveHour     CodexQuotaBucket
	Weekly       CodexQuotaBucket
	BlockedUntil time.Time
}

func (u codexQuotaHeaderUpdate) hasAnyData() bool {
	return u.FiveHour.hasData() || u.Weekly.hasData() || !u.BlockedUntil.IsZero()
}

func extractCodexQuotaHeaderUpdate(headers http.Header, now time.Time) codexQuotaHeaderUpdate {
	var update codexQuotaHeaderUpdate
	for rawName, values := range headers {
		if len(values) == 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(rawName))
		value := strings.TrimSpace(values[0])
		if value == "" {
			continue
		}
		if strings.HasPrefix(name, "x-codex-primary-") || strings.HasPrefix(name, "x-codex-secondary-") {
			bucket := &update.FiveHour
			if strings.HasPrefix(name, "x-codex-secondary-") {
				bucket = &update.Weekly
			}
			switch {
			case strings.HasSuffix(name, "used-percent"):
				if used, ok := parseHeaderNumber(value); ok {
					remaining := math.Max(0, 100-used)
					bucket.Limit = float64Ptr(100)
					bucket.Remaining = float64Ptr(remaining)
				}
			case strings.HasSuffix(name, "reset-at"):
				if ts, ok := parseHeaderTimestamp(value, now); ok {
					bucket.ResetAt = &ts
				}
			}
			continue
		}
		if strings.HasPrefix(name, "x-ratelimit-") {
			var bucket *CodexQuotaBucket
			switch {
			case strings.Contains(name, "5h"), strings.Contains(name, "5hr"), strings.Contains(name, "5hour"), strings.Contains(name, "5-hour"):
				bucket = &update.FiveHour
			case strings.Contains(name, "week"), strings.Contains(name, "weekly"), strings.Contains(name, "1w"), strings.Contains(name, "7d"):
				bucket = &update.Weekly
			}
			if bucket == nil {
				continue
			}
			switch {
			case strings.Contains(name, "limit"):
				if n, ok := parseHeaderNumber(value); ok {
					bucket.Limit = float64Ptr(n)
				}
			case strings.Contains(name, "remaining"):
				if n, ok := parseHeaderNumber(value); ok {
					bucket.Remaining = float64Ptr(n)
				}
			case strings.Contains(name, "reset"):
				if ts, ok := parseHeaderTimestamp(value, now); ok {
					bucket.ResetAt = &ts
				}
			}
			continue
		}
		if name == "retry-after" {
			if ts, ok := parseRetryAfterTimestamp(value, now); ok {
				update.BlockedUntil = ts
			}
		}
	}
	return update
}

func parseHeaderNumber(value string) (float64, bool) {
	match := strings.TrimSpace(value)
	if match == "" {
		return 0, false
	}
	for i, r := range match {
		if !(r == '-' || r == '+' || r == '.' || (r >= '0' && r <= '9')) {
			match = match[:i]
			break
		}
	}
	if match == "" || match == "+" || match == "-" || match == "." {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(match, 64)
	if err != nil {
		return 0, false
	}
	return sanitizeFiniteFloat(parsed)
}

func parseHeaderTimestamp(value string, now time.Time) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	if ts, ok := parseTimeValue(trimmed); ok && !ts.IsZero() {
		return ts.UTC(), true
	}
	if secs, ok := parseHeaderNumber(trimmed); ok && secs > 0 {
		if secs > 1e12 {
			return time.UnixMilli(int64(secs)).UTC(), true
		}
		if secs > 1e9 {
			return time.Unix(int64(secs), 0).UTC(), true
		}
		return now.Add(time.Duration(secs * float64(time.Second))).UTC(), true
	}
	return time.Time{}, false
}

func parseRetryAfterTimestamp(value string, now time.Time) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	if secs, err := strconv.ParseFloat(trimmed, 64); err == nil && secs >= 0 {
		return now.Add(time.Duration(secs * float64(time.Second))).UTC(), true
	}
	if ts, err := http.ParseTime(trimmed); err == nil {
		return ts.UTC(), true
	}
	return time.Time{}, false
}
