package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"relay-monitor/internal/provider"
)

// Engine orchestrates model testing across providers.
type Engine struct {
	Client            *http.Client
	MaxConcurrency    int
	RequestInterval   time.Duration
	MaxModelsPerCheck int // 0 = no limit; >0 = randomly sample this many flagships per provider
}

// CheckMode controls what gets tested.
type CheckMode int

const (
	// ModeQuick tests only flagship models (one per vendor per provider). For scheduled checks.
	ModeQuick CheckMode = iota
	// ModeFull tests all models on every provider. For manual deep checks.
	ModeFull
)

// FetchModels discovers available models via /v1/models.
// Retries up to 3 times, but not for 401/403/404.
func (e *Engine) FetchModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"

	noRetry := map[int]bool{401: true, 403: true, 404: true}
	var lastErr string

	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("User-Agent", UserAgent)

		resp, err := e.Client.Do(req)
		if err != nil {
			lastErr = DiagnoseError(0, err.Error(), apiKey)
			if attempt < 2 {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var data struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &data); err != nil {
				return nil, fmt.Errorf("parse models JSON: %w", err)
			}
			if len(data.Data) == 0 {
				return nil, fmt.Errorf("models list empty")
			}
			models := make([]string, len(data.Data))
			for i, m := range data.Data {
				models[i] = m.ID
			}
			sort.Strings(models)
			return models, nil
		}

		lastErr = DiagnoseError(resp.StatusCode, string(body), apiKey)
		if noRetry[resp.StatusCode] {
			break
		}
		if attempt < 2 {
			time.Sleep(2 * time.Second)
		}
	}
	return nil, fmt.Errorf("%s", lastErr)
}

// TestModel runs the basic test on a single model.
// For GPT-5+ models, tries responses API first (OpenAI's default).
// If any format returns 400, falls back to the other format.
func (e *Engine) TestModel(ctx context.Context, baseURL, apiKey, modelID, apiFormat string) *TestResult {
	// Pick a random question to avoid sending the same prompt to every model
	q := RandomTestQuestion()

	// GPT-5+ and codex models prefer responses API
	effectiveFormat := apiFormat
	if effectiveFormat == "" || effectiveFormat == "chat" {
		low := strings.ToLower(modelID)
		if strings.Contains(low, "gpt-5") || strings.Contains(low, "codex") {
			effectiveFormat = "responses"
		}
	}

	r, _ := Chat(ctx, e.Client, baseURL, apiKey, modelID, q.Prompt, ChatOptions{
		APIFormat:   effectiveFormat,
		MaxTokens:   200,
		Temperature: 0,
	})

	// If failed with 400/500, try the other format
	if !r.OK && (r.Code == 400 || r.Code == 500) {
		altFormat := "responses"
		if effectiveFormat == "responses" {
			altFormat = "chat"
		}
		r2, _ := Chat(ctx, e.Client, baseURL, apiKey, modelID, q.Prompt, ChatOptions{
			APIFormat:   altFormat,
			MaxTokens:   200,
			Temperature: 0,
		})
		if r2.OK {
			r = r2
		}
	}

	result := &TestResult{
		Model:     modelID,
		Vendor:    provider.IdentifyVendor(modelID),
		LatencyMs: r.Elapsed.Milliseconds(),
	}

	if r.OK {
		result.Status = "ok"
		if len(r.Content) > 300 {
			result.Answer = r.Content[:300]
		} else {
			result.Answer = r.Content
		}
		result.Correct = CheckNum(r.Content, q.Expected, 0.01)
		result.HasReasoning = r.Reasoning != ""
	} else {
		result.Status = "error"
		result.Error = r.Error
	}

	return result
}

// TestProvider tests models on a single provider sequentially.
// In ModeQuick, only tests flagship models (one per vendor).
func (e *Engine) TestProvider(ctx context.Context, p provider.Provider, mode CheckMode, logFn func(string)) *ProviderResult {
	result := &ProviderResult{
		Provider: p.Name,
		BaseURL:  p.BaseURL,
	}

	allModels, err := e.FetchModels(ctx, p.BaseURL, p.APIKey)
	if err != nil {
		result.Error = err.Error()
		logFn(fmt.Sprintf("%s: %s", p.Name, err))
		return result
	}

	// Filter non-chat models
	var filtered []string
	for _, m := range allModels {
		if !provider.ShouldSkip(m) {
			filtered = append(filtered, m)
		}
	}
	result.ModelsFound = len(filtered)

	// In quick mode, only test flagships
	models := filtered
	if mode == ModeQuick {
		flagships := provider.PickFlagships(filtered)
		models = make([]string, 0, len(flagships))
		for _, m := range flagships {
			models = append(models, m)
		}
		sort.Strings(models)

		// Cap the number of models tested per run to limit request volume.
		// When capped, shuffle and take the first N so coverage rotates across runs.
		if e.MaxModelsPerCheck > 0 && len(models) > e.MaxModelsPerCheck {
			rand.Shuffle(len(models), func(i, j int) { models[i], models[j] = models[j], models[i] })
			models = models[:e.MaxModelsPerCheck]
			sort.Strings(models) // re-sort for stable log output
			logFn(fmt.Sprintf("%s: %d models found, testing %d/%d flagships (capped)", p.Name, len(filtered), len(models), len(flagships)))
		} else {
			logFn(fmt.Sprintf("%s: %d models found, testing %d flagships", p.Name, len(filtered), len(models)))
		}
	} else {
		logFn(fmt.Sprintf("%s: %d models, full test", p.Name, len(models)))
	}

	for i, mid := range models {
		if ctx.Err() != nil {
			break
		}

		tr := e.TestModel(ctx, p.BaseURL, p.APIKey, mid, p.APIFormat)
		result.Results = append(result.Results, *tr)

		if tr.Status == "ok" {
			tag := "WRONG"
			if tr.Correct {
				tag = "OK"
			}
			ans := tr.Answer
			if len(ans) > 50 {
				ans = ans[:50]
			}
			ans = strings.ReplaceAll(ans, "\n", " ")
			extra := ""
			if tr.HasReasoning {
				extra = " [reasoning]"
			}
			logFn(fmt.Sprintf("  %s [%d/%d] %s ... %s  %s  (%.2fs)%s",
				p.Name, i+1, len(models), mid, tag, ans,
				float64(tr.LatencyMs)/1000, extra))
		} else {
			errMsg := tr.Error
			if len(errMsg) > 80 {
				errMsg = errMsg[:80]
			}
			logFn(fmt.Sprintf("  %s [%d/%d] %s ... FAIL  %s",
				p.Name, i+1, len(models), mid, errMsg))
		}

		// Smart sleep: deduct elapsed time from interval
		if i < len(models)-1 {
			elapsed := time.Duration(tr.LatencyMs) * time.Millisecond
			remaining := e.RequestInterval - elapsed
			if remaining < 500*time.Millisecond {
				remaining = 500 * time.Millisecond
			}
			time.Sleep(remaining)
		}
	}

	ok := 0
	correct := 0
	for _, r := range result.Results {
		if r.Status == "ok" {
			ok++
		}
		if r.Correct {
			correct++
		}
	}
	logFn(fmt.Sprintf("  --- %s: %d/%d available, %d/%d correct ---",
		p.Name, ok, len(models), correct, len(models)))

	return result
}

// RunCheck runs the test on all providers concurrently.
// onResult is called as each provider finishes (from the provider's goroutine).
func (e *Engine) RunCheck(ctx context.Context, providers []provider.Provider, mode CheckMode, logFn func(string), onResult func(*ProviderResult)) {
	sem := make(chan struct{}, e.MaxConcurrency)
	var wg sync.WaitGroup

	for _, p := range providers {
		wg.Add(1)
		go func(p provider.Provider) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pr := e.TestProvider(ctx, p, mode, logFn)
			if onResult != nil {
				onResult(pr)
			}
		}(p)
	}

	wg.Wait()
}

// RunBasicCheck is a convenience wrapper that collects all results (for CLI mode).
func (e *Engine) RunBasicCheck(ctx context.Context, providers []provider.Provider, logFn func(string)) []*ProviderResult {
	var mu sync.Mutex
	var results []*ProviderResult
	e.RunCheck(ctx, providers, ModeFull, logFn, func(pr *ProviderResult) {
		mu.Lock()
		results = append(results, pr)
		mu.Unlock()
	})
	return results
}
