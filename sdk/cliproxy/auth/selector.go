package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
type RoundRobinSelector struct {
	mu      sync.Mutex
	cursors map[string]int
	maxKeys int
}

// FillFirstSelector selects the first available credential (deterministic ordering).
// This "burns" one account before moving to the next, which can help stagger
// rolling-window subscription caps (e.g. chat message limits).
type FillFirstSelector struct{}

// CodexQuotaScoreSelector applies Codex quota-aware ranking when every candidate
// in the active slice is a Codex auth. Otherwise it falls back to round-robin.
type CodexQuotaScoreSelector struct {
	fallback FillFirstSelector
	sticky   *codexStickySelectionState
}

type codexStickySelectionState struct {
	mu         sync.Mutex
	byKey      map[string]string
	byProvider map[string]string
}

var globalCodexStickySelection = &codexStickySelectionState{byKey: map[string]string{}, byProvider: map[string]string{}}

const codexQuotaScoreFreshnessWindow = 15 * time.Minute
const codexQuotaScoreTransientRefreshErrorGrace = CodexQuotaRefreshInterval * 4

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model    string
	resetIn  time.Duration
	provider string
}

func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
	}
}

func (e *modelCooldownError) Error() string {
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

func authPriority(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return parsed
}

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

func authWebsocketsEnabled(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func preferCodexWebsocketAuths(ctx context.Context, provider string, available []*Auth) []*Auth {
	if len(available) == 0 {
		return available
	}
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return available
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return available
	}

	wsEnabled := make([]*Auth, 0, len(available))
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		if authWebsocketsEnabled(candidate) {
			wsEnabled = append(wsEnabled, candidate)
		}
	}
	if len(wsEnabled) > 0 {
		return wsEnabled
	}
	return available
}

func collectAvailableByPriority(auths []*Auth, model string, now time.Time) (available map[int][]*Auth, cooldownCount int, earliest time.Time) {
	available = make(map[int][]*Auth)
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			priority := authPriority(candidate)
			available[priority] = append(available[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}
	return available, cooldownCount, earliest
}

func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, model, now)
	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}

	available := availableByPriority[bestPriority]
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

// Pick selects the next available auth for the provider in a round-robin manner.
// For gemini-cli virtual auths (identified by the gemini_virtual_parent attribute),
// a two-level round-robin is used: first cycling across credential groups (parent
// accounts), then cycling within each group's project auths.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	// Check if any available auth has gemini_virtual_parent attribute,
	// indicating gemini-cli virtual auths that should use credential-level polling.
	groups, parentOrder := groupByVirtualParent(available)
	if len(parentOrder) > 1 {
		// Two-level round-robin: first select a credential group, then pick within it.
		groupKey := key + "::group"
		s.ensureCursorKey(groupKey, limit)
		if _, exists := s.cursors[groupKey]; !exists {
			// Seed with a random initial offset so the starting credential is randomized.
			s.cursors[groupKey] = rand.IntN(len(parentOrder))
		}
		groupIndex := s.cursors[groupKey]
		if groupIndex >= 2_147_483_640 {
			groupIndex = 0
		}
		s.cursors[groupKey] = groupIndex + 1

		selectedParent := parentOrder[groupIndex%len(parentOrder)]
		group := groups[selectedParent]

		// Second level: round-robin within the selected credential group.
		innerKey := key + "::cred:" + selectedParent
		s.ensureCursorKey(innerKey, limit)
		innerIndex := s.cursors[innerKey]
		if innerIndex >= 2_147_483_640 {
			innerIndex = 0
		}
		s.cursors[innerKey] = innerIndex + 1
		s.mu.Unlock()
		return group[innerIndex%len(group)], nil
	}

	// Flat round-robin for non-grouped auths (original behavior).
	s.ensureCursorKey(key, limit)
	index := s.cursors[key]
	if index >= 2_147_483_640 {
		index = 0
	}
	s.cursors[key] = index + 1
	s.mu.Unlock()
	return available[index%len(available)], nil
}

// ensureCursorKey ensures the cursor map has capacity for the given key.
// Must be called with s.mu held.
func (s *RoundRobinSelector) ensureCursorKey(key string, limit int) {
	if _, ok := s.cursors[key]; !ok && len(s.cursors) >= limit {
		s.cursors = make(map[string]int)
	}
}

// groupByVirtualParent groups auths by their gemini_virtual_parent attribute.
// Returns a map of parentID -> auths and a sorted slice of parent IDs for stable iteration.
// Only auths with a non-empty gemini_virtual_parent are grouped; if any auth lacks
// this attribute, nil/nil is returned so the caller falls back to flat round-robin.
func groupByVirtualParent(auths []*Auth) (map[string][]*Auth, []string) {
	if len(auths) == 0 {
		return nil, nil
	}
	groups := make(map[string][]*Auth)
	for _, a := range auths {
		parent := ""
		if a.Attributes != nil {
			parent = strings.TrimSpace(a.Attributes["gemini_virtual_parent"])
		}
		if parent == "" {
			// Non-virtual auth present; fall back to flat round-robin.
			return nil, nil
		}
		groups[parent] = append(groups[parent], a)
	}
	// Collect parent IDs in sorted order for stable cursor indexing.
	parentOrder := make([]string, 0, len(groups))
	for p := range groups {
		parentOrder = append(parentOrder, p)
	}
	sort.Strings(parentOrder)
	return groups, parentOrder
}

// Pick selects the first available auth for the provider in a deterministic manner.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	return available[0], nil
}

func (s *CodexQuotaScoreSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if canUseCodexQuotaScoreSelection(provider, available) {
		scoreable, exhaustedCount, earliestReset := codexQuotaScoreFilterFiveHourExhausted(available, now)
		if len(scoreable) == 0 && exhaustedCount == len(available) {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := time.Duration(0)
			if !earliestReset.IsZero() {
				resetIn = earliestReset.Sub(now)
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		if picked := pickStickyOrBestCodexQuotaScoreAuth(s.stickyState(), provider, model, scoreable, now); picked != nil {
			return picked, nil
		}
	}
	return s.fallback.Pick(ctx, provider, model, opts, auths)
}

func (s *CodexQuotaScoreSelector) stickyState() *codexStickySelectionState {
	if s == nil || s.sticky == nil {
		return globalCodexStickySelection
	}
	return s.sticky
}

type codexQuotaScoreSnapshot struct {
	auth                   *Auth
	disqualified           bool
	hasKnownScore          bool
	finalScore             float64
	hasWeeklyRemaining     bool
	weeklyRemaining        float64
	hasWeeklyPct           bool
	weeklyPct              float64
	hasUsableFiveHourReset bool
	fiveHourReset          time.Time
}

func canUseCodexQuotaScoreSelection(provider string, auths []*Auth) bool {
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if providerKey != "codex" && providerKey != "mixed" {
		return false
	}
	if len(auths) == 0 {
		return false
	}
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			return false
		}
	}
	return true
}

func codexQuotaScoreFilterFiveHourExhausted(auths []*Auth, now time.Time) ([]*Auth, int, time.Time) {
	filtered := make([]*Auth, 0, len(auths))
	exhaustedCount := 0
	var earliestReset time.Time
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		quota, ok := auth.GetCodexQuotaState()
		if ok && codexFiveHourQuotaExhausted(quota.FiveHour, now) {
			exhaustedCount++
			if quota.FiveHour.ResetAt != nil && !quota.FiveHour.ResetAt.IsZero() {
				reset := quota.FiveHour.ResetAt.UTC()
				if earliestReset.IsZero() || reset.Before(earliestReset) {
					earliestReset = reset
				}
			}
			continue
		}
		filtered = append(filtered, auth)
	}
	return filtered, exhaustedCount, earliestReset
}

func pickBestCodexQuotaScoreAuth(auths []*Auth, now time.Time) *Auth {
	if len(auths) == 0 {
		return nil
	}
	snapshots := make([]codexQuotaScoreSnapshot, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		snapshots = append(snapshots, codexQuotaScoreSnapshotForAuth(auth, now))
	}
	if len(snapshots) == 0 {
		return nil
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return codexQuotaScoreLess(snapshots[i], snapshots[j])
	})
	return snapshots[0].auth
}

func pickBestRetainableCodexQuotaScoreAuth(auths []*Auth, now time.Time) *Auth {
	filtered := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || !codexStickyRetainable(auth, now) {
			continue
		}
		filtered = append(filtered, auth)
	}
	if len(filtered) == 0 {
		return nil
	}
	return pickBestCodexQuotaScoreAuth(filtered, now)
}

func pickStickyOrBestCodexQuotaScoreAuth(state *codexStickySelectionState, provider, model string, auths []*Auth, now time.Time) *Auth {
	if len(auths) == 0 {
		return nil
	}
	if state == nil {
		state = globalCodexStickySelection
	}
	key := codexStickySelectionKey(provider, model)
	if picked := state.pickRetained(key, auths, now); picked != nil {
		return picked
	}
	picked := pickBestCodexQuotaScoreAuth(auths, now)
	if picked == nil {
		state.clear(key)
		return nil
	}
	if codexStickyRetainable(picked, now) {
		state.set(key, picked.ID)
	} else {
		state.clear(key)
	}
	return picked
}

func codexStickySelectionKey(provider, model string) string {
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if providerKey == "" {
		providerKey = "codex"
	}
	return providerKey + ":" + canonicalModelKey(model)
}

func (s *codexStickySelectionState) pickRetained(key string, auths []*Auth, now time.Time) *Auth {
	if s == nil {
		return nil
	}
	providerKey := providerFromStickySelectionKey(key)
	providerRoot := strings.SplitN(strings.TrimSpace(key), ":", 2)[0] + ":"
	s.mu.Lock()
	stickyID := s.byKey[key]
	if strings.TrimSpace(stickyID) == "" {
		stickyID = s.byKey[providerRoot]
	}
	s.mu.Unlock()
	if strings.TrimSpace(stickyID) == "" {
		return nil
	}
	for _, auth := range auths {
		if auth == nil || auth.ID != stickyID {
			continue
		}
		if codexStickyRetainable(auth, now) {
			if providerKey != "" {
				s.mu.Lock()
				if s.byProvider == nil {
					s.byProvider = map[string]string{}
				}
				s.byProvider[providerKey] = auth.ID
				s.mu.Unlock()
			}
			return auth
		}
		break
	}
	s.clear(key)
	return nil
}

func (s *codexStickySelectionState) set(key, authID string) {
	if s == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(authID) == "" {
		return
	}
	providerKey := providerFromStickySelectionKey(key)
	s.mu.Lock()
	if s.byKey == nil {
		s.byKey = map[string]string{}
	}
	if s.byProvider == nil {
		s.byProvider = map[string]string{}
	}
	s.byKey[key] = authID
	if providerKey != "" {
		s.byProvider[providerKey] = authID
	}
	s.mu.Unlock()
}

func (s *codexStickySelectionState) clearProvider(provider string) {
	if s == nil {
		return
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if providerKey == "" {
		return
	}
	prefix := providerKey + ":"
	s.mu.Lock()
	for key := range s.byKey {
		if strings.HasPrefix(key, prefix) {
			delete(s.byKey, key)
		}
	}
	delete(s.byProvider, providerKey)
	s.mu.Unlock()
}

func (s *codexStickySelectionState) clearAuth(authID string) {
	if s == nil || strings.TrimSpace(authID) == "" {
		return
	}
	targetAuthID := strings.TrimSpace(authID)
	s.mu.Lock()
	for key, existingAuthID := range s.byKey {
		if strings.TrimSpace(existingAuthID) == targetAuthID {
			delete(s.byKey, key)
		}
	}
	for providerKey, existingAuthID := range s.byProvider {
		if strings.TrimSpace(existingAuthID) == targetAuthID {
			delete(s.byProvider, providerKey)
		}
	}
	s.mu.Unlock()
}

func (s *codexStickySelectionState) clear(key string) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	providerKey := providerFromStickySelectionKey(key)
	s.mu.Lock()
	clearedAuthID := strings.TrimSpace(s.byKey[key])
	delete(s.byKey, key)
	if providerKey != "" && strings.TrimSpace(s.byProvider[providerKey]) == clearedAuthID {
		providerRoot := providerKey + ":"
		next := strings.TrimSpace(s.byKey[providerRoot])
		if next == "" {
			for existingKey, authID := range s.byKey {
				if strings.HasPrefix(existingKey, providerRoot) && strings.TrimSpace(authID) != "" {
					next = strings.TrimSpace(authID)
					break
				}
			}
		}
		if next == "" {
			delete(s.byProvider, providerKey)
		} else {
			s.byProvider[providerKey] = next
		}
	}
	s.mu.Unlock()
}

func (s *codexStickySelectionState) currentAuthIDForProvider(provider string) string {
	if s == nil {
		return ""
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if providerKey == "" {
		providerKey = "codex"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.byProvider[providerKey])
}

func (s *codexStickySelectionState) currentSelectionsForProvider(provider string) map[string]string {
	selections := map[string]string{}
	if s == nil {
		return selections
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	if providerKey == "" {
		providerKey = "codex"
	}
	prefix := providerKey + ":"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, authID := range s.byKey {
		if !strings.HasPrefix(key, prefix) || strings.TrimSpace(authID) == "" {
			continue
		}
		model := strings.TrimPrefix(key, prefix)
		selections[model] = strings.TrimSpace(authID)
	}
	return selections
}

func providerFromStickySelectionKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	parts := strings.SplitN(key, ":", 2)
	return strings.ToLower(strings.TrimSpace(parts[0]))
}

func CurrentCodexStickyAuthID() string {
	return globalCodexStickySelection.currentAuthIDForProvider("codex")
}

func CurrentCodexStickySelections() map[string]string {
	return globalCodexStickySelection.currentSelectionsForProvider("codex")
}

func ReleaseCodexStickyAuth(authID string) {
	globalCodexStickySelection.clearAuth(authID)
}

func RecalculateCurrentCodexStickyAuth(auths []*Auth, now time.Time) *Auth {
	key := codexStickySelectionKey("codex", "")
	globalCodexStickySelection.clearProvider("codex")
	filtered := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || !IsCodexOAuthLikeAuth(auth) {
			continue
		}
		filtered = append(filtered, auth)
	}
	picked := pickBestRetainableCodexQuotaScoreAuth(filtered, now)
	if picked == nil {
		return nil
	}
	globalCodexStickySelection.set(key, picked.ID)
	return picked
}

func codexStickyRetainable(auth *Auth, now time.Time) bool {
	if auth == nil || !IsCodexOAuthLikeAuth(auth) {
		return false
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "", now); blocked {
		return false
	}
	quota, ok := auth.GetCodexQuotaState()
	if !ok || !codexQuotaRefreshStateUsable(quota, now) {
		return false
	}
	if quota.FiveHour.Remaining == nil {
		return false
	}
	return *quota.FiveHour.Remaining > 0
}

func codexQuotaScoreSnapshotForAuth(auth *Auth, now time.Time) codexQuotaScoreSnapshot {
	snapshot := codexQuotaScoreSnapshot{auth: auth}
	if auth == nil {
		return snapshot
	}
	explanation := BuildCodexScoreExplanation(auth, now)
	if explanation.DisqualifierReason != "" {
		snapshot.disqualified = true
	}
	quota, ok := auth.GetCodexQuotaState()
	if !ok {
		return snapshot
	}
	if explanation.WeeklyRemaining != nil {
		snapshot.hasWeeklyRemaining = true
		snapshot.weeklyRemaining = *explanation.WeeklyRemaining
	}
	if explanation.WeeklyRemaining != nil && explanation.WeeklyLimit != nil && *explanation.WeeklyLimit > 0 {
		snapshot.hasWeeklyPct = true
		snapshot.weeklyPct = *explanation.WeeklyRemaining / *explanation.WeeklyLimit
	}
	if quota.FiveHour.ResetAt != nil && quota.FiveHour.ResetAt.After(now) {
		snapshot.hasUsableFiveHourReset = true
		snapshot.fiveHourReset = quota.FiveHour.ResetAt.UTC()
	}
	if explanation.ScoreAvailable && explanation.ComputedScoreLive != nil {
		snapshot.finalScore = *explanation.ComputedScoreLive
		snapshot.hasKnownScore = true
	}
	return snapshot
}

func codexQuotaRefreshStateUsable(quota CodexQuotaState, now time.Time) bool {
	if quota.LastRefreshAt == nil || quota.LastRefreshAt.IsZero() {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(quota.RefreshStatus))
	switch status {
	case "", "ok", "success", "fresh":
		if quota.LastRefreshAt.Before(now.Add(-codexQuotaScoreFreshnessWindow)) {
			return false
		}
		return true
	case "error", "failed":
		if codexQuotaRefreshErrorIsHardAuthFailure(quota.RefreshError) {
			return false
		}
		if !codexQuotaRefreshErrorIsTransient(quota.RefreshError) {
			return false
		}
		return !quota.LastRefreshAt.Before(now.Add(-codexQuotaScoreTransientRefreshErrorGrace))
	default:
		return false
	}
}

func codexQuotaScoreLess(left, right codexQuotaScoreSnapshot) bool {
	if left.disqualified != right.disqualified {
		return !left.disqualified
	}
	if left.hasKnownScore != right.hasKnownScore {
		return left.hasKnownScore
	}
	if left.hasKnownScore && right.hasKnownScore && left.finalScore != right.finalScore {
		return left.finalScore > right.finalScore
	}
	if left.hasWeeklyRemaining != right.hasWeeklyRemaining {
		return left.hasWeeklyRemaining
	}
	if left.hasWeeklyRemaining && right.hasWeeklyRemaining && left.weeklyRemaining != right.weeklyRemaining {
		return left.weeklyRemaining > right.weeklyRemaining
	}
	if left.hasWeeklyPct != right.hasWeeklyPct {
		return left.hasWeeklyPct
	}
	if left.hasWeeklyPct && right.hasWeeklyPct && left.weeklyPct != right.weeklyPct {
		return left.weeklyPct > right.weeklyPct
	}
	if left.hasUsableFiveHourReset != right.hasUsableFiveHourReset {
		return left.hasUsableFiveHourReset
	}
	if left.hasUsableFiveHourReset && right.hasUsableFiveHourReset && !left.fiveHourReset.Equal(right.fiveHourReset) {
		return left.fiveHourReset.Before(right.fiveHourReset)
	}
	leftID := ""
	rightID := ""
	if left.auth != nil {
		leftID = left.auth.ID
	}
	if right.auth != nil {
		rightID = right.auth.ID
	}
	return leftID < rightID
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, ok := auth.ModelStates[model]
			if (!ok || state == nil) && model != "" {
				baseModel := canonicalModelKey(model)
				if baseModel != "" && baseModel != model {
					state, ok = auth.ModelStates[baseModel]
				}
			}
			if ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
			}
		}
		return false, blockReasonNone, time.Time{}
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		next := auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if auth.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

// sessionPattern matches Claude Code user_id format:
// user_{hash}_account__session_{uuid}
var sessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// SessionAffinitySelector wraps another selector with session-sticky behavior.
// It extracts session ID from multiple sources and maintains session-to-auth
// mappings with automatic failover when the bound auth becomes unavailable.
type SessionAffinitySelector struct {
	fallback Selector
	cache    *SessionCache
}

// SessionAffinityConfig configures the session affinity selector.
type SessionAffinityConfig struct {
	Fallback Selector
	TTL      time.Duration
}

// NewSessionAffinitySelector creates a new session-aware selector.
func NewSessionAffinitySelector(fallback Selector) *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: fallback,
		TTL:      time.Hour,
	})
}

// NewSessionAffinitySelectorWithConfig creates a selector with custom configuration.
func NewSessionAffinitySelectorWithConfig(cfg SessionAffinityConfig) *SessionAffinitySelector {
	if cfg.Fallback == nil {
		cfg.Fallback = &RoundRobinSelector{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = time.Hour
	}
	return &SessionAffinitySelector{
		fallback: cfg.Fallback,
		cache:    NewSessionCache(cfg.TTL),
	}
}

// Pick selects an auth with session affinity when possible.
// Priority for session ID extraction:
//  1. metadata.user_id (Claude Code format with _session_{uuid}) - highest priority
//  2. X-Session-ID header
//  3. Session_id header (Codex)
//  4. X-Amp-Thread-Id header (Amp CLI thread ID)
//  5. X-Client-Request-Id header (PI)
//  6. metadata.user_id (non-Claude Code format)
//  7. conversation_id field in request body
//  8. Stable hash from first few messages content (fallback)
//
// Note: The cache key includes provider, session ID, and model to handle cases where
// a session uses multiple models (e.g., gemini-2.5-pro and gemini-3-flash-preview)
// that may be supported by different auth credentials, and to avoid cross-provider conflicts.
func (s *SessionAffinitySelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	entry := selectorLogEntry(ctx)
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		entry.Debugf("session-affinity: no session ID extracted, falling back to default selector | provider=%s model=%s", provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, auths)
	}

	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}

	cacheKey := provider + "::" + primaryID + "::" + model

	if cachedAuthID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		for _, auth := range available {
			if auth.ID == cachedAuthID {
				entry.Infof("session-affinity: cache hit | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
				return auth, nil
			}
		}
		// Cached auth not available, reselect via fallback selector for even distribution
		auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
		if err != nil {
			return nil, err
		}
		s.cache.Set(cacheKey, auth.ID)
		entry.Infof("session-affinity: cache hit but auth unavailable, reselected | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
		return auth, nil
	}

	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey := provider + "::" + fallbackID + "::" + model
		if cachedAuthID, ok := s.cache.Get(fallbackKey); ok {
			for _, auth := range available {
				if auth.ID == cachedAuthID {
					s.cache.Set(cacheKey, auth.ID)
					entry.Infof("session-affinity: fallback cache hit | session=%s fallback=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), auth.ID, provider, model)
					return auth, nil
				}
			}
		}
	}

	auth, err := s.fallback.Pick(ctx, provider, model, opts, auths)
	if err != nil {
		return nil, err
	}
	s.cache.Set(cacheKey, auth.ID)
	entry.Infof("session-affinity: cache miss, new binding | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), auth.ID, provider, model)
	return auth, nil
}

func selectorLogEntry(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

// truncateSessionID shortens session ID for logging (first 8 chars + "...")
func truncateSessionID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return id[:8] + "..."
}

// Stop releases resources held by the selector.
func (s *SessionAffinitySelector) Stop() {
	if s.cache != nil {
		s.cache.Stop()
	}
}

// InvalidateAuth removes all session bindings for a specific auth.
// Called when an auth becomes rate-limited or unavailable.
func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s.cache != nil {
		s.cache.InvalidateAuth(authID)
	}
}

// ExtractSessionID extracts session identifier from multiple sources.
// Priority order:
//  1. metadata.user_id (Claude Code format with _session_{uuid}) - highest priority for Claude Code clients
//  2. X-Session-ID header
//  3. Session_id header (Codex)
//  4. X-Amp-Thread-Id header (Amp CLI thread ID)
//  5. X-Client-Request-Id header (PI)
//  6. metadata.user_id (non-Claude Code format)
//  7. conversation_id field in request body
//  8. Stable hash from first few messages content (fallback)
func ExtractSessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	primary, _ := extractSessionIDs(headers, payload, metadata)
	return primary
}

// extractSessionIDs returns (primaryID, fallbackID) for session affinity.
// primaryID: full hash including assistant response (stable after first turn)
// fallbackID: short hash without assistant (used to inherit binding from first turn)
func extractSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (string, string) {
	// 1. metadata.user_id with Claude Code session format (highest priority)
	if len(payload) > 0 {
		userID := gjson.GetBytes(payload, "metadata.user_id").String()
		if userID != "" {
			// Old format: user_{hash}_account__session_{uuid}
			if matches := sessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
				id := "claude:" + matches[1]
				return id, ""
			}
			// New format: JSON object with session_id field
			// e.g. {"device_id":"...","account_uuid":"...","session_id":"uuid"}
			if len(userID) > 0 && userID[0] == '{' {
				if sid := gjson.Get(userID, "session_id").String(); sid != "" {
					return "claude:" + sid, ""
				}
			}
		}
	}

	// 2. X-Session-ID header
	if headers != nil {
		if sid := headers.Get("X-Session-ID"); sid != "" {
			return "header:" + sid, ""
		}
	}

	// 3. Session_id header (Codex)
	if headers != nil {
		if sid := headers.Get("Session-Id"); sid != "" {
			return "codex:" + sid, ""
		}
		if sid := headers.Get("Session_id"); sid != "" {
			return "codex:" + sid, ""
		}
	}

	// 4. X-Amp-Thread-Id header (Amp CLI thread ID)
	if headers != nil {
		if tid := headers.Get("X-Amp-Thread-Id"); tid != "" {
			return "amp:" + tid, ""
		}
	}

	// 5. X-Client-Request-Id header (PI)
	if headers != nil {
		if rid := headers.Get("X-Client-Request-Id"); rid != "" {
			return "clientreq:" + rid, ""
		}
	}

	if len(payload) == 0 {
		return "", ""
	}

	// 6. metadata.user_id (non-Claude Code format)
	userID := gjson.GetBytes(payload, "metadata.user_id").String()
	if userID != "" {
		return "user:" + userID, ""
	}

	// 7. conversation_id field
	if convID := gjson.GetBytes(payload, "conversation_id").String(); convID != "" {
		return "conv:" + convID, ""
	}

	// 8. Hash-based fallback from message content
	return extractMessageHashIDs(payload)
}

func extractMessageHashIDs(payload []byte) (primaryID, fallbackID string) {
	var systemPrompt, firstUserMsg, firstAssistantMsg string

	// OpenAI/Claude messages format
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			content := extractMessageContent(msg.Get("content"))
			if content == "" {
				return true
			}

			switch role {
			case "system":
				if systemPrompt == "" {
					systemPrompt = truncateString(content, 100)
				}
			case "user":
				if firstUserMsg == "" {
					firstUserMsg = truncateString(content, 100)
				}
			case "assistant":
				if firstAssistantMsg == "" {
					firstAssistantMsg = truncateString(content, 100)
				}
			}

			if systemPrompt != "" && firstUserMsg != "" && firstAssistantMsg != "" {
				return false
			}
			return true
		})
	}

	// Claude API: top-level "system" field (array or string)
	if systemPrompt == "" {
		topSystem := gjson.GetBytes(payload, "system")
		if topSystem.Exists() {
			if topSystem.IsArray() {
				topSystem.ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text").String(); text != "" && systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
						return false
					}
					return true
				})
			} else if topSystem.Type == gjson.String {
				systemPrompt = truncateString(topSystem.String(), 100)
			}
		}
	}

	// Gemini format
	if systemPrompt == "" && firstUserMsg == "" {
		sysInstr := gjson.GetBytes(payload, "systemInstruction.parts")
		if sysInstr.Exists() && sysInstr.IsArray() {
			sysInstr.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" && systemPrompt == "" {
					systemPrompt = truncateString(text, 100)
					return false
				}
				return true
			})
		}

		contents := gjson.GetBytes(payload, "contents")
		if contents.Exists() && contents.IsArray() {
			contents.ForEach(func(_, msg gjson.Result) bool {
				role := msg.Get("role").String()
				msg.Get("parts").ForEach(func(_, part gjson.Result) bool {
					text := part.Get("text").String()
					if text == "" {
						return true
					}
					switch role {
					case "user":
						if firstUserMsg == "" {
							firstUserMsg = truncateString(text, 100)
						}
					case "model":
						if firstAssistantMsg == "" {
							firstAssistantMsg = truncateString(text, 100)
						}
					}
					return false
				})
				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	// OpenAI Responses API format (v1/responses)
	if systemPrompt == "" && firstUserMsg == "" {
		if instr := gjson.GetBytes(payload, "instructions").String(); instr != "" {
			systemPrompt = truncateString(instr, 100)
		}

		input := gjson.GetBytes(payload, "input")
		if input.Exists() && input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				itemType := item.Get("type").String()
				if itemType == "reasoning" {
					return true
				}
				// Skip non-message typed items (function_call, function_call_output, etc.)
				// but allow items with no type that have a role (inline message format).
				if itemType != "" && itemType != "message" {
					return true
				}

				role := item.Get("role").String()
				if itemType == "" && role == "" {
					return true
				}

				// Handle both string content and array content (multimodal).
				content := item.Get("content")
				var text string
				if content.Type == gjson.String {
					text = content.String()
				} else {
					text = extractResponsesAPIContent(content)
				}
				if text == "" {
					return true
				}

				switch role {
				case "developer", "system":
					if systemPrompt == "" {
						systemPrompt = truncateString(text, 100)
					}
				case "user":
					if firstUserMsg == "" {
						firstUserMsg = truncateString(text, 100)
					}
				case "assistant":
					if firstAssistantMsg == "" {
						firstAssistantMsg = truncateString(text, 100)
					}
				}

				if firstUserMsg != "" && firstAssistantMsg != "" {
					return false
				}
				return true
			})
		}
	}

	if systemPrompt == "" && firstUserMsg == "" {
		return "", ""
	}

	shortHash := computeSessionHash(systemPrompt, firstUserMsg, "")
	if firstAssistantMsg == "" {
		return shortHash, ""
	}

	fullHash := computeSessionHash(systemPrompt, firstUserMsg, firstAssistantMsg)
	return fullHash, shortHash
}

func computeSessionHash(systemPrompt, userMsg, assistantMsg string) string {
	h := fnv.New64a()
	if systemPrompt != "" {
		h.Write([]byte("sys:" + systemPrompt + "\n"))
	}
	if userMsg != "" {
		h.Write([]byte("usr:" + userMsg + "\n"))
	}
	if assistantMsg != "" {
		h.Write([]byte("ast:" + assistantMsg + "\n"))
	}
	return fmt.Sprintf("msg:%016x", h.Sum64())
}

func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}

// extractMessageContent extracts text content from a message content field.
// Handles both string content and array content (multimodal messages).
// For array content, extracts text from all text-type elements.
func extractMessageContent(content gjson.Result) string {
	// String content: "Hello world"
	if content.Type == gjson.String {
		return content.String()
	}

	// Array content: [{"type":"text","text":"Hello"},{"type":"image",...}]
	if content.IsArray() {
		var texts []string
		content.ForEach(func(_, part gjson.Result) bool {
			// Handle Claude format: {"type":"text","text":"content"}
			if part.Get("type").String() == "text" {
				if text := part.Get("text").String(); text != "" {
					texts = append(texts, text)
				}
			}
			// Handle OpenAI format: {"type":"text","text":"content"}
			// Same structure as Claude, already handled above
			return true
		})
		if len(texts) > 0 {
			return strings.Join(texts, " ")
		}
	}

	return ""
}

func extractResponsesAPIContent(content gjson.Result) string {
	if !content.IsArray() {
		return ""
	}
	var texts []string
	content.ForEach(func(_, part gjson.Result) bool {
		partType := part.Get("type").String()
		if partType == "input_text" || partType == "output_text" || partType == "text" {
			if text := part.Get("text").String(); text != "" {
				texts = append(texts, text)
			}
		}
		return true
	})
	if len(texts) > 0 {
		return strings.Join(texts, " ")
	}
	return ""
}

// extractSessionID is kept for backward compatibility.
// Deprecated: Use ExtractSessionID instead.
func extractSessionID(payload []byte) string {
	return ExtractSessionID(nil, payload, nil)
}
