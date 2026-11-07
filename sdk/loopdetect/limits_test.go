package loopdetect

import "testing"

func TestReplyLimitsValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		limits  ReplyLimits
		invalid bool
	}{
		"all defaults":              {ReplyLimits{}, false},
		"window at its lower bound": {ReplyLimits{Window: MinReplyWindow}, false},
		"window at its upper bound": {ReplyLimits{Window: MaxReplyWindow}, false},
		"window below its bound":    {ReplyLimits{Window: MinReplyWindow - 1}, true},
		"window above its bound":    {ReplyLimits{Window: MaxReplyWindow + 1}, true},
		"negative window":           {ReplyLimits{Window: -5}, true},
		// A distinct threshold alone is judged against the default window.
		"distinct at a quarter of the default window":    {ReplyLimits{Distinct: DefaultReplyWindow / 4}, false},
		"distinct above a quarter of the default window": {ReplyLimits{Distinct: DefaultReplyWindow/4 + 1}, true},
		"distinct at a quarter of its window":            {ReplyLimits{Window: 512, Distinct: 128}, false},
		"distinct above a quarter of its window":         {ReplyLimits{Window: 512, Distinct: 129}, true},
		"distinct at its lower bound":                    {ReplyLimits{Distinct: MinReplyDistinct}, false},
		"distinct below its lower bound":                 {ReplyLimits{Distinct: MinReplyDistinct - 1}, true},
		"negative distinct":                              {ReplyLimits{Distinct: -1}, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.limits.Validate()
			if (err != nil) != tc.invalid {
				t.Fatalf("Validate(%+v) = %v, invalid = %v", tc.limits, err, tc.invalid)
			}
			// What Validate accepts, Normalized leaves alone (apart from defaults).
			if err == nil {
				normalized := tc.limits.Normalized()
				if tc.limits.Window != 0 && normalized.Window != tc.limits.Window {
					t.Fatalf("a valid window %d became %d", tc.limits.Window, normalized.Window)
				}
				if tc.limits.Distinct != 0 && normalized.Distinct != tc.limits.Distinct {
					t.Fatalf("a valid distinct %d became %d", tc.limits.Distinct, normalized.Distinct)
				}
			}
		})
	}
}

func TestToolLimitsValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		limits  ToolLimits
		invalid bool
	}{
		"default":         {ToolLimits{}, false},
		"lower bound":     {ToolLimits{Repeats: MinToolRepeats}, false},
		"upper bound":     {ToolLimits{Repeats: MaxToolRepeats}, false},
		"below the bound": {ToolLimits{Repeats: MinToolRepeats - 1}, true},
		"above the bound": {ToolLimits{Repeats: MaxToolRepeats + 1}, true},
		"negative":        {ToolLimits{Repeats: -3}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.limits.Validate(); (err != nil) != tc.invalid {
				t.Fatalf("Validate(%+v) = %v, invalid = %v", tc.limits, err, tc.invalid)
			}
		})
	}
}

func TestMaxReplyDistinct(t *testing.T) {
	for window, want := range map[int]int{
		0:                  DefaultReplyWindow / 4,
		MinReplyWindow:     MinReplyWindow / 4,
		256:                64,
		MaxReplyWindow:     MaxReplyWindow / 4,
		MaxReplyWindow * 4: MaxReplyWindow / 4, // held to the bound, as Normalized does
		MinReplyWindow / 2: MinReplyWindow / 4,
		-1:                 DefaultReplyWindow / 4,
	} {
		if got := MaxReplyDistinct(window); got != want {
			t.Fatalf("MaxReplyDistinct(%d) = %d, want %d", window, got, want)
		}
	}
}
