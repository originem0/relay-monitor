package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"relay-monitor/internal/provider"
)

// Duration wraps time.Duration for TOML string parsing ("30m", "4s", etc.).
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// ProxyConfig holds reverse-proxy settings.
type ProxyConfig struct {
	Enabled                bool     `toml:"enabled"`
	APIKey                 string   `toml:"api_key"`
	RequestTimeout         Duration `toml:"request_timeout"`
	StreamFirstByteTimeout Duration `toml:"stream_first_byte_timeout"`
	StreamIdleTimeout      Duration `toml:"stream_idle_timeout"`
	MaxRetries             int      `toml:"max_retries"`
	MaxRequestBodyBytes    int64    `toml:"max_request_body_bytes"`
	MaxResponsesBodyBytes  int64    `toml:"max_responses_body_bytes"`
}

// AppConfig holds all application settings.
type AppConfig struct {
	Listen           string      `toml:"listen"`
	CheckInterval    Duration    `toml:"check_interval"`
	RetentionDays    int         `toml:"retention_days"`
	MaxConcurrency   int         `toml:"max_concurrency"`
	RequestInterval  Duration    `toml:"request_interval"`
	SSLVerify        bool        `toml:"ssl_verify"`
	DataDir          string      `toml:"data_dir"`
	BalanceThreshold float64     `toml:"balance_threshold"`
	ProvidersFile    string      `toml:"providers_file"`
	ExternalURL      string      `toml:"external_url"`
	Proxy            ProxyConfig `toml:"proxy"`
}

// DefaultConfig returns an AppConfig with sensible defaults.
func DefaultConfig() *AppConfig {
	return &AppConfig{
		Listen:           ":8080",
		CheckInterval:    Duration{8 * time.Hour},
		RetentionDays:    7,
		MaxConcurrency:   16,
		RequestInterval:  Duration{2 * time.Second},
		SSLVerify:        true,
		DataDir:          ".",
		BalanceThreshold: 5.0,
		ProvidersFile:    "providers.json",
		Proxy: ProxyConfig{
			RequestTimeout:         Duration{30 * time.Second},
			StreamFirstByteTimeout: Duration{30 * time.Second},
			StreamIdleTimeout:      Duration{60 * time.Second},
			MaxRetries:             2,
			MaxRequestBodyBytes:    8 * 1024 * 1024,
			MaxResponsesBodyBytes:  2 * 1024 * 1024,
		},
	}
}

// LoadConfig reads a TOML config file. Missing fields keep their defaults.
// If the file doesn't exist, returns DefaultConfig.
func LoadConfig(path string) (*AppConfig, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Clamp values that would break the runtime if mis-set. A zero
	// max_concurrency deadlocks the checker (unbuffered semaphore released only
	// by the same goroutine); a zero/negative retention_days makes Cleanup wipe
	// all history. Fall back to the documented defaults rather than honoring an
	// obviously invalid value.
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = DefaultConfig().MaxConcurrency
	}
	if cfg.RetentionDays < 1 {
		cfg.RetentionDays = DefaultConfig().RetentionDays
	}
	if cfg.RequestInterval.Duration < 0 {
		cfg.RequestInterval.Duration = 0
	}

	return cfg, nil
}

// LoadProviders reads providers from a JSON file, deduplicating by hostname.
// Returns an empty slice (not nil) if the file doesn't exist.
func LoadProviders(path string) ([]provider.Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []provider.Provider{}, nil
		}
		return nil, fmt.Errorf("read providers: %w", err)
	}

	var raw []provider.Provider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse providers: %w", err)
	}

	// Deduplicate by hostname
	seen := make(map[string]int) // hostname -> index in result
	var providers []provider.Provider
	for _, p := range raw {
		host := extractHost(p.BaseURL)
		if idx, exists := seen[host]; exists {
			// Merge: keep existing, absorb missing fields from duplicate.
			if p.AccessToken != "" && providers[idx].AccessToken == "" {
				providers[idx].AccessToken = p.AccessToken
			}
			if p.LastKnownQuota > 0 && providers[idx].LastKnownQuota == 0 {
				providers[idx].LastKnownQuota = p.LastKnownQuota
			}
			if p.ClientMode != "" && providers[idx].ClientMode == "" {
				providers[idx].ClientMode = p.ClientMode
			}
			if p.CodexUserAgent != "" && providers[idx].CodexUserAgent == "" {
				providers[idx].CodexUserAgent = p.CodexUserAgent
			}
			if p.CodexOriginator != "" && providers[idx].CodexOriginator == "" {
				providers[idx].CodexOriginator = p.CodexOriginator
			}
			if p.ClaudeCodeUserAgent != "" && providers[idx].ClaudeCodeUserAgent == "" {
				providers[idx].ClaudeCodeUserAgent = p.ClaudeCodeUserAgent
			}
			if p.AnthropicVersion != "" && providers[idx].AnthropicVersion == "" {
				providers[idx].AnthropicVersion = p.AnthropicVersion
			}
			if p.AnthropicBeta != "" && providers[idx].AnthropicBeta == "" {
				providers[idx].AnthropicBeta = p.AnthropicBeta
			}
			continue
		}
		seen[host] = len(providers)
		providers = append(providers, p)
	}

	// Dedup is applied in-memory only. A read must not mutate the file on disk;
	// persisting the deduped list is left to explicit writes (add/remove/edit).
	return providers, nil
}

func extractHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	return u.Hostname()
}

// SameHost checks if two base URLs point to the same hostname. Providers on the
// same host are treated as one station (same account/key/balance) even when
// their base paths differ (e.g. /v1 vs /v2) — see LoadProviders dedup.
func SameHost(a, b string) bool {
	return extractHost(a) == extractHost(b)
}

// SaveProviders writes providers to a JSON file with readable formatting.
func SaveProviders(path string, providers []provider.Provider) error {
	data, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal providers: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}
