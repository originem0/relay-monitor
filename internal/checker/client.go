package checker

import (
	"crypto/tls"
	"net/http"
	"time"
)

const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// NewClient creates an *http.Client optimized for proxy use.
// The client-level timeout is disabled (0) because proxy uses per-request
// context timeouts. A global timeout would kill long-running streaming connections.
func NewClient(sslVerify bool) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !sslVerify,
		},
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Timeout:   0, // per-request context handles timeouts
		Transport: transport,
	}
}
