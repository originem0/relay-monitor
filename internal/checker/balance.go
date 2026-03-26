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
// Tries /api/user/self first (needs access_token), then falls back to
// /v1/dashboard/billing/* (works with API key on most new-api forks).
func QueryBalance(ctx context.Context, client *http.Client, baseURL, accessToken, apiKey string) (*BalanceInfo, error) {
	root := panelRoot(baseURL)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Method 1: /api/user/self with access_token (classic new-api/one-api)
	if accessToken != "" {
		bi, err := queryBalanceUserSelf(ctx, client, root, accessToken)
		if bi != nil {
			return bi, nil
		}
		_ = err // fall through to method 2
	}

	// Method 2: /v1/dashboard/billing/* with API key (OpenAI-compatible billing)
	if apiKey != "" {
		bi, err := queryBalanceBilling(ctx, client, baseURL, apiKey)
		if bi != nil {
			return bi, nil
		}
		return nil, err
	}

	return nil, nil
}

func queryBalanceUserSelf(ctx context.Context, client *http.Client, root, accessToken string) (*BalanceInfo, error) {
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
		io.ReadAll(resp.Body)
		return nil, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Quota     float64 `json:"quota"`
			UsedQuota float64 `json:"used_quota"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, nil
	}
	if !result.Success {
		return nil, nil
	}

	return &BalanceInfo{
		Remaining: result.Data.Quota,
		Used:      result.Data.UsedQuota,
	}, nil
}

func queryBalanceBilling(ctx context.Context, client *http.Client, baseURL, accessToken string) (*BalanceInfo, error) {
	base := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}

	// Use accessToken as Bearer auth for billing endpoint
	auth := accessToken

	subReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/dashboard/billing/subscription", nil)
	if err != nil {
		return nil, err
	}
	subReq.Header.Set("Authorization", "Bearer "+auth)
	subReq.Header.Set("User-Agent", UserAgent)

	subResp, err := client.Do(subReq)
	if err != nil {
		return nil, err
	}
	defer subResp.Body.Close()

	if subResp.StatusCode != 200 {
		io.ReadAll(subResp.Body)
		return nil, nil
	}

	var sub struct {
		HardLimitUSD float64 `json:"hard_limit_usd"`
	}
	body, _ := io.ReadAll(subResp.Body)
	if json.Unmarshal(body, &sub) != nil || sub.HardLimitUSD <= 0 {
		return nil, nil
	}

	// Query usage for current month
	now := time.Now()
	startDate := now.Format("2006-01-01")
	endDate := now.AddDate(0, 1, 0).Format("2006-01-02")

	usageReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/dashboard/billing/usage?start_date="+startDate+"&end_date="+endDate, nil)
	if err != nil {
		return nil, err
	}
	usageReq.Header.Set("Authorization", "Bearer "+auth)
	usageReq.Header.Set("User-Agent", UserAgent)

	usageResp, err := client.Do(usageReq)
	if err != nil {
		return nil, err
	}
	defer usageResp.Body.Close()

	var usage struct {
		TotalUsage float64 `json:"total_usage"`
	}
	body2, _ := io.ReadAll(usageResp.Body)
	if json.Unmarshal(body2, &usage) != nil {
		return nil, nil
	}

	return &BalanceInfo{
		Remaining: sub.HardLimitUSD - usage.TotalUsage,
		Used:      usage.TotalUsage,
	}, nil
}
