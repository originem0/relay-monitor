package provider

import (
	"sort"
	"strconv"
	"strings"
)

// VendorModel is a ranked model pick for a vendor.
type VendorModel struct {
	Vendor string
	Model  string
}

// lowKeywords demote a model in flagship selection (smaller/non-chat variants).
var lowKeywords = []string{
	"mini", "nano", "low", "small", "lite", "tiny",
	"imagine", "image", "video", "edit", "docs", "oss",
}

// highKeywords promote a model in flagship selection (stronger variants).
var highKeywords = []string{
	"pro", "plus", "max", "high", "xhigh", "expert",
	"thinking", "codex",
}

// flagshipScore scores a model for flagship selection.
// Lower-tier keywords reduce the score; higher-tier keywords increase it.
func flagshipPenalty(modelID string) int {
	ml := strings.ToLower(modelID)
	for _, kw := range lowKeywords {
		if strings.Contains(ml, kw) {
			return -10
		}
	}
	return 0
}

func flagshipBonus(modelID string) int {
	ml := strings.ToLower(modelID)
	for _, kw := range highKeywords {
		if strings.Contains(ml, kw) {
			return 5
		}
	}
	return 0
}

func flagshipScore(modelID string) int {
	return flagshipPenalty(modelID) + flagshipBonus(modelID)
}

func numericSignature(modelID string) []int {
	var nums []int
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		n, err := strconv.Atoi(cur.String())
		if err == nil {
			nums = append(nums, n)
		}
		cur.Reset()
	}
	for _, ch := range modelID {
		if ch >= '0' && ch <= '9' {
			cur.WriteRune(ch)
			continue
		}
		flush()
	}
	flush()
	return nums
}

func compareNumericSignature(a, b []int) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(a) > len(b):
		return 1
	case len(a) < len(b):
		return -1
	default:
		return 0
	}
}

func sortFlagshipCandidates(models []string) {
	sort.Slice(models, func(i, j int) bool {
		_, mini := ClassifyTier(models[i])
		_, minj := ClassifyTier(models[j])
		if mini != minj {
			return mini > minj
		}
		if pi, pj := flagshipPenalty(models[i]), flagshipPenalty(models[j]); pi != pj {
			return pi > pj
		}
		if cmp := compareNumericSignature(numericSignature(models[i]), numericSignature(models[j])); cmp != 0 {
			return cmp > 0
		}
		si, sj := flagshipBonus(models[i]), flagshipBonus(models[j])
		if si != sj {
			return si > sj
		}
		return models[i] > models[j]
	})
}

// PickFlagships selects one flagship model per vendor from the given model list.
// Models are scored by heuristic (penalizing "mini"/"lite"/etc., boosting "pro"/"max"/etc.),
// then the highest-scoring model per vendor is picked, with lexicographic tiebreak.
func PickFlagships(models []string) map[string]string {
	byVendor := make(map[string][]string)
	for _, m := range models {
		v := IdentifyVendor(m)
		byVendor[v] = append(byVendor[v], m)
	}

	result := make(map[string]string, len(byVendor))
	for vendor, mlist := range byVendor {
		sortFlagshipCandidates(mlist)
		result[vendor] = mlist[0]
	}
	return result
}

// PickTopModelsPerVendor selects up to n best-ranked models per vendor.
// This is used by fingerprinting to avoid the current one-model-per-vendor blind spot.
func PickTopModelsPerVendor(models []string, n int) []VendorModel {
	if n <= 0 {
		return nil
	}

	byVendor := make(map[string][]string)
	for _, m := range models {
		v := IdentifyVendor(m)
		byVendor[v] = append(byVendor[v], m)
	}

	vendors := make([]string, 0, len(byVendor))
	for vendor := range byVendor {
		vendors = append(vendors, vendor)
	}
	sort.Strings(vendors)

	var result []VendorModel
	for _, vendor := range vendors {
		mlist := byVendor[vendor]
		sortFlagshipCandidates(mlist)
		limit := n
		if len(mlist) < limit {
			limit = len(mlist)
		}
		for i := 0; i < limit; i++ {
			result = append(result, VendorModel{
				Vendor: vendor,
				Model:  mlist[i],
			})
		}
	}
	return result
}

// tierPattern maps a model name substring to an expected tier and minimum score.
type tierPattern struct {
	pattern  string
	tier     string
	minScore int
}

// tierPatterns is checked in order; first match wins.
var tierPatterns = []tierPattern{
	{"gpt-5.4", "S", 9}, {"gpt-5.3", "S", 9},
	{"gpt-5.2", "S", 9}, {"gpt-5.1", "S", 9},
	{"claude-4.6", "S", 9}, {"claude-4.5", "S", 9},
	{"claude-opus", "S", 9}, {"claude-sonnet-4", "S", 9},
	{"gemini-3-pro", "S", 9}, {"gemini-2.5-pro", "S", 9},
	{"deepseek-v3.2", "S", 9}, {"deepseek-r1", "S", 9},
	{"qwen3.5", "S", 9}, {"qwen3-235b", "S", 9},
	{"glm-5", "S", 9}, {"kimi-k2.5", "A", 7},
	{"gpt-5", "A", 7}, {"gemini-2.5-flash", "A", 7},
	{"gemini-3-flash", "A", 7}, {"deepseek-v3", "A", 7},
	{"glm-4.7", "A", 7}, {"glm-4.6", "A", 7},
	{"qwen3-max", "A", 7}, {"minimax-m2", "A", 7},
	{"kimi-k2", "A", 7}, {"qwen3-plus", "A", 7},
	{"qwen3-32b", "B", 4}, {"llama-3.3-70b", "B", 4},
	{"glm-4.5", "B", 4}, {"gpt-4o", "B", 4},
	{"llama", "C", 1}, {"gemma", "C", 1},
	{"dolphin", "C", 1}, {"phi-", "C", 1},
}

// ClassifyTier returns the expected performance tier and minimum score for a model.
// Tier values: "S" (flagship, >=9/10), "A" (strong, >=7), "B" (mid, >=4), "C" (weak, >=1).
// Returns ("?", 5) if no pattern matches.
func ClassifyTier(modelID string) (tier string, minScore int) {
	low := strings.ToLower(modelID)
	for _, tp := range tierPatterns {
		if strings.Contains(low, tp.pattern) {
			return tp.tier, tp.minScore
		}
	}
	return "?", 5
}
