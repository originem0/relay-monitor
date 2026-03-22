package checker

import (
	"crypto/tls"
	"net/http"
	"time"
)

const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// NewClient creates an *http.Client with a 60-second timeout.
// When sslVerify is false the TLS certificate chain is not validated,
// matching the Python default for relay proxies whose certs may be irregular.
func NewClient(sslVerify bool) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !sslVerify,
		},
	}
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
	}
}
