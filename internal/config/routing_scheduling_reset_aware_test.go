package config

import "testing"

func TestNormalizeDistributionAcceptsResetAware(t *testing.T) {
	for _, input := range []string{"reset-aware", "Reset-Aware", " resetaware ", "smart"} {
		if got := NormalizeDistribution(input); got != DistributionResetAware {
			t.Errorf("NormalizeDistribution(%q) = %q, want %q", input, got, DistributionResetAware)
		}
	}
}

// Sanitizing keeps the mode and mirrors the closest legacy strategy, so an
// older binary that only reads `strategy` still keeps one account answering
// until it is nearly spent.
func TestSanitizeRoutingKeepsResetAware(t *testing.T) {
	cfg := &Config{Routing: RoutingConfig{ChannelGroups: []RoutingChannelGroup{{
		Name:       "smart",
		Scheduling: GroupScheduling{Distribution: "smart"},
	}}}}
	cfg.SanitizeRouting()

	group := cfg.Routing.ChannelGroups[0]
	if group.Scheduling.Distribution != DistributionResetAware {
		t.Fatalf("distribution = %q, want %q", group.Scheduling.Distribution, DistributionResetAware)
	}
	if group.Strategy != "fill-first" {
		t.Fatalf("legacy strategy = %q, want fill-first", group.Strategy)
	}
}
