// Package photos retrieves optional licensed editorial illustrations without
// exposing post text, Telegram identities, or credentials to arbitrary hosts.
package photos

import (
	"context"
	"errors"
)

var (
	ErrConfiguration  = errors.New("invalid photo provider configuration")
	ErrAuthentication = errors.New("photo provider authentication failed")
	ErrRateLimited    = errors.New("photo provider rate limit reached")
	ErrUnavailable    = errors.New("photo provider is temporarily unavailable")
	ErrNotFound       = errors.New("no suitable licensed photo found")
	ErrInvalidResult  = errors.New("invalid photo provider result")
	ErrTooLarge       = errors.New("photo provider response is too large")
)

type Asset struct {
	Provider  string
	AssetID   string
	PageURL   string
	Author    string
	AuthorURL string
	Query     string
	Alt       string
	Data      []byte
}

type Source interface {
	Find(context.Context, string) (Asset, error)
}

// AlternativeSource can exclude assets already shown to the operator. The
// exclusion remains local: only the anonymous query is sent to the provider.
type AlternativeSource interface {
	FindAlternative(context.Context, string, ...string) (Asset, error)
}
