package checker

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"relay-monitor/internal/provider"
)

// FingerprintModel runs the 10-question fingerprint + self-ID probe on a single model.
func (e *Engine) FingerprintModel(ctx context.Context, baseURL, apiKey, modelID, apiFormat string) *FingerprintResult {
	r := &FingerprintResult{
		Model:   modelID,
		Vendor:  provider.IdentifyVendor(modelID),
		Answers: make(map[string]FingerprintAnswer),
		Scores:  make(map[string][2]int),
	}

	opts := ChatOptions{
		APIFormat:   apiFormat,
		MaxTokens:   200,
		Temperature: 0,
	}

	gateFailed := false
	networkErrors := 0

	for i, q := range FingerprintQuestions {
		if ctx.Err() != nil {
			break
		}

		start := time.Now()
		resp, _ := Chat(ctx, e.Client, baseURL, apiKey, modelID, q.Prompt, opts)
		elapsed := time.Since(start).Seconds()

		ans := FingerprintAnswer{
			Expected: q.Answer,
			TimeSec:  elapsed,
		}

		if resp == nil || !resp.OK {
			ans.NetworkError = true
			networkErrors++
			if q.Level == 1 {
				// L1 network error → gate_failed, stop
				gateFailed = true
				ans.Raw = ""
				if resp != nil {
					ans.Raw = resp.Error
				}
				r.Answers[q.ID] = ans
				r.Verdict = "NETWORK ERROR"
				r.GateFailed = true
				r.NetworkErrors = networkErrors
				computeScores(r)
				return r
			}
		} else {
			ans.Raw = resp.Content
			ans.Correct = q.Check(resp.Content)
		}

		r.Answers[q.ID] = ans

		// L1 is gate: if wrong, stop
		if q.Level == 1 && !ans.Correct && !ans.NetworkError {
			gateFailed = true
			r.GateFailed = true
			break
		}

		// 2s pause between questions
		if i < len(FingerprintQuestions)-1 {
			time.Sleep(2 * time.Second)
		}
	}

	r.NetworkErrors = networkErrors

	// Self-ID probe
	if !gateFailed && ctx.Err() == nil {
		time.Sleep(2 * time.Second)
		selfResp, _ := Chat(ctx, e.Client, baseURL, apiKey, modelID,
			"What is your exact model name and version? Who created you? Answer in one short sentence.", opts)
		if selfResp != nil && selfResp.OK {
			r.SelfID.Verdict, r.SelfID.Detail = JudgeSelfID(modelID, selfResp.Content)
		} else {
			r.SelfID.Verdict = "UNCLEAR"
			r.SelfID.Detail = "network error during self-id"
		}
	}

	computeScores(r)
	r.ExpectedTier, r.ExpectedMin = provider.ClassifyTier(modelID)
	r.Verdict = determineVerdict(r)

	return r
}

// computeScores calculates per-level scores and total from answers.
func computeScores(r *FingerprintResult) {
	levelCorrect := make(map[int]int)
	levelTotal := make(map[int]int)

	for _, q := range FingerprintQuestions {
		ans, ok := r.Answers[q.ID]
		if !ok {
			continue
		}
		if ans.NetworkError {
			continue // exclude network errors from scoring
		}
		levelTotal[q.Level]++
		if ans.Correct {
			levelCorrect[q.Level]++
		}
	}

	total := 0
	for level := 1; level <= 4; level++ {
		key := fmt.Sprintf("L%d", level)
		c, t := levelCorrect[level], levelTotal[level]
		r.Scores[key] = [2]int{c, t}
		total += c
	}
	r.TotalScore = total
}

func determineVerdict(r *FingerprintResult) string {
	if r.NetworkErrors >= 3 {
		return "NETWORK ISSUES"
	}
	if r.GateFailed {
		return "FAIL"
	}
	if r.SelfID.Verdict == "MISMATCH" {
		return "IDENTITY MISMATCH"
	}

	min := r.ExpectedMin
	switch {
	case r.TotalScore >= min:
		return "GENUINE"
	case r.TotalScore >= min-2:
		return "PLAUSIBLE"
	case r.TotalScore >= min-4:
		return "SUSPECTED DOWNGRADE"
	default:
		return "LIKELY FAKE"
	}
}

// JudgeSelfID checks if the model's self-identification matches the claimed vendor.
func JudgeSelfID(modelID, answer string) (verdict, detail string) {
	ansLow := strings.ToLower(answer)
	modelLow := strings.ToLower(modelID)

	// vendor keyword sets: if model contains key, answer should contain one of the values
	vendorKeywords := []struct {
		modelKey    string
		answerWords []string
	}{
		{"claude", []string{"claude", "anthropic"}},
		{"gpt", []string{"gpt", "openai"}},
		{"gemini", []string{"gemini", "google"}},
		{"grok", []string{"grok", "xai", "x.ai"}},
		{"deepseek", []string{"deepseek"}},
		{"glm", []string{"glm", "zhipu", "chatglm"}},
		{"kimi", []string{"kimi", "moonshot"}},
		{"qwen", []string{"qwen", "alibaba", "tongyi"}},
		{"minimax", []string{"minimax"}},
		{"llama", []string{"llama", "meta"}},
		{"mistral", []string{"mistral"}},
	}

	for _, vk := range vendorKeywords {
		if !strings.Contains(modelLow, vk.modelKey) {
			continue
		}
		for _, w := range vk.answerWords {
			if strings.Contains(ansLow, w) {
				return "MATCH", fmt.Sprintf("answer mentions %q, consistent with %s", w, modelID)
			}
		}
		return "MISMATCH", fmt.Sprintf("model claims %s but answer does not mention any of %v", modelID, vk.answerWords)
	}

	return "UNCLEAR", "no vendor keyword mapping for this model"
}

const fingerprintModelsPerVendor = 3

// RunFingerprintAll runs fingerprint on the given provider's top-ranked models.
// Returns results for each model tested.
func (e *Engine) RunFingerprintAll(ctx context.Context, client *http.Client, p provider.Provider, logFn func(string)) []*FingerprintResult {
	models, err := e.FetchModels(ctx, p.BaseURL, p.APIKey)
	if err != nil {
		logFn(fmt.Sprintf("[fingerprint] %s: failed to fetch models: %v", p.Name, err))
		return nil
	}

	var filtered []string
	for _, m := range models {
		if !provider.ShouldSkip(m) {
			filtered = append(filtered, m)
		}
	}

	targets := provider.PickTopModelsPerVendor(filtered, fingerprintModelsPerVendor)
	logFn(fmt.Sprintf("[fingerprint] %s: %d models, testing %d top targets (%d per vendor)", p.Name, len(filtered), len(targets), fingerprintModelsPerVendor))

	var results []*FingerprintResult
	for i, target := range targets {
		if ctx.Err() != nil {
			break
		}
		logFn(fmt.Sprintf("[fingerprint] %s [%d/%d] %s (%s)...", p.Name, i+1, len(targets), target.Model, target.Vendor))

		fr := e.FingerprintModel(ctx, p.BaseURL, p.APIKey, target.Model, p.APIFormat)
		fr.Provider = p.Name
		results = append(results, fr)

		log.Printf("[fingerprint] %s %s → %s (score %d/%d, self-id: %s)",
			p.Name, target.Model, fr.Verdict, fr.TotalScore, fr.ExpectedMin, fr.SelfID.Verdict)
	}

	return results
}
