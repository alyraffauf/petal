package routing

import (
	"testing"
)

func TestPlacementExplainsRoutingPolicies(t *testing.T) {
	account := func(remaining string, capacity float64) Account {
		return Account{Models: map[string][]string{"m": {"adapter"}},
			Usage: []string{"printf", "%s", remaining}, Capacity: &capacity}
	}
	c := Config{Model: "m", Accounts: map[string]Account{
		"alpha": account("50", 1), "parent": account("25", 2), "spent": account("0", 5),
	}}
	chosen, err := c.ChoosePlacement(t.Context(), "alpha")
	if err != nil || (chosen.Account != "alpha" && chosen.Account != "parent") || chosen.Reason != RandomTie || chosen.Weighted != "0.5" {
		t.Fatalf("ordinary tie: %+v, %v", chosen, err)
	}
	ranked, err := c.RankPlacements(t.Context(), "parent")
	if err != nil || len(ranked) != 2 || ranked[0].Account != "parent" || ranked[0].Reason != ParentTie || ranked[0].Weighted != "0.5" {
		t.Fatalf("delegated tie: %+v, %v", ranked, err)
	}
	ranked, err = c.RankPlacements(t.Context(), "spent")
	if err != nil || len(ranked) != 2 || ranked[0].Account != "alpha" || ranked[0].Reason != NameTie {
		t.Fatalf("delegated tie without eligible parent: %+v, %v", ranked, err)
	}

	c.PinnedAccount = "spent"
	chosen, err = c.ChoosePlacement(t.Context(), "alpha")
	if err != nil || chosen.Account != "spent" || chosen.Reason != Pinned || chosen.Weighted != "" {
		t.Fatalf("explicit pin: %+v, %v", chosen, err)
	}

	c.PinnedAccount = ""
	c.Accounts["alpha"] = account("invalid", 1)
	c.Accounts["parent"] = account("0", 2)
	chosen, err = c.ChoosePlacement(t.Context(), "parent")
	if err != nil || chosen.Account != "parent" || chosen.Reason != OpeningFallback || chosen.Eligibility != "Exhausted" || chosen.Weighted != "0" {
		t.Fatalf("ordinary fallback: %+v, %v", chosen, err)
	}
}

func TestQuotaPlacementRecordsLimitingWindow(t *testing.T) {
	for _, tc := range []struct {
		quota  Quota
		window string
	}{
		{claudeQuotaWindow([]byte(`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":20},"seven_day":{"utilization":40},"seven_day_opus":{"utilization":70}}}`), "opus"), "seven_day_opus"},
		{codexQuotaWindow([]byte(`{"rateLimits":{"primary":{"usedPercent":20},"secondary":{"usedPercent":70}}}`)), "Secondary"},
	} {
		tc.quota.Name, tc.quota.Capacity = "work", 5
		p := tc.quota.placement("work", HighestQuota)
		if tc.quota.Err != nil || p.Window != tc.window || p.Remaining != 30 || p.Weighted != "1.5" {
			t.Fatalf("placement: %+v, %v", p, tc.quota.Err)
		}
	}
}
