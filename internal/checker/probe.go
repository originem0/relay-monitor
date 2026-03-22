package checker

import "strings"

// Basic test probe — the chicken-and-rabbit problem.
const TestPrompt = "A farm has 17 chickens and 23 rabbits. How many legs in total? Answer with only the number."
const CorrectNum = 126

// FingerprintQuestion defines one item in the 10-question capability fingerprint.
type FingerprintQuestion struct {
	ID     string
	Level  int
	Prompt string
	Answer string
	Check  func(string) bool
}

// FingerprintQuestions contains all 10 questions from the Python source (lines 797-862).
var FingerprintQuestions = []FingerprintQuestion{
	// L1: gate
	{
		ID: "chicken_rabbit", Level: 1,
		Prompt: "A farm has 17 chickens and 23 rabbits. How many legs in total? Answer with only the number.",
		Answer: "126",
		Check:  func(ans string) bool { return CheckNum(ans, 126, 0.01) },
	},
	// L2: arithmetic traps
	{
		ID: "multi_step_arith", Level: 2,
		Prompt: "Calculate: (37 * 45) + (128 / 8) - (56 * 3). Answer with only the number.",
		Answer: "1513",
		Check:  func(ans string) bool { return CheckNum(ans, 1513, 0.01) },
	},
	{
		ID: "markup_discount", Level: 2,
		Prompt: "An item costs $100. A store marks it up by 30%, then offers a 25% discount on the marked-up price. What is the final price in dollars? Answer with only the number.",
		Answer: "97.5",
		Check:  func(ans string) bool { return CheckNum(ans, 97.5, 0.01) },
	},
	{
		ID: "bat_ball", Level: 2,
		Prompt: "A bat and a ball cost $1.10 in total. The bat costs $1.00 more than the ball. How much does the ball cost in dollars? Answer with only the number.",
		Answer: "0.05",
		Check: func(ans string) bool {
			return CheckNum(ans, 0.05, 0.01) || (strings.Contains(ans, "5") && strings.Contains(strings.ToLower(ans), "cent"))
		},
	},
	// L3: reasoning gradient
	{
		ID: "left_handed", Level: 3,
		Prompt: "In a room of 100 people, 99 are left-handed. How many left-handed people must leave so that exactly 98% of the remaining are left-handed? Answer with only the number.",
		Answer: "50",
		Check:  func(ans string) bool { return CheckNum(ans, 50, 0.01) },
	},
	{
		ID: "code_trace", Level: 3,
		Prompt: "What does this Python code print?\nx = [1, 2, 3, 4, 5]\nprint(sum(x[1::2]))\nAnswer with only the number.",
		Answer: "6",
		Check:  func(ans string) bool { return CheckNum(ans, 6, 0.01) },
	},
	{
		ID: "snail_well", Level: 3,
		Prompt: "A snail is at the bottom of a 30-foot well. Each day it climbs up 3 feet, but each night it slips back 2 feet. On which day does the snail reach the top? Answer with only the number.",
		Answer: "28",
		Check:  func(ans string) bool { return CheckNum(ans, 28, 0.01) },
	},
	// L4: advanced computation
	{
		ID: "prime_sum", Level: 4,
		Prompt: "What is the sum of all prime numbers less than 50? Answer with only the number.",
		Answer: "328",
		Check:  func(ans string) bool { return CheckNum(ans, 328, 0.01) },
	},
	{
		ID: "digit_count", Level: 4,
		Prompt: "How many times does the digit '1' appear when you write out all integers from 1 to 200? Answer with only the number.",
		Answer: "140",
		Check:  func(ans string) bool { return CheckNum(ans, 140, 0.01) },
	},
	{
		ID: "power_sum", Level: 4,
		Prompt: "What is 2^10 + 3^5 + 7^3? Answer with only the number.",
		Answer: "1610",
		Check:  func(ans string) bool { return CheckNum(ans, 1610, 0.01) },
	},
}

// VerifyProbe defines a single verification probe for the --verify mode.
type VerifyProbe struct {
	Name           string
	Prompt         string
	Check          func(string) bool // nil for self_id (judged separately)
	CorrectDisplay string
	WrongTrap      string
}

// VerifyProbes contains the 3 probes from Python (lines 532-551).
var VerifyProbes = []VerifyProbe{
	{
		Name:   "self_id",
		Prompt: "What is your exact model name and version? Who created you? Answer in one short sentence.",
		Check:  nil,
	},
	{
		Name:           "bat_ball",
		Prompt:         "A bat and a ball cost $1.10 in total. The bat costs $1.00 more than the ball. How much does the ball cost? Answer with only the dollar amount.",
		CorrectDisplay: "$0.05",
		WrongTrap:      "$0.10",
		Check: func(ans string) bool {
			return CheckNum(ans, 0.05, 0.01) ||
				strings.Contains(strings.ToLower(ans), "5 cents") ||
				strings.Contains(strings.ToLower(ans), "five cents")
		},
	},
	{
		Name:           "counter_intuitive",
		Prompt:         "In a room of 100 people, 99 are left-handed. How many left-handed people must leave so that exactly 98% of the remaining people are left-handed? Answer with only the number.",
		CorrectDisplay: "50",
		Check:          func(ans string) bool { return CheckNum(ans, 50, 0.01) },
	},
}
