package management

import "testing"

func TestNormalizeRoutingStrategyAcceptsCodexQuotaScore(t *testing.T) {
	got, ok := normalizeRoutingStrategy("codex-quota-score")
	if !ok {
		t.Fatal("normalizeRoutingStrategy(codex-quota-score) ok = false, want true")
	}
	if got != "codex-quota-score" {
		t.Fatalf("normalizeRoutingStrategy(codex-quota-score) = %q, want codex-quota-score", got)
	}
}
