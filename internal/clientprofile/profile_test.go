package clientprofile

import (
	"net/http"
	"testing"
)

func TestGenericProfileDoesNotLeakCLIHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	Config{}.ApplyHeaders(req, "gpt-5.6-sol")
	if req.Header.Get("User-Agent") != DefaultGenericUserAgent {
		t.Fatalf("generic User-Agent = %q", req.Header.Get("User-Agent"))
	}
	for _, name := range []string{"originator", "anthropic-version", "anthropic-beta"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("generic profile leaked %s=%q", name, got)
		}
	}
}

func TestProviderScopedOverridesReplaceVersionedDefaults(t *testing.T) {
	codexReq, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	Config{Mode: ModeCodex, CodexUserAgent: "codex_cli_rs/9.9.9", CodexOriginator: "future_codex"}.ApplyHeaders(codexReq, "gpt-5.6-sol")
	if codexReq.Header.Get("User-Agent") != "codex_cli_rs/9.9.9" || codexReq.Header.Get("originator") != "future_codex" {
		t.Fatalf("Codex override not applied: %#v", codexReq.Header)
	}

	claudeReq, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	Config{
		Mode: ModeClaudeCode, ClaudeCodeUserAgent: "claude-code/9.9.9",
		AnthropicVersion: "2099-01-01", AnthropicBeta: "future-beta",
	}.ApplyHeaders(claudeReq, "sonnet5")
	if claudeReq.Header.Get("User-Agent") != "claude-code/9.9.9" ||
		claudeReq.Header.Get("anthropic-version") != "2099-01-01" ||
		claudeReq.Header.Get("anthropic-beta") != "future-beta" {
		t.Fatalf("Claude overrides not applied: %#v", claudeReq.Header)
	}
}

func TestAutoModeResolvesPerModel(t *testing.T) {
	if got := ResolveMode(ModeAuto, "opus4.8"); got != ModeClaudeCode {
		t.Fatalf("opus mode = %q", got)
	}
	if got := ResolveMode(ModeAuto, "gpt-5.6-sol"); got != ModeCodex {
		t.Fatalf("gpt mode = %q", got)
	}
}
