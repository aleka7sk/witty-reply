// Package photos retrieves optional licensed editorial illustrations without
// exposing post text, Telegram identities, or credentials to arbitrary hosts.
package photos

import (
	"context"
	"errors"
)

var (
	ErrConfiguration = errors.New("invalid photo provider configuration")
	ErrNotFound      = errors.New("no suitable licensed photo found")
	ErrInvalidResult = errors.New("invalid photo provider result")
	ErrTooLarge      = errors.New("photo provider response is too large")
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
