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
		SSLVerify:        false,
		DataDir:          ".",
		BalanceThreshold: 5.0,
		ProvidersFile:    "providers.json",
		Proxy: ProxyConfig{
			RequestTimeout:         Duration{30 * time.Second},
			StreamFirstByteTimeout: Duration{30 * time.Second},
			StreamIdleTimeout:      Duration{60 * time.Second},
			MaxRetries:             2,
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
	changed := false
	for _, p := range raw {
		host := extractHost(p.BaseURL)
		if idx, exists := seen[host]; exists {
			// Merge: keep existing, absorb missing fields from duplicate
			changed = true
			if p.AccessToken != "" && providers[idx].AccessToken == "" {
				providers[idx].AccessToken = p.AccessToken
			}
			if p.LastKnownQuota > 0 && providers[idx].LastKnownQuota == 0 {
				providers[idx].LastKnownQuota = p.LastKnownQuota
			}
			continue
		}
		seen[host] = len(providers)
		providers = append(providers, p)
	}

	// Auto-save if duplicates were removed
	if changed {
		SaveProviders(path, providers)
	}
	return providers, nil
}

func extractHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	return u.Hostname()
}

// SameHost checks if two base URLs point to the same hostname.
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
