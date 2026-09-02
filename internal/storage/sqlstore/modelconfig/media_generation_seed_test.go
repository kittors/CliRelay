package modelconfig

import "testing"

// The library scope only surfaces 'seed' and 'openrouter' rows, and the
// channel-group editor reads the library. A media model missing from the seed is
// therefore unselectable there, which is what made a Grok request fail with
// "not in the allowed models of channel group" and left the operator no fix.
func TestSeedRowsCoverGrokMediaModels(t *testing.T) {
	rows := defaultModelConfigRows()
	byID := make(map[string]ModelConfigRow, len(rows))
	for _, row := range rows {
		byID[row.ModelID] = row
	}

	for _, id := range []string{"grok-imagine-image", "grok-imagine-image-quality", "grok-imagine-video-1.5"} {
		row, ok := byID[id]
		if !ok {
			t.Errorf("%s missing from the seeded model library", id)
			continue
		}
		if NormalizePricingMode(row.PricingMode) != "call" {
			t.Errorf("%s pricing mode = %q, want call: media models are billed per invocation", id, row.PricingMode)
		}
		if len(row.OutputModalities) == 0 {
			t.Errorf("%s has no output modality, so the catalog cannot classify it", id)
		}
	}

	if got := byID["grok-imagine-video-1.5"].OutputModalities; len(got) != 1 || got[0] != "video" {
		t.Errorf("video output modalities = %v, want [video]", got)
	}
}

func TestMediaSeedDefaultsDoNotCrossClassify(t *testing.T) {
	video := ModelConfigRow{ModelID: "grok-imagine-video-1.5", PricingMode: "token"}
	applyMediaGenerationSeedDefaults(&video, video.ModelID)
	if len(video.InputModalities) != 2 {
		t.Fatalf("video input modalities = %v, want text and image", video.InputModalities)
	}

	image := ModelConfigRow{ModelID: "grok-imagine-image", PricingMode: "token"}
	applyMediaGenerationSeedDefaults(&image, image.ModelID)
	if len(image.OutputModalities) != 1 || image.OutputModalities[0] != "image" {
		t.Fatalf("image output modalities = %v, want [image]", image.OutputModalities)
	}

	chat := ModelConfigRow{ModelID: "grok-4.5", PricingMode: "token"}
	applyMediaGenerationSeedDefaults(&chat, chat.ModelID)
	if NormalizePricingMode(chat.PricingMode) != "token" {
		t.Fatalf("a chat model must keep token pricing, got %q", chat.PricingMode)
	}
}
