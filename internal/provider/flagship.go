package provider

import (
	"sort"
	"strings"
)

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
func flagshipScore(modelID string) int {
	ml := strings.ToLower(modelID)
	s := 0
	for _, kw := range lowKeywords {
		if strings.Contains(ml, kw) {
			s -= 10
			break
		}
	}
	for _, kw := range highKeywords {
		if strings.Contains(ml, kw) {
			s += 5
			break
		}
	}
	return s
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
		sort.Slice(mlist, func(i, j int) bool {
			si, sj := flagshipScore(mlist[i]), flagshipScore(mlist[j])
			if si != sj {
				return si > sj // higher score first
			}
			return mlist[i] > mlist[j] // lexicographic descending as tiebreak
		})
		result[vendor] = mlist[0]
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
