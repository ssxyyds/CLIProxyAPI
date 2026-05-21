package config

import "strings"

// NormalizeRoutingStrategy canonicalizes supported routing strategy names.
func NormalizeRoutingStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin"
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	case "codex-quota-score":
		return "codex-quota-score"
	default:
		return "round-robin"
	}
}
