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
		"quota refresh gate",
		"summary",
		"统计页面",
	}
	for _, want := range required {
		if !strings.Contains(doc, want) {
			t.Fatalf("docs/codex-enhancements.md missing %q", want)
		}
	}
}
