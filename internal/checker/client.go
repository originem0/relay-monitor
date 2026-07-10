package checker

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"
)

const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// maxUpstreamBodyBytes caps how much of an untrusted relay response the checker
// buffers. Health/capability/balance replies are small; a hostile or broken
// station could otherwise stream an unbounded body into io.ReadAll and OOM the
// monitor. Mirrors the proxy's own response cap.
const maxUpstreamBodyBytes = 8 * 1024 * 1024

// readLimited reads up to maxUpstreamBodyBytes+1 from r and reports whether the
// body exceeded the cap. Callers treat an over-limit body as a failure rather
// than trusting a truncated parse.
func readLimited(r io.Reader) (body []byte, tooLarge bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, maxUpstreamBodyBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > maxUpstreamBodyBytes {
		return body[:maxUpstreamBodyBytes], true, nil
	}
	return body, false, nil
}

// errBodyTooLarge is returned when an upstream response exceeds the read cap.
var errBodyTooLarge = fmt.Errorf("upstream response body exceeds %d bytes", maxUpstreamBodyBytes)

// NewClient creates an *http.Client optimized for proxy use.
// The client-level timeout is disabled (0) because proxy uses per-request
// context timeouts. A global timeout would kill long-running streaming connections.
func NewClient(sslVerify bool) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !sslVerify,
		},
		MaxIdleConns: 200,
		// Relay traffic concentrates on a few hosts (the checker fans out 16-wide
		// and the proxy routes to a small provider set), so a per-host idle cap of
		// 10 forced constant TLS re-handshakes. Raise it to keep hot connections warm.
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		// Bound the connection-setup phases. Without these the only ceiling is the
		// per-request context, so a black-holed TCP connect or a TLS handshake to a
		// wedged host ties up a checker slot for the full request timeout.
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   0, // per-request context handles timeouts
		Transport: transport,
	}
}
