package cliproxy

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func selectorFromRoutingConfig(cfg *config.Config) coreauth.Selector {
	strategy := config.NormalizeRoutingStrategy("")
	if cfg != nil {
		strategy = config.NormalizeRoutingStrategy(cfg.Routing.Strategy)
	}

	var selector coreauth.Selector
	switch strategy {
	case "fill-first":
		selector = &coreauth.FillFirstSelector{}
	case "codex-quota-score":
		selector = &coreauth.CodexQuotaScoreSelector{}
	default:
		selector = &coreauth.RoundRobinSelector{}
	}

	if cfg == nil || !cfg.Routing.SessionAffinity {
		return selector
	}

	ttl := time.Hour
	if ttlStr := strings.TrimSpace(cfg.Routing.SessionAffinityTTL); ttlStr != "" {
		if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
			ttl = parsed
		}
	}

	return coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: selector,
		TTL:      ttl,
	})
}
