package photos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultPexelsBaseURL = "https://api.pexels.com/v1"
	maxPexelsJSONBytes   = 1 << 20
	maxPexelsImageBytes  = 8 << 20
	pexelsImageHost      = "images.pexels.com"
	maxPexelsCandidates  = 5
	maxPexelsDownloads   = 3
)

type PexelsConfig struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	HTTPClient *http.Client
}

type Pexels struct {
	apiKey   string
	endpoint string
	client   *http.Client
	timeout  time.Duration
}

func NewPexels(config PexelsConfig) (*Pexels, error) {
	apiKey := strings.TrimSpace(config.APIKey)
	if apiKey == "" || strings.ContainsAny(apiKey, "\r\n") {
		return nil, fmt.Errorf("%w: Pexels API key is required", ErrConfiguration)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultPexelsBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !safePhotoAPIScheme(parsed) {
		return nil, fmt.Errorf("%w: invalid Pexels API base URL", ErrConfiguration)
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.Timeout < time.Second {
		return nil, fmt.Errorf("%w: Pexels timeout must be at least one second", ErrConfiguration)
	}
	client := &http.Client{Timeout: config.Timeout}
	if config.HTTPClient != nil {
		copyClient := *config.HTTPClient
		client = &copyClient
		if client.Timeout == 0 {
			client.Timeout = config.Timeout
		}
	}
	// A caller-supplied client may customize Transport for tests or observability,
	// but redirects remain forbidden because the allowlist applies to the exact
	// URL returned by Pexels.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Pexels{apiKey: apiKey, endpoint: baseURL + "/search", client: client, timeout: config.Timeout}, nil
}

func safePhotoAPIScheme(parsed *url.URL) bool {
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (provider *Pexels) Find(ctx context.Context, query string) (Asset, error) {
	return provider.find(ctx, query, nil)
}

func (provider *Pexels) FindAlternative(ctx context.Context, query string, excludedAssetIDs ...string) (Asset, error) {
	excluded := make(map[string]struct{}, len(excludedAssetIDs))
	for _, assetID := range excludedAssetIDs {
		assetID = strings.TrimSpace(assetID)
		if assetID != "" {
			excluded[assetID] = struct{}{}
		}
	}
	return provider.find(ctx, query, excluded)
}

func (provider *Pexels) find(ctx context.Context, query string, excluded map[string]struct{}) (Asset, error) {
	query = strings.TrimSpace(query)
	if !validPhotoQuery(query) {
		return Asset{}, fmt.Errorf("%w: invalid Pexels search query", ErrInvalidResult)
	}
	operationCtx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	endpoint, err := url.Parse(provider.endpoint)
	if err != nil {
		return Asset{}, fmt.Errorf("%w: invalid Pexels endpoint", ErrConfiguration)
	}
	values := endpoint.Query()
	values.Set("query", query)
	values.Set("orientation", "portrait")
	values.Set("size", "medium")
	values.Set("locale", "en-US")
	values.Set("per_page", "5")
	values.Set("page", "1")
	endpoint.RawQuery = values.Encode()

	request, err := http.NewRequestWithContext(operationCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Asset{}, fmt.Errorf("%w: build Pexels request", ErrConfiguration)
	}
	request.Header.Set("Authorization", provider.apiKey)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "witty-reply/1")
	response, err := provider.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return Asset{}, ctx.Err()
		}
		if operationCtx.Err() != nil {
			return Asset{}, fmt.Errorf("%w: Pexels search transport: %w", ErrUnavailable, operationCtx.Err())
		}
		return Asset{}, fmt.Errorf("%w: Pexels search transport", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return Asset{}, fmt.Errorf("%w: Pexels search status %d", ErrAuthentication, response.StatusCode)
		case http.StatusTooManyRequests:
			return Asset{}, fmt.Errorf("%w: Pexels search status %d", ErrRateLimited, response.StatusCode)
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return Asset{}, fmt.Errorf("%w: Pexels search status %d", ErrUnavailable, response.StatusCode)
		default:
			return Asset{}, fmt.Errorf("%w: Pexels search status %d", ErrInvalidResult, response.StatusCode)
		}
	}
	raw, err := readBounded(response.Body, maxPexelsJSONBytes)
	if err != nil {
		return Asset{}, err
	}
	var payload struct {
		Photos []struct {
			ID              int64  `json:"id"`
			URL             string `json:"url"`
			Photographer    string `json:"photographer"`
			PhotographerURL string `json:"photographer_url"`
			Alt             string `json:"alt"`
			Src             struct {
				Large2X string `json:"large2x"`
				Large   string `json:"large"`
				Medium  string `json:"medium"`
			} `json:"src"`
		} `json:"photos"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Asset{}, fmt.Errorf("%w: decode Pexels search", ErrInvalidResult)
	}
	if len(payload.Photos) == 0 {
		return Asset{}, ErrNotFound
	}

	photos := payload.Photos
	if len(photos) > maxPexelsCandidates {
		photos = photos[:maxPexelsCandidates]
	}
	downloads := 0
	seenImageURLs := make(map[string]struct{})
	searching := true
	var retryableDownloadErr error
	for _, photo := range photos {
		if !searching {
			break
		}
		if _, skip := excluded[strconv.FormatInt(photo.ID, 10)]; skip {
			continue
		}
		if altSuggestsRecognizablePeople(photo.Alt) {
			continue
		}
		asset, ok := validatedPexelsAsset(photo.ID, photo.URL, photo.Photographer, photo.PhotographerURL, photo.Alt, query)
		if !ok {
			continue
		}
		for _, imageURL := range []string{photo.Src.Large2X, photo.Src.Large, photo.Src.Medium} {
			imageURL = strings.TrimSpace(imageURL)
			if imageURL == "" {
				continue
			}
			if _, duplicate := seenImageURLs[imageURL]; duplicate {
				continue
			}
			seenImageURLs[imageURL] = struct{}{}
			if downloads >= maxPexelsDownloads {
				searching = false
				break
			}
			downloads++
			data, downloadErr := provider.download(operationCtx, imageURL)
			if downloadErr != nil {
				if err := operationCtx.Err(); err != nil {
					return Asset{}, fmt.Errorf("%w: Pexels image download: %w", ErrUnavailable, err)
				}
				if errors.Is(downloadErr, ErrRateLimited) || errors.Is(downloadErr, ErrUnavailable) {
					retryableDownloadErr = downloadErr
				}
				continue
			}
			asset.Data = data
			return asset, nil
		}
	}
	if retryableDownloadErr != nil {
		return Asset{}, retryableDownloadErr
	}
	return Asset{}, ErrNotFound
}

func validatedPexelsAsset(id int64, pageURL, author, authorURL, alt, query string) (Asset, bool) {
	author = strings.TrimSpace(author)
	if id <= 0 || !safeAttributionText(author, 160) {
		return Asset{}, false
	}
	if !safeHTTPSURL(pageURL, "www.pexels.com") || !safeHTTPSURL(authorURL, "www.pexels.com") {
		return Asset{}, false
	}
	alt = strings.TrimSpace(alt)
	if utf8.RuneCountInString(alt) > 300 {
		alt = string([]rune(alt)[:300])
	}
	return Asset{
		Provider: "pexels", AssetID: strconv.FormatInt(id, 10), PageURL: pageURL,
		Author: author, AuthorURL: authorURL, Query: query, Alt: alt,
	}, true
}

func safeAttributionText(value string, maxRunes int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return false
		}
	}
	return true
}

func (provider *Pexels) download(ctx context.Context, rawURL string) ([]byte, error) {
	if !safeHTTPSURL(rawURL, pexelsImageHost) {
		return nil, fmt.Errorf("%w: untrusted Pexels image URL", ErrInvalidResult)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build Pexels image request", ErrConfiguration)
	}
	request.Header.Set("Accept", "image/*")
	request.Header.Set("User-Agent", "witty-reply/1")
	response, err := provider.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: Pexels image transport: %w", ErrUnavailable, ctx.Err())
		}
		return nil, fmt.Errorf("%w: Pexels image transport", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch response.StatusCode {
		case http.StatusTooManyRequests:
			return nil, fmt.Errorf("%w: Pexels image status %d", ErrRateLimited, response.StatusCode)
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return nil, fmt.Errorf("%w: Pexels image status %d", ErrUnavailable, response.StatusCode)
		default:
			return nil, fmt.Errorf("%w: Pexels image status %d", ErrInvalidResult, response.StatusCode)
		}
	}
	if response.ContentLength > maxPexelsImageBytes {
		return nil, ErrTooLarge
	}
	return readBounded(response.Body, maxPexelsImageBytes)
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, ErrTooLarge
	}
	return raw, nil
}

func validPhotoQuery(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) < 3 || utf8.RuneCountInString(value) > 100 {
		return false
	}
	letters := 0
	for _, character := range value {
		if unicode.IsLetter(character) {
			letters++
			if character > unicode.MaxASCII {
				return false
			}
			continue
		}
		if character != ' ' && character != '-' {
			return false
		}
	}
	return letters >= 3
}

func safeHTTPSURL(rawURL, expectedHost string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), expectedHost) || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	return parsed.Port() == "" || parsed.Port() == "443"
}

func altSuggestsRecognizablePeople(value string) bool {
	lower := strings.ToLower(value)
	for _, word := range []string{"person", "people", "woman", "women", "man ", "men ", "girl", "boy", "child", "singer", "musician", "portrait", "face"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}
