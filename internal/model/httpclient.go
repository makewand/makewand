package model

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// noRedirectToForeignHost returns a CheckRedirect policy that blocks
// cross-host redirects (prevents credential leaks via malicious redirects)
// while allowing same-host redirects up to a reasonable limit.
func noRedirectToForeignHost() func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if len(via) >= 1 && req.URL.Host != via[0].URL.Host {
			return http.ErrUseLastResponse
		}
		return nil
	}
}

// newAPIClient returns an *http.Client suitable for non-streaming API calls.
func newAPIClient() *http.Client {
	return &http.Client{
		Timeout:       5 * time.Minute,
		CheckRedirect: noRedirectToForeignHost(),
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
	}
}

// newStreamClient returns an *http.Client for streaming requests.
// It has no overall timeout (uses context cancellation instead),
// but sets transport-level timeouts.
func newStreamClient() *http.Client {
	return &http.Client{
		CheckRedirect: noRedirectToForeignHost(),
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
	}
}

// newHealthCheckClient returns a short-timeout client for health checks.
func newHealthCheckClient() *http.Client {
	return &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: noRedirectToForeignHost(),
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			TLSHandshakeTimeout: 2 * time.Second,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// limitedReadAll reads up to maxBytes from r. Returns an error if the limit is exceeded.
func limitedReadAll(r io.Reader, maxBytes int64) ([]byte, error) {
	lr := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes limit", maxBytes)
	}
	return data, nil
}
