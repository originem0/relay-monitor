package checker

import "testing"

func makeResult(answers map[string]FingerprintAnswer, gateFailed bool, networkErrors int, selfID string, expectedMin int) *FingerprintResult {
	r := &FingerprintResult{
		Model:         "test-model",
		Answers:       answers,
		Scores:        make(map[string][2]int),
		GateFailed:    gateFailed,
		NetworkErrors: networkErrors,
		ExpectedMin:   expectedMin,
		SelfID:        SelfIDResult{Verdict: selfID},
	}
	computeScores(r)
	return r
}

func TestComputeScores(t *testing.T) {
	// Build answers: L1 correct, L2 2/3 correct, L3 1/3, L4 0/3
	answers := make(map[string]FingerprintAnswer)
	for _, q := range FingerprintQuestions {
		ans := FingerprintAnswer{Expected: q.Answer}
		switch {
		case q.Level == 1:
			ans.Correct = true
		case q.Level == 2 && q.ID != "bat_ball":
			ans.Correct = true
		case q.Level == 3 && q.ID == "left_handed":
			ans.Correct = true
		default:
			ans.Correct = false
		}
		answers[q.ID] = ans
	}

	r := &FingerprintResult{
		Answers: answers,
		Scores:  make(map[string][2]int),
	}
	computeScores(r)

	if r.Scores["L1"] != [2]int{1, 1} {
		t.Errorf("L1 = %v, want [1 1]", r.Scores["L1"])
	}
	if r.Scores["L2"] != [2]int{2, 3} {
		t.Errorf("L2 = %v, want [2 3]", r.Scores["L2"])
	}
	if r.Scores["L3"] != [2]int{1, 3} {
		t.Errorf("L3 = %v, want [1 3]", r.Scores["L3"])
	}
	if r.Scores["L4"] != [2]int{0, 3} {
		t.Errorf("L4 = %v, want [0 3]", r.Scores["L4"])
	}
	if r.TotalScore != 4 {
		t.Errorf("TotalScore = %d, want 4", r.TotalScore)
	}
}

func TestComputeScoresExcludesNetworkErrors(t *testing.T) {
	answers := make(map[string]FingerprintAnswer)
	for _, q := range FingerprintQuestions {
		ans := FingerprintAnswer{Expected: q.Answer, Correct: true}
		if q.Level == 4 {
			ans.Correct = false
			ans.NetworkError = true
		}
		answers[q.ID] = ans
	}

	r := &FingerprintResult{Answers: answers, Scores: make(map[string][2]int)}
	computeScores(r)

	// L4 has 3 questions, all network errors → excluded from total
	if r.Scores["L4"] != [2]int{0, 0} {
		t.Errorf("L4 = %v, want [0 0] (network errors excluded)", r.Scores["L4"])
	}
	if r.TotalScore != 7 {
		t.Errorf("TotalScore = %d, want 7 (L1:1 + L2:3 + L3:3)", r.TotalScore)
	}
}

func TestDetermineVerdict(t *testing.T) {
	tests := []struct {
		name         string
		totalScore   int
		expectedMin  int
		gateFailed   bool
		networkErrs  int
		selfID       string
		wantVerdict  string
	}{
		{"genuine", 9, 9, false, 0, "MATCH", "GENUINE"},
		{"genuine above min", 10, 7, false, 0, "MATCH", "GENUINE"},
		{"plausible", 7, 9, false, 0, "MATCH", "PLAUSIBLE"},
		{"suspected", 5, 9, false, 0, "MATCH", "SUSPECTED DOWNGRADE"},
		{"likely fake", 2, 9, false, 0, "MATCH", "LIKELY FAKE"},
		{"gate failed", 0, 9, true, 0, "", "FAIL"},
		{"network issues", 0, 9, false, 3, "", "NETWORK ISSUES"},
		{"identity mismatch", 8, 7, false, 0, "MISMATCH", "IDENTITY MISMATCH"},
		// Priority: network > gate > mismatch > score
		{"network beats gate", 0, 9, true, 3, "", "NETWORK ISSUES"},
		{"gate beats mismatch", 0, 9, true, 1, "MISMATCH", "FAIL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &FingerprintResult{
				TotalScore:    tt.totalScore,
				ExpectedMin:   tt.expectedMin,
				GateFailed:    tt.gateFailed,
				NetworkErrors: tt.networkErrs,
				SelfID:        SelfIDResult{Verdict: tt.selfID},
			}
			got := determineVerdict(r)
			if got != tt.wantVerdict {
				t.Errorf("determineVerdict() = %q, want %q", got, tt.wantVerdict)
			}
		})
	}
}

func TestJudgeSelfID(t *testing.T) {
	tests := []struct {
		model       string
		answer      string
		wantVerdict string
	}{
		{"gpt-5.2", "I am GPT-5.2, created by OpenAI.", "MATCH"},
		{"gpt-5.2", "I am Claude, made by Anthropic.", "MISMATCH"},
		{"claude-sonnet-4", "I'm Claude, made by Anthropic.", "MATCH"},
		{"claude-sonnet-4", "I am GPT-4 by OpenAI.", "MISMATCH"},
		{"deepseek-v3", "I am DeepSeek-V3.", "MATCH"},
		{"gpt-5.2", "I am an AI assistant.", "MISMATCH"}, // model contains "gpt" but answer has no gpt/openai
		{"some-unknown-model", "I am an AI.", "UNCLEAR"},
	}

	for _, tt := range tests {
		t.Run(tt.model+"_"+tt.wantVerdict, func(t *testing.T) {
			verdict, _ := JudgeSelfID(tt.model, tt.answer)
			if verdict != tt.wantVerdict {
				t.Errorf("JudgeSelfID(%q, %q) verdict = %q, want %q", tt.model, tt.answer, verdict, tt.wantVerdict)
			}
		})
	}
}
