package provider

import "testing"

func TestPickTopModelsPerVendorReturnsTopThreePerVendor(t *testing.T) {
	picks := PickTopModelsPerVendor([]string{
		"claude-sonnet-4.5",
		"claude-sonnet-4.5-thinking",
		"claude-sonnet-4.6",
		"claude-haiku-4.5",
		"gpt-5.4",
		"gpt-5.3-codex",
		"gpt-5.4-mini",
		"gpt-4o",
	}, 3)

	got := make(map[string][]string)
	for _, pick := range picks {
		got[pick.Vendor] = append(got[pick.Vendor], pick.Model)
	}

	wantClaude := []string{
		"claude-sonnet-4.6",
		"claude-sonnet-4.5-thinking",
		"claude-sonnet-4.5",
	}
	wantGPT := []string{
		"gpt-5.4",
		"gpt-5.3-codex",
		"gpt-5.4-mini",
	}

	assertModels := func(vendor string, gotModels, wantModels []string) {
		if len(gotModels) != len(wantModels) {
			t.Fatalf("%s picks = %v, want %v", vendor, gotModels, wantModels)
		}
		for i := range wantModels {
			if gotModels[i] != wantModels[i] {
				t.Fatalf("%s picks = %v, want %v", vendor, gotModels, wantModels)
			}
		}
	}

	assertModels("Claude", got["Claude"], wantClaude)
	assertModels("GPT", got["GPT"], wantGPT)
}

func TestPickTopModelsPerVendorRejectsNonPositiveLimit(t *testing.T) {
	if picks := PickTopModelsPerVendor([]string{"gpt-5.4"}, 0); len(picks) != 0 {
		t.Fatalf("expected no picks, got %v", picks)
	}
}

func TestPickFlagshipsPrefersNewerCoreModelOverKeywordBoost(t *testing.T) {
	got := PickFlagships([]string{"gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini"})
	if got["GPT"] != "gpt-5.4" {
		t.Fatalf("GPT flagship = %s, want gpt-5.4", got["GPT"])
	}
}
