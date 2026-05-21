package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestSelectorFromRoutingConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *config.Config
		want any
	}{
		{name: "default", cfg: &config.Config{}, want: &coreauth.RoundRobinSelector{}},
		{name: "fill-first", cfg: &config.Config{Routing: config.RoutingConfig{Strategy: "fillfirst"}}, want: &coreauth.FillFirstSelector{}},
		{name: "codex-quota-score", cfg: &config.Config{Routing: config.RoutingConfig{Strategy: "codex-quota-score"}}, want: &coreauth.CodexQuotaScoreSelector{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectorFromRoutingConfig(tc.cfg)
			switch tc.want.(type) {
			case *coreauth.RoundRobinSelector:
				if _, ok := got.(*coreauth.RoundRobinSelector); !ok {
					t.Fatalf("selector type = %T, want RoundRobinSelector", got)
				}
			case *coreauth.FillFirstSelector:
				if _, ok := got.(*coreauth.FillFirstSelector); !ok {
					t.Fatalf("selector type = %T, want FillFirstSelector", got)
				}
			case *coreauth.CodexQuotaScoreSelector:
				if _, ok := got.(*coreauth.CodexQuotaScoreSelector); !ok {
					t.Fatalf("selector type = %T, want CodexQuotaScoreSelector", got)
				}
			}
		})
	}
}

func TestSelectorFromRoutingConfig_WithSessionAffinityWrapsFallback(t *testing.T) {
	t.Parallel()

	got := selectorFromRoutingConfig(&config.Config{Routing: config.RoutingConfig{
		Strategy:        "codex-quota-score",
		SessionAffinity: true,
	}})
	if _, ok := got.(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("selector type = %T, want SessionAffinitySelector", got)
	}
}
