package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ErrorKind classifies provider failures in a way that routing, retries, and
// doctor checks can reason about without depending on brittle string matching.
type ErrorKind string

const (
	ErrorKindUnknown     ErrorKind = "unknown"
	ErrorKindTimeout     ErrorKind = "timeout"
	ErrorKindCanceled    ErrorKind = "canceled"
	ErrorKindRateLimit   ErrorKind = "rate_limit"
	ErrorKindAuth        ErrorKind = "auth"
	ErrorKindConfig      ErrorKind = "config"
	ErrorKindNetwork     ErrorKind = "network"
	ErrorKindProvider    ErrorKind = "provider"
	ErrorKindUnavailable ErrorKind = "unavailable"
)

// ProviderError carries structured provider failure metadata.
type ProviderError struct {
	Provider   string
	Op         string
	Kind       ErrorKind
	Retryable  bool
	StatusCode int
	Message    string
	Err        error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}

	prefix := strings.TrimSpace(e.Provider)
	if prefix == "" {
		prefix = "provider"
	}
	if op := strings.TrimSpace(e.Op); op != "" {
		prefix += " " + op
	}

	msg := strings.TrimSpace(e.Message)
	if msg == "" && e.Err != nil {
		msg = strings.TrimSpace(e.Err.Error())
	}
	if msg == "" && e.StatusCode > 0 {
		msg = http.StatusText(e.StatusCode)
	}
	if msg == "" {
		msg = string(e.Kind)
	}

	if e.StatusCode > 0 {
		return fmt.Sprintf("%s error (%d): %s", prefix, e.StatusCode, msg)
	}
	return fmt.Sprintf("%s error: %s", prefix, msg)
}

func (e *ProviderError) Unwrap() error { return e.Err }

func ErrorKindOf(err error) ErrorKind {
	if err == nil {
		return ErrorKindUnknown
	}

	var perr *ProviderError
	if errors.As(err, &perr) && perr != nil {
		if perr.Kind != "" {
			return perr.Kind
		}
		return ErrorKindUnknown
	}

	switch {
	case errors.Is(err, context.Canceled):
		return ErrorKindCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorKindTimeout
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorKindTimeout
		}
		return ErrorKindNetwork
	}

	return ErrorKindUnknown
}

func IsRetryableProviderError(err error) bool {
	if err == nil {
		return false
	}
	var perr *ProviderError
	if errors.As(err, &perr) && perr != nil {
		return perr.Retryable
	}
	kind := ErrorKindOf(err)
	return kind == ErrorKindTimeout || kind == ErrorKindNetwork
}

func newProviderError(provider, op string, kind ErrorKind, retryable bool, statusCode int, msg string, err error) *ProviderError {
	return &ProviderError{
		Provider:   strings.TrimSpace(provider),
		Op:         strings.TrimSpace(op),
		Kind:       kind,
		Retryable:  retryable,
		StatusCode: statusCode,
		Message:    compactProviderMessage(msg),
		Err:        err,
	}
}

func wrapTransportError(provider, op string, err error) error {
	if err == nil {
		return nil
	}
	var perr *ProviderError
	if errors.As(err, &perr) {
		return err
	}

	switch {
	case errors.Is(err, context.Canceled):
		return newProviderError(provider, op, ErrorKindCanceled, false, 0, "canceled", err)
	case errors.Is(err, context.DeadlineExceeded):
		return newProviderError(provider, op, ErrorKindTimeout, true, 0, "deadline exceeded", err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return newProviderError(provider, op, ErrorKindTimeout, true, 0, netErr.Error(), err)
		}
		return newProviderError(provider, op, ErrorKindNetwork, true, 0, netErr.Error(), err)
	}

	lower := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(lower, "timeout"),
		strings.Contains(lower, "timed out"),
		strings.Contains(lower, "deadline exceeded"),
		strings.Contains(lower, "tls handshake timeout"):
		return newProviderError(provider, op, ErrorKindTimeout, true, 0, err.Error(), err)
	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "network is unreachable"),
		strings.Contains(lower, "no such host"),
		strings.Contains(lower, "temporary failure in name resolution"),
		strings.Contains(lower, "proxyconnect"),
		strings.Contains(lower, "transport channel closed"):
		return newProviderError(provider, op, ErrorKindNetwork, true, 0, err.Error(), err)
	default:
		return newProviderError(provider, op, ErrorKindNetwork, true, 0, err.Error(), err)
	}
}

func wrapResponseReadError(provider, op string, err error) error {
	if err == nil {
		return nil
	}
	if kind := ErrorKindOf(err); kind == ErrorKindTimeout || kind == ErrorKindNetwork || kind == ErrorKindCanceled {
		return wrapTransportError(provider, op, err)
	}
	return newProviderError(provider, op, ErrorKindProvider, false, 0, err.Error(), err)
}

func newProviderStatusError(provider, op string, statusCode int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(statusCode)
	}

	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return newProviderError(provider, op, ErrorKindAuth, false, statusCode, msg, nil)
	case statusCode == http.StatusTooManyRequests:
		return newProviderError(provider, op, ErrorKindRateLimit, true, statusCode, msg, nil)
	case statusCode == http.StatusRequestTimeout:
		return newProviderError(provider, op, ErrorKindTimeout, true, statusCode, msg, nil)
	case statusCode >= 500:
		return newProviderError(provider, op, ErrorKindUnavailable, true, statusCode, msg, nil)
	case statusCode == http.StatusBadRequest || statusCode == http.StatusNotFound || statusCode == http.StatusUnprocessableEntity:
		return newProviderError(provider, op, ErrorKindConfig, false, statusCode, msg, nil)
	default:
		return newProviderError(provider, op, ErrorKindProvider, false, statusCode, msg, nil)
	}
}

func compactProviderMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	msg = strings.Join(strings.Fields(msg), " ")
	const maxLen = 512
	if len(msg) > maxLen {
		return msg[:maxLen-3] + "..."
	}
	return msg
}
