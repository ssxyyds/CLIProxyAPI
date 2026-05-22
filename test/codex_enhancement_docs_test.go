package test

import (
	"os"
	"strings"
	"testing"
)

func TestCodexEnhancementDocsCoverOperationalAPI(t *testing.T) {
	body, err := os.ReadFile("../docs/codex-enhancements.md")
	if err != nil {
		t.Fatalf("read docs/codex-enhancements.md: %v", err)
	}
	doc := string(body)
	required := []string{
		"codex-quota-score",
		"GET /v0/management/codex-state",
		"PATCH /v0/management/codex-state/manual-score",
		"POST /v0/management/codex-state/refresh",
		"POST /v0/management/codex-state/recalc",
		"codex-quota-probe",
		"bootstrap refresh",
		"quota refresh gate",
		"current_selections",
		"manual score save recalculates",
		"scheduler fast path",
		"Credentials",
		"summary",
		"统计页面",
	}
	for _, want := range required {
		if !strings.Contains(doc, want) {
			t.Fatalf("docs/codex-enhancements.md missing %q", want)
		}
	}
}

func TestCodexEnhancementConfigExampleDefaultsToQuotaScore(t *testing.T) {
	body, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	if !strings.Contains(string(body), `strategy: "codex-quota-score"`) {
		t.Fatal(`config.example.yaml should default routing.strategy to "codex-quota-score" on this branch`)
	}
}
