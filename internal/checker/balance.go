package checker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// BalanceInfo holds quota information from a new-api/one-api panel.
type BalanceInfo struct {
	Remaining float64
	Used      float64
}

// panelRoot strips /v1 or /v1/ suffix from a base URL to get the panel root.
func panelRoot(baseURL string) string {
	u := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(u, "/v1") {
		u = u[:len(u)-3]
	}
	return u
}

// DetectPlatform tries to identify if a relay station runs new-api or one-api.
func DetectPlatform(ctx context.Context, client *http.Client, baseURL string) string {
	root := panelRoot(baseURL)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Try /api/status endpoint (new-api specific)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/status", nil)
	if err == nil {
		req.Header.Set("User-Agent", UserAgent)
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			bodyStr := strings.ToLower(string(body))
			if resp.StatusCode == 200 && len(body) > 0 {
				if strings.Contains(bodyStr, "one_api") || strings.Contains(bodyStr, "one-api") {
					return "one-api"
				}
				if strings.Contains(bodyStr, "new_api") || strings.Contains(bodyStr, "new-api") {
					return "new-api"
				}
				// Has a status endpoint with JSON → likely new-api variant
				var js map[string]any
				if json.Unmarshal(body, &js) == nil {
					return "new-api"
				}
			}
		}
	}

	// Try fetching the panel homepage
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/", nil)
	if err == nil {
		req2.Header.Set("User-Agent", UserAgent)
		resp2, err := client.Do(req2)
		if err == nil {
			// Read at most 16KB
			limited := io.LimitReader(resp2.Body, 16384)
			body2, _ := io.ReadAll(limited)
			resp2.Body.Close()

			lower := strings.ToLower(string(body2))
			if strings.Contains(lower, "one-api") || strings.Contains(lower, "one_api") {
				return "one-api"
			}
			if strings.Contains(lower, "new-api") || strings.Contains(lower, "new_api") {
				return "new-api"
			}
		}
	}

	return "unknown"
}

// QueryBalance queries the remaining quota from a new-api/one-api panel.
// Returns nil if accessToken is empty or the query fails.
func QueryBalance(ctx context.Context, client *http.Client, baseURL, accessToken string) (*BalanceInfo, error) {
	if accessToken == "" {
		return nil, nil
	}

	root := panelRoot(baseURL)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/user/self", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Data struct {
			Quota     float64 `json:"quota"`
			UsedQuota float64 `json:"used_quota"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return &BalanceInfo{
		Remaining: result.Data.Quota,
		Used:      result.Data.UsedQuota,
	}, nil
}
