package usage

import (
	"math"
	"testing"
)

func TestOneHourCacheWritesCostTwiceTheInputPrice(t *testing.T) {
	// Anthropic's list prices for one model: input 3, output 15, cache write
	// (five minutes) 3.75, cache read 0.3, all per million tokens.
	cost := func(writes, writes1h int64) float64 {
		return calculateTokenCostV2(0, 0, 0, writes, writes1h, false, 3, 15, 0, 0.3, 3.75)
	}
	if got := cost(1_000_000, 0); math.Abs(got-3.75) > 1e-9 {
		t.Fatalf("a million five-minute writes cost %v, want 3.75", got)
	}
	if got := cost(1_000_000, 1_000_000); math.Abs(got-6) > 1e-9 {
		t.Fatalf("a million one-hour writes cost %v, want 6 (twice the input price)", got)
	}
	if got := cost(1_000_000, 400_000); math.Abs(got-(3.75+0.4*2.25)) > 1e-9 {
		t.Fatalf("a mixed million cost %v, want %v", got, 3.75+0.4*2.25)
	}
	// A one-hour count above the writes it is part of is capped at them.
	if got := cost(1_000_000, 5_000_000); math.Abs(got-6) > 1e-9 {
		t.Fatalf("an oversized one-hour count cost %v, want 6", got)
	}
}
