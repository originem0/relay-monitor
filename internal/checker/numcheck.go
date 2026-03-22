package checker

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var numRe = regexp.MustCompile(`-?\d+\.?\d*`)

// CheckNum extracts numbers from text and returns true if any match expected
// within tolerance. Port of Python _check_num (lines 329-345):
//  1. Try whole text (stripped, dollar-sign stripped) as a float.
//  2. Regex-find all numbers and check each.
func CheckNum(text string, expected float64, tol float64) bool {
	if text == "" || strings.HasPrefix(text, "[") {
		return false
	}
	clean := strings.TrimSpace(text)
	clean = strings.Trim(clean, "$")
	clean = strings.TrimSpace(clean)

	if v, err := strconv.ParseFloat(clean, 64); err == nil {
		if math.Abs(v-expected) < tol {
			return true
		}
	}

	for _, m := range numRe.FindAllString(text, -1) {
		if v, err := strconv.ParseFloat(m, 64); err == nil {
			if math.Abs(v-expected) < tol {
				return true
			}
		}
	}
	return false
}
