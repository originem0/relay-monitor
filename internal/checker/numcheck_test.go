package checker

import "testing"

func TestCheckNum(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected float64
		tol      float64
		want     bool
	}{
		{"pure number", "126", 126, 0.01, true},
		{"with text", "答案是126", 126, 0.01, true},
		{"dollar sign", "$97.5", 97.5, 0.01, true},
		{"decimal", "0.05", 0.05, 0.01, true},
		{"negative", "-3.14", -3.14, 0.01, true},
		{"multiple numbers picks correct", "鸡有23只, 兔有23只, 共126条腿", 126, 0.01, true},
		{"markdown code block", "```\n126\n```", 126, 0.01, true},
		{"empty string", "", 126, 0.01, false},
		{"json array prefix", "[1,2,3]", 1, 0.01, false},
		{"outside tolerance", "99", 100, 0.01, false},
		{"within tolerance", "100.005", 100, 0.01, true},
		{"whitespace", "  126  ", 126, 0.01, true},
		{"dollar with space", "$ 97.5", 97.5, 0.01, true},
		{"large number", "1513", 1513, 0.01, true},
		{"zero", "0", 0, 0.01, true},
		{"text only no number", "no number here", 42, 0.01, false},
		{"number in sentence", "The answer is 328.", 328, 0.01, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckNum(tt.text, tt.expected, tt.tol)
			if got != tt.want {
				t.Errorf("CheckNum(%q, %v, %v) = %v, want %v", tt.text, tt.expected, tt.tol, got, tt.want)
			}
		})
	}
}
