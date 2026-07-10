package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProvidersNormal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	os.WriteFile(path, []byte(`[
		{"name": "Test", "base_url": "https://test.example.com/v1", "api_key": "sk-xxx"}
	]`), 0644)

	providers, err := LoadProviders(path)
	if err != nil {
		t.Fatalf("LoadProviders error: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("got %d providers, want 1", len(providers))
	}
	if providers[0].Name != "Test" {
		t.Errorf("name = %q, want Test", providers[0].Name)
	}
	if providers[0].APIKey != "sk-xxx" {
		t.Errorf("api_key = %q, want sk-xxx", providers[0].APIKey)
	}
}

func TestLoadProvidersClientMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	os.WriteFile(path, []byte(`[
		{"name":"CLI relay","base_url":"https://cli.example.com/v1","api_key":"sk-test","client_mode":"auto","codex_user_agent":"codex_cli_rs/9.9.9"}
	]`), 0644)

	providers, err := LoadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].ClientMode != "auto" || providers[0].CodexUserAgent != "codex_cli_rs/9.9.9" {
		t.Fatalf("providers = %#v", providers)
	}
}

func TestLoadProvidersDeduplicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	os.WriteFile(path, []byte(`[
		{"name": "First", "base_url": "https://example.com/v1", "api_key": "sk-1", "access_token": ""},
		{"name": "Second", "base_url": "https://example.com/v2", "api_key": "sk-2", "access_token": "tok-abc"}
	]`), 0644)

	providers, err := LoadProviders(path)
	if err != nil {
		t.Fatalf("LoadProviders error: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("got %d providers, want 1 (deduplicated)", len(providers))
	}
	// Should keep first entry's name and key
	if providers[0].Name != "First" {
		t.Errorf("name = %q, want First", providers[0].Name)
	}
	if providers[0].APIKey != "sk-1" {
		t.Errorf("api_key = %q, want sk-1", providers[0].APIKey)
	}
	// Should absorb second entry's access token
	if providers[0].AccessToken != "tok-abc" {
		t.Errorf("access_token = %q, want tok-abc", providers[0].AccessToken)
	}
}

func TestLoadProvidersEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	os.WriteFile(path, []byte(`[]`), 0644)

	providers, err := LoadProviders(path)
	if err != nil {
		t.Fatalf("LoadProviders error: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("got %d providers, want 0", len(providers))
	}
}

func TestLoadProvidersFileNotExist(t *testing.T) {
	providers, err := LoadProviders("/nonexistent/path/providers.json")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("got %d providers, want 0", len(providers))
	}
}

func TestLoadProvidersBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	os.WriteFile(path, []byte(`{not valid json`), 0644)

	_, err := LoadProviders(path)
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestExtractHost(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://example.com/v1", "example.com"},
		{"https://api.test.io:8080/v1", "api.test.io"},
		{"http://localhost:3000", "localhost"},
		{"not-a-url", ""},
	}
	for _, tt := range tests {
		got := extractHost(tt.url)
		if got != tt.want {
			t.Errorf("extractHost(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TLS verification must be on by default: the proxy forwards paid API keys,
// and a silent InsecureSkipVerify default hands them to any MITM. Opting out
// stays possible via ssl_verify = false.
func TestDefaultConfigVerifiesTLS(t *testing.T) {
	if !DefaultConfig().SSLVerify {
		t.Fatal("DefaultConfig().SSLVerify = false, want true")
	}
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.SSLVerify {
		t.Fatal("LoadConfig default SSLVerify = false, want true")
	}
}
