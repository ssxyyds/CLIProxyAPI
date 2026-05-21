package test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type codexManagementStore struct {
	mu    sync.Mutex
	items map[string]*coreauth.Auth
}

func (s *codexManagementStore) List(_ context.Context) ([]*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item.Clone())
	}
	return out, nil
}

func (s *codexManagementStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string]*coreauth.Auth{}
	}
	s.items[auth.ID] = auth.Clone()
	return auth.ID, nil
}

func (s *codexManagementStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}

func (s *codexManagementStore) SetBaseDir(string) {}

type codexManagementExecutor struct {
	mu           sync.Mutex
	refreshCount map[string]int
}

func (e *codexManagementExecutor) Identifier() string { return "codex" }
func (e *codexManagementExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *codexManagementExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *codexManagementExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *codexManagementExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}
func (e *codexManagementExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if auth == nil {
		return nil, nil
	}
	e.mu.Lock()
	if e.refreshCount == nil {
		e.refreshCount = map[string]int{}
	}
	e.refreshCount[auth.ID]++
	e.mu.Unlock()
	now := time.Now().UTC()
	quota, _ := auth.GetCodexQuotaState()
	quota.LastRefreshAt = &now
	quota.RefreshStatus = "ok"
	quota.RefreshError = ""
	auth.SetCodexQuotaState(quota)
	auth.SetCodexLastSelectionReason("refreshed by management endpoint")
	return auth, nil
}

func (e *codexManagementExecutor) count(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshCount[id]
}

func newCodexManagementHandler(t *testing.T) (*management.Handler, *coreauth.Manager, *codexManagementExecutor, *coreauth.Auth, *coreauth.Auth, *coreauth.Auth) {
	t.Helper()
	store := &codexManagementStore{}
	manager := coreauth.NewManager(store, nil, nil)
	exec := &codexManagementExecutor{}
	manager.RegisterExecutor(exec)

	quotaReset := time.Now().UTC().Add(6 * time.Hour)
	weeklyLimit := 100.0
	weeklyRemaining := 42.0
	manual := 1.5
	computed := 8.4
	oauthAuth := &coreauth.Auth{
		ID:       "codex-oauth-id",
		Provider: "codex",
		FileName: "codex-oauth.json",
		Metadata: map[string]any{
			"email":    "oauth@example.com",
			"id_token": fakeCodexJWT("oauth@example.com", "acct_123", "plus"),
		},
		Status: coreauth.StatusActive,
	}
	oauthAuth.SetCodexQuotaState(coreauth.CodexQuotaState{
		Weekly:        coreauth.CodexQuotaBucket{Remaining: &weeklyRemaining, Limit: &weeklyLimit, ResetAt: &quotaReset},
		LastRefreshAt: &quotaReset,
		RefreshStatus: "ok",
	})
	oauthAuth.SetCodexManualScoreAdjustment(manual)
	oauthAuth.SetCodexComputedScore(computed)
	oauthAuth.SetCodexScoreReason("weekly remaining / hours to reset + manual")
	oauthAuth.SetCodexLastSelectionReason("highest final score")

	apiKeyAuth := &coreauth.Auth{
		ID:       "codex-api-key-id",
		Provider: "codex",
		FileName: "codex-api-key.json",
		Attributes: map[string]string{
			"api_key": "secret",
		},
		Status: coreauth.StatusActive,
	}

	otherAuth := &coreauth.Auth{
		ID:       "gemini-id",
		Provider: "gemini",
		FileName: "gemini.json",
		Metadata: map[string]any{"email": "gemini@example.com"},
		Status:   coreauth.StatusActive,
	}

	if _, err := manager.Register(context.Background(), oauthAuth); err != nil {
		t.Fatalf("register oauth auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), apiKeyAuth); err != nil {
		t.Fatalf("register api key auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), otherAuth); err != nil {
		t.Fatalf("register other auth: %v", err)
	}

	tmpDir := t.TempDir()
	h := management.NewHandler(&config.Config{AuthDir: filepath.Join(tmpDir, "auths")}, filepath.Join(tmpDir, "config.yaml"), manager)
	return h, manager, exec, oauthAuth, apiKeyAuth, otherAuth
}

func setupCodexManagementRouter(h *management.Handler) *gin.Engine {
	r := gin.New()
	mgmt := r.Group("/v0/management")
	{
		mgmt.GET("/codex-state", h.GetCodexState)
		mgmt.PATCH("/codex-state/manual-score", h.PatchCodexStateManualScore)
		mgmt.POST("/codex-state/refresh", h.PostCodexStateRefresh)
	}
	return r
}

func decodeCodexStateItems(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp struct {
		Items []map[string]any `json:"codex-state"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp.Items
}

func TestGetCodexState(t *testing.T) {
	h, _, _, oauthAuth, _, _ := newCodexManagementHandler(t)
	r := setupCodexManagementRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/codex-state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	items := decodeCodexStateItems(t, w.Body.Bytes())
	if len(items) != 1 {
		t.Fatalf("expected 1 codex runtime auth, got %d", len(items))
	}
	item := items[0]
	if item["id"] != oauthAuth.ID {
		t.Fatalf("expected id %q, got %#v", oauthAuth.ID, item["id"])
	}
	if item["email"] != "oauth@example.com" {
		t.Fatalf("expected email, got %#v", item["email"])
	}
	if item["codex_manual_score_adjustment"].(float64) != 1.5 {
		t.Fatalf("expected manual score 1.5, got %#v", item["codex_manual_score_adjustment"])
	}
	if item["codex_computed_score"].(float64) != 8.4 {
		t.Fatalf("expected computed score 8.4, got %#v", item["codex_computed_score"])
	}
	if item["codex_score_reason"] != "weekly remaining / hours to reset + manual" {
		t.Fatalf("unexpected score reason: %#v", item["codex_score_reason"])
	}
	explanation, ok := item["codex_score_explanation"].(map[string]any)
	if !ok {
		t.Fatalf("expected codex_score_explanation object, got %#v", item["codex_score_explanation"])
	}
	if explanation["score_available"] != true {
		t.Fatalf("expected score_available true, got %#v", explanation["score_available"])
	}
	if explanation["refresh_is_fresh"] != true {
		t.Fatalf("expected refresh_is_fresh true, got %#v", explanation["refresh_is_fresh"])
	}
	if explanation["formula_label"] != "weekly_remaining / max(hours_until_weekly_reset, 1) + expiry_urgency_bonus + manual_adjustment" {
		t.Fatalf("unexpected formula_label: %#v", explanation["formula_label"])
	}
	if _, ok := explanation["expiry_urgency_bonus"].(float64); !ok {
		t.Fatalf("expected expiry_urgency_bonus, got %#v", explanation["expiry_urgency_bonus"])
	}
	if explanation["manual_adjustment"].(float64) != 1.5 {
		t.Fatalf("unexpected manual_adjustment: %#v", explanation["manual_adjustment"])
	}
	if item["codex_last_selection_reason"] != "highest final score" {
		t.Fatalf("unexpected selection reason: %#v", item["codex_last_selection_reason"])
	}
	if _, ok := item["codex_quota"].(map[string]any); !ok {
		t.Fatalf("expected codex_quota object, got %#v", item["codex_quota"])
	}
	idToken, ok := item["id_token"].(map[string]any)
	if !ok {
		t.Fatalf("expected id_token claims, got %#v", item["id_token"])
	}
	if idToken["chatgpt_account_id"] != "acct_123" {
		t.Fatalf("unexpected chatgpt_account_id: %#v", idToken["chatgpt_account_id"])
	}
	if idToken["plan_type"] != "plus" {
		t.Fatalf("unexpected plan_type: %#v", idToken["plan_type"])
	}
}

func TestGetCodexState_IncludesPoolSummary(t *testing.T) {
	h, manager, _, _, _, _ := newCodexManagementHandler(t)
	r := setupCodexManagementRouter(h)

	weeklyReset := time.Now().UTC().Add(12 * time.Hour)
	fiveHourReset := time.Now().UTC().Add(2 * time.Hour)
	lastRefresh := time.Now().UTC().Add(-3 * time.Minute)
	weeklyLimit := 200.0
	weeklyRemaining := 80.0
	fiveHourLimit := 40.0
	fiveHourRemaining := 10.0
	extra := &coreauth.Auth{
		ID:       "codex-oauth-id-2",
		Provider: "codex",
		FileName: "codex-oauth-2.json",
		Metadata: map[string]any{"email": "oauth2@example.com"},
		Status:   coreauth.StatusActive,
	}
	extra.SetCodexQuotaState(coreauth.CodexQuotaState{
		Weekly:        coreauth.CodexQuotaBucket{Remaining: &weeklyRemaining, Limit: &weeklyLimit, ResetAt: &weeklyReset},
		FiveHour:      coreauth.CodexQuotaBucket{Remaining: &fiveHourRemaining, Limit: &fiveHourLimit, ResetAt: &fiveHourReset},
		LastRefreshAt: &lastRefresh,
		RefreshStatus: "ok",
	})
	if _, err := manager.Register(context.Background(), extra); err != nil {
		t.Fatalf("register extra auth: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/codex-state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	summary, ok := resp["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary = %#v, want object", resp["summary"])
	}
	accounts := summary["accounts"].(map[string]any)
	if accounts["total"].(float64) != 2 || accounts["active"].(float64) != 2 {
		t.Fatalf("accounts summary = %#v, want total/active 2", accounts)
	}
	weekly := summary["weekly"].(map[string]any)
	if weekly["known"].(float64) != 2 {
		t.Fatalf("weekly known = %#v, want 2", weekly["known"])
	}
	if weekly["limit"].(float64) != 300 || weekly["remaining"].(float64) != 122 {
		t.Fatalf("weekly summary = %#v, want limit 300 remaining 122", weekly)
	}
	fiveHour := summary["five_hour"].(map[string]any)
	if fiveHour["known"].(float64) != 1 {
		t.Fatalf("five_hour known = %#v, want 1", fiveHour["known"])
	}
	if fiveHour["limit"].(float64) != 40 || fiveHour["remaining"].(float64) != 10 {
		t.Fatalf("five_hour summary = %#v, want limit 40 remaining 10", fiveHour)
	}
	if summary["last_refresh_at"] == nil {
		t.Fatalf("last_refresh_at missing in summary: %#v", summary)
	}
}

func TestGetCodexState_HidesDisabledRemovedFileBackedAuthsButKeepsActiveOnes(t *testing.T) {
	h, manager, _, oauthAuth, _, _ := newCodexManagementHandler(t)

	activeAuth := &coreauth.Auth{
		ID:       "codex-oauth-active-2",
		Provider: "codex",
		FileName: "codex-active-2.json",
		Attributes: map[string]string{
			"path": filepath.Join(t.TempDir(), "codex-active-2.json"),
		},
		Metadata: map[string]any{
			"email":    "active2@example.com",
			"id_token": fakeCodexJWT("active2@example.com", "acct_456", "pro"),
		},
		Status: coreauth.StatusActive,
	}
	if err := os.WriteFile(activeAuth.Attributes["path"], []byte(`{"provider":"codex"}`), 0o644); err != nil {
		t.Fatalf("write active auth file: %v", err)
	}
	if _, err := manager.Register(context.Background(), activeAuth); err != nil {
		t.Fatalf("register second active auth: %v", err)
	}

	removedAuth := &coreauth.Auth{
		ID:       "codex-oauth-removed",
		Provider: "codex",
		FileName: "codex-removed.json",
		Attributes: map[string]string{
			"path": filepath.Join(t.TempDir(), "codex-removed.json"),
		},
		Metadata: map[string]any{
			"email":    "c@tang.ee",
			"id_token": fakeCodexJWT("c@tang.ee", "acct_removed", "plus"),
		},
		Disabled:      true,
		Status:        coreauth.StatusDisabled,
		StatusMessage: "removed via management API",
	}
	if _, err := manager.Register(context.Background(), removedAuth); err != nil {
		t.Fatalf("register removed auth: %v", err)
	}

	r := setupCodexManagementRouter(h)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/codex-state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	items := decodeCodexStateItems(t, w.Body.Bytes())
	if len(items) != 2 {
		t.Fatalf("expected 2 active codex runtime auths, got %d", len(items))
	}

	seen := map[string]bool{}
	for _, item := range items {
		if id, _ := item["id"].(string); id != "" {
			seen[id] = true
		}
		if email, _ := item["email"].(string); email == "c@tang.ee" {
			t.Fatalf("expected removed auth email to be hidden, got response item %#v", item)
		}
	}
	if !seen[oauthAuth.ID] {
		t.Fatalf("expected original active auth %q to remain visible", oauthAuth.ID)
	}
	if !seen[activeAuth.ID] {
		t.Fatalf("expected second active auth %q to remain visible", activeAuth.ID)
	}
	if seen[removedAuth.ID] {
		t.Fatalf("expected removed auth %q to be hidden", removedAuth.ID)
	}
}

func TestGetCodexState_DoesNotBackfillPersistedScoreFieldsFromLiveExplanation(t *testing.T) {
	h, manager, _, oauthAuth, _, _ := newCodexManagementHandler(t)
	updated, ok := manager.GetByID(oauthAuth.ID)
	if !ok {
		t.Fatalf("expected auth to exist")
	}
	updated.Metadata[coreauth.CodexComputedScoreMetadataKey] = nil
	delete(updated.Metadata, coreauth.CodexComputedScoreMetadataKey)
	updated.Metadata[coreauth.CodexScoreReasonMetadataKey] = nil
	delete(updated.Metadata, coreauth.CodexScoreReasonMetadataKey)
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	r := setupCodexManagementRouter(h)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/codex-state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	item := decodeCodexStateItems(t, w.Body.Bytes())[0]
	if _, exists := item["codex_computed_score"]; exists {
		t.Fatalf("expected no persisted codex_computed_score backfill, got %#v", item["codex_computed_score"])
	}
	if _, exists := item["codex_score_reason"]; exists {
		t.Fatalf("expected no persisted codex_score_reason backfill, got %#v", item["codex_score_reason"])
	}
	explanation, ok := item["codex_score_explanation"].(map[string]any)
	if !ok {
		t.Fatalf("expected codex_score_explanation object, got %#v", item["codex_score_explanation"])
	}
	if explanation["computed_score_live"] == nil {
		t.Fatalf("expected live explanation to retain computed_score_live, got %#v", explanation)
	}
}

func TestGetCodexState_ExplanationReflectsHardIneligibility(t *testing.T) {
	h, manager, _, oauthAuth, _, _ := newCodexManagementHandler(t)
	updated, ok := manager.GetByID(oauthAuth.ID)
	if !ok {
		t.Fatalf("expected auth to exist")
	}
	updated.Unavailable = true
	updated.NextRetryAfter = time.Now().UTC().Add(30 * time.Minute)
	updated.Quota.Exceeded = true
	updated.Quota.Reason = "quota"
	updated.Quota.NextRecoverAt = updated.NextRetryAfter
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	r := setupCodexManagementRouter(h)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/codex-state", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	item := decodeCodexStateItems(t, w.Body.Bytes())[0]
	explanation, ok := item["codex_score_explanation"].(map[string]any)
	if !ok {
		t.Fatalf("expected codex_score_explanation object, got %#v", item["codex_score_explanation"])
	}
	if explanation["score_available"] != false {
		t.Fatalf("expected score_available false, got %#v", explanation["score_available"])
	}
	if explanation["disqualifier_reason"] != "auth_cooldown" {
		t.Fatalf("expected auth_cooldown disqualifier, got %#v", explanation["disqualifier_reason"])
	}
}

func TestPatchCodexStateManualScore(t *testing.T) {
	h, manager, _, oauthAuth, _, _ := newCodexManagementHandler(t)
	r := setupCodexManagementRouter(h)

	body := `{"name":"codex-oauth.json","value":3.25}`
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/codex-state/manual-score", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	updated, ok := manager.GetByID(oauthAuth.ID)
	if !ok {
		t.Fatalf("expected updated auth to exist")
	}
	manual, ok := updated.CodexManualScoreAdjustment()
	if !ok || manual != 3.25 {
		t.Fatalf("expected updated manual score 3.25, got %v, %v", manual, ok)
	}
}

func TestPatchCodexStateManualScore_RejectsOutOfRangeValue(t *testing.T) {
	h, _, _, oauthAuth, _, _ := newCodexManagementHandler(t)
	r := setupCodexManagementRouter(h)

	body := `{"id":"` + oauthAuth.ID + `","value":101}`
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/codex-state/manual-score", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "between -100 and 100") {
		t.Fatalf("expected range error, got %s", w.Body.String())
	}
}

func TestPatchCodexStateManualScore_RejectsCodexAPIKeyAuth(t *testing.T) {
	h, _, _, _, apiKeyAuth, _ := newCodexManagementHandler(t)
	r := setupCodexManagementRouter(h)

	body := `{"id":"` + apiKeyAuth.ID + `","value":2}`
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/codex-state/manual-score", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
}

func TestPostCodexStateRefresh_SingleAuth(t *testing.T) {
	h, manager, exec, oauthAuth, _, _ := newCodexManagementHandler(t)
	r := setupCodexManagementRouter(h)

	body := `{"id":"` + oauthAuth.ID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/codex-state/refresh", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if exec.count(oauthAuth.ID) != 1 {
		t.Fatalf("expected 1 refresh call, got %d", exec.count(oauthAuth.ID))
	}
	updated, ok := manager.GetByID(oauthAuth.ID)
	if !ok {
		t.Fatalf("expected refreshed auth to exist")
	}
	quota, ok := updated.GetCodexQuotaState()
	if !ok || quota.LastRefreshAt == nil || quota.RefreshStatus != "ok" {
		t.Fatalf("expected refreshed quota state, got %#v, %v", quota, ok)
	}
}

func TestPostCodexStateRefresh_AllOnlyRefreshesOAuthLikeCodexAuths(t *testing.T) {
	h, _, exec, oauthAuth, apiKeyAuth, otherAuth := newCodexManagementHandler(t)
	r := setupCodexManagementRouter(h)

	body := `{"all":true}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/codex-state/refresh", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if exec.count(oauthAuth.ID) != 1 {
		t.Fatalf("expected oauth codex auth to refresh once, got %d", exec.count(oauthAuth.ID))
	}
	if exec.count(apiKeyAuth.ID) != 0 {
		t.Fatalf("expected codex api_key auth to be excluded, got %d refreshes", exec.count(apiKeyAuth.ID))
	}
	if exec.count(otherAuth.ID) != 0 {
		t.Fatalf("expected non-codex auth to be excluded, got %d refreshes", exec.count(otherAuth.ID))
	}
}

func fakeCodexJWT(email, accountID, planType string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `","https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID + `","chatgpt_plan_type":"` + planType + `"}}`))
	return header + "." + payload + ".signature"
}
