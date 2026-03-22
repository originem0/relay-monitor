package checker

import "time"

// ChatResponse is the normalized result of a single chat API call.
type ChatResponse struct {
	OK        bool
	Content   string
	Reasoning string
	Code      int
	Elapsed   time.Duration
	Error     string
}

// TestResult captures the outcome of testing one model with the basic probe.
type TestResult struct {
	Model        string `json:"model"`
	Vendor       string `json:"vendor"`
	Status       string `json:"status"`       // "ok" | "error"
	Correct      bool   `json:"correct"`
	Answer       string `json:"answer,omitempty"`
	LatencyMs    int64  `json:"latency_ms"`
	Error        string `json:"error,omitempty"`
	HasReasoning bool   `json:"has_reasoning,omitempty"`
}

// FingerprintAnswer records a single fingerprint question result.
type FingerprintAnswer struct {
	Raw          string  `json:"raw"`
	Correct      bool    `json:"correct"`
	NetworkError bool    `json:"network_error"`
	Expected     string  `json:"expected"`
	TimeSec      float64 `json:"time"`
}

// FingerprintResult holds the full fingerprint assessment of one model.
type FingerprintResult struct {
	Model         string                       `json:"model"`
	Vendor        string                       `json:"vendor"`
	Provider      string                       `json:"provider,omitempty"`
	Answers       map[string]FingerprintAnswer  `json:"answers"`
	Scores        map[string][2]int            `json:"scores"`        // "L1" -> [correct, total]
	GateFailed    bool                         `json:"gate_failed"`
	SelfID        SelfIDResult                 `json:"self_id"`
	TotalScore    int                          `json:"total_score"`
	NetworkErrors int                          `json:"network_errors"`
	ExpectedTier  string                       `json:"expected_tier"`
	ExpectedMin   int                          `json:"expected_min"`
	Verdict       string                       `json:"verdict"`
}

// SelfIDResult captures the self-identification probe outcome.
type SelfIDResult struct {
	Verdict string `json:"verdict"`
	Detail  string `json:"detail"`
}

// ProviderResult aggregates test outcomes for a single relay provider.
type ProviderResult struct {
	Provider   string       `json:"provider"`
	BaseURL    string       `json:"base_url"`
	ModelsFound int         `json:"models_found"`
	Error      string       `json:"error,omitempty"`
	Results    []TestResult `json:"results"`
}
