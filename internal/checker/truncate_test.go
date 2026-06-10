package checker

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"under limit", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"ascii truncate", "hello world", 5, "hello"},
		{"n zero", "hello", 0, ""},
		{"n negative", "hello", -1, ""},
		{"cjk no split", "你好世界朋友", 3, "你好世"},
		{"cjk under limit", "你好", 5, "你好"},
		{"mixed ascii+cjk", "ab你好cd", 3, "ab你"},
	}
	for _, tt := range tests {
		if got := TruncateRunes(tt.s, tt.n); got != tt.want {
			t.Errorf("%s: TruncateRunes(%q, %d) = %q, want %q", tt.name, tt.s, tt.n, got, tt.want)
		}
	}

	// The whole reason this helper exists: a byte-slice truncation of CJK text
	// would split a multi-byte rune and yield invalid UTF-8. Guard against that.
	for _, n := range []int{1, 2, 3, 4, 5} {
		if out := TruncateRunes("你好世界朋友", n); !utf8.ValidString(out) {
			t.Errorf("TruncateRunes(cjk, %d) = %q is not valid UTF-8", n, out)
		}
	}
}
