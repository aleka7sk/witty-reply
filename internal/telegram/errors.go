package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrNetwork = errors.New("telegram: network request failed")

// APIError is a rejected Bot API request. Description is sanitized and
// bounded before it is stored, so formatting the error cannot expose the bot
// token even if an upstream or test server reflects the request URL.
type APIError struct {
	Method          string
	Code            int
	Description     string
	RetryAfter      time.Duration
	MigrateToChatID int64
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := fmt.Sprintf("telegram %s failed: api code %d", e.Method, e.Code)
	if e.Description != "" {
		message += ": " + e.Description
	}
	if e.RetryAfter > 0 {
		message += fmt.Sprintf(" (retry after %s)", e.RetryAfter)
	}
	return message
}

func (e *APIError) Retryable() bool {
	return e != nil && (e.Code == http.StatusTooManyRequests || e.Code >= 500)
}

// TransportError wraps only normalized sentinel errors. In particular, it
// never unwraps net/http's url.Error because that value contains the tokenized
// Bot API endpoint.
type TransportError struct {
	Method string
	Cause  error
}

func (e *TransportError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause == nil {
		return fmt.Sprintf("telegram %s failed: transport error", e.Method)
	}
	return fmt.Sprintf("telegram %s failed: %v", e.Method, e.Cause)
}

func (e *TransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type HTTPError struct {
	Method     string
	StatusCode int
}

type RequestError struct {
	Method string
	Reason string
}

func (e *RequestError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Reason == "" {
		return fmt.Sprintf("telegram %s failed: could not encode request", e.Method)
	}
	return fmt.Sprintf("telegram %s failed: %s", e.Method, e.Reason)
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("telegram %s failed: unexpected HTTP status %d", e.Method, e.StatusCode)
}

type DecodeError struct {
	Method string
	Reason string
}

func (e *DecodeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Reason == "" {
		return fmt.Sprintf("telegram %s failed: invalid API response", e.Method)
	}
	return fmt.Sprintf("telegram %s failed: invalid API response: %s", e.Method, e.Reason)
}

type ResponseTooLargeError struct {
	Method string
	Limit  int64
}

func (e *ResponseTooLargeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("telegram %s failed: response exceeds %d bytes", e.Method, e.Limit)
}

type FileTooLargeError struct {
	Size  int64
	Limit int64
}

func (e *FileTooLargeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Size > 0 {
		return fmt.Sprintf("telegram file is too large: %d bytes exceeds %d-byte limit", e.Size, e.Limit)
	}
	return fmt.Sprintf("telegram file is too large: exceeds %d-byte limit", e.Limit)
}

type WebhookErrorCode string

const (
	WebhookMethodNotAllowed WebhookErrorCode = "method_not_allowed"
	WebhookUnauthorized     WebhookErrorCode = "unauthorized"
	WebhookUnsupportedMedia WebhookErrorCode = "unsupported_media_type"
	WebhookBodyTooLarge     WebhookErrorCode = "body_too_large"
	WebhookInvalidJSON      WebhookErrorCode = "invalid_json"
)

// WebhookError contains only a stable classification and status code. It does
// not retain the secret header or request body.
type WebhookError struct {
	Code       WebhookErrorCode
	StatusCode int
}

func (e *WebhookError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("telegram webhook rejected: %s", e.Code)
}

func normalizeTransportError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return ErrNetwork
	}
}

func sanitizeUpstreamText(value, token string) string {
	const maxBytes = 1024
	if token != "" {
		pathToken := url.PathEscape(token)
		queryToken := url.QueryEscape(token)
		for _, candidate := range []string{
			token,
			pathToken,
			queryToken,
			strings.ReplaceAll(pathToken, "%3A", "%3a"),
			strings.ReplaceAll(queryToken, "%3A", "%3a"),
		} {
			value = strings.ReplaceAll(value, candidate, "[REDACTED]")
		}
	}
	value = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return ' '
		}
		return char
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > maxBytes {
		end := maxBytes
		for end > 0 && !utf8.ValidString(value[:end]) {
			end--
		}
		value = value[:end] + "..."
	}
	return value
}
