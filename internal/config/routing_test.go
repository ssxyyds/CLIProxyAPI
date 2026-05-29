package config

import "testing"

func TestNormalizeRoutingStrategy(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"":                  "codex-quota-score",
		"round-robin":       "round-robin",
		"fillfirst":         "fill-first",
		"FF":                "fill-first",
		"codex-quota-score": "codex-quota-score",
		"unknown":           "codex-quota-score",
	}

	for input, want := range tests {
		if got := NormalizeRoutingStrategy(input); got != want {
			t.Fatalf("NormalizeRoutingStrategy(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseConfigBytes_NormalizesRoutingStrategy(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfigBytes([]byte("routing:\n  strategy: fillfirst\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.Routing.Strategy != "fill-first" {
		t.Fatalf("Routing.Strategy = %q, want fill-first", cfg.Routing.Strategy)
	}

	cfg, err = ParseConfigBytes([]byte("routing:\n  strategy: codex-quota-score\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() codex error = %v", err)
	}
	if cfg.Routing.Strategy != "codex-quota-score" {
		t.Fatalf("Routing.Strategy = %q, want codex-quota-score", cfg.Routing.Strategy)
	}
}
