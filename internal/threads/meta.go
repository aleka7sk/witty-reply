package threads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultBaseURL         = "https://graph.threads.net/v1.0"
	defaultTimeout         = 20 * time.Second
	defaultMaxResponseSize = int64(64 << 10)
)

type Config struct {
	UserID           string
	AccessToken      string
	BaseURL          string
	Timeout          time.Duration
	HTTPClient       *http.Client
	MaxResponseBytes int64
}

type Meta struct {
	userID           string
	accessToken      string
	baseURL          string
	httpClient       *http.Client
	maxResponseBytes int64
}

func NewMeta(config Config) (*Meta, error) {
	userID := strings.TrimSpace(config.UserID)
	if !validResourceID(userID) {
		return nil, configError()
	}
	accessToken := strings.TrimSpace(config.AccessToken)
	if accessToken == "" || len(accessToken) > 8192 || strings.IndexFunc(accessToken, unicode.IsSpace) >= 0 {
		return nil, configError()
	}

	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	normalizedBaseURL, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, configError()
	}

	if config.Timeout < 0 || config.MaxResponseBytes < 0 {
		return nil, configError()
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseSize
	}

	httpClient := &http.Client{Timeout: defaultTimeout}
	if config.HTTPClient != nil {
		clientCopy := *config.HTTPClient
		httpClient = &clientCopy
		if httpClient.Timeout <= 0 {
			httpClient.Timeout = defaultTimeout
		}
	}
	if config.Timeout > 0 {
		httpClient.Timeout = config.Timeout
	}
	// Meta's publishing endpoints do not require redirects. Failing closed also
	// prevents the access_token query parameter used by status calls from being
	// forwarded or exposed through a Referer header.
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Meta{
		userID:           userID,
		accessToken:      accessToken,
		baseURL:          normalizedBaseURL,
		httpClient:       httpClient,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func NewMetaClient(config Config) (*Meta, error) {
	return NewMeta(config)
}

func (client *Meta) Enabled() bool {
	return client != nil
}

func (client *Meta) CreateText(ctx context.Context, text, replyToID string) (string, error) {
	const operation = "create_text"
	if client == nil {
		return "", configOperationError(operation)
	}
	if err := validateText(text, operation); err != nil {
		return "", err
	}
	if err := validateOptionalID(replyToID, operation); err != nil {
		return "", err
	}

	form := url.Values{
		"access_token": {client.accessToken},
		"media_type":   {"TEXT"},
		"text":         {text},
	}
	if replyToID = strings.TrimSpace(replyToID); replyToID != "" {
		form.Set("reply_to_id", replyToID)
	}
	// A failed create may leave an unpublished orphan container, but it cannot
	// create public content. The caller can safely create a replacement.
	body, err := client.doForm(ctx, operation, client.userEndpoint("threads"), form, false)
	if err != nil {
		return "", err
	}
	id, err := decodeID(body)
	if err != nil {
		return "", &Error{Operation: operation, Code: CodeInvalidResponse, Class: Definite}
	}
	return id, nil
}

func (client *Meta) ContainerStatus(ctx context.Context, id string) (Status, error) {
	const operation = "container_status"
	if client == nil {
		return Status{}, configOperationError(operation)
	}
	if err := validateRequiredID(id, operation); err != nil {
		return Status{}, err
	}
	id = strings.TrimSpace(id)
	query := url.Values{
		"access_token": {client.accessToken},
		"fields":       {"id,status,error_message"},
	}
	endpoint := client.resourceEndpoint(id) + "?" + query.Encode()
	body, err := client.do(ctx, operation, http.MethodGet, endpoint, "", false)
	if err != nil {
		return Status{}, err
	}

	var response struct {
		ID           string `json:"id"`
		State        string `json:"status"`
		ErrorMessage string `json:"error_message"`
	}
	if err := json.Unmarshal(body, &response); err != nil || !validResourceID(response.ID) {
		return Status{}, &Error{Operation: operation, Code: CodeInvalidResponse, Class: Definite}
	}
	state := ContainerState(response.State)
	switch state {
	case StateExpired, StateError, StateFinished, StateInProgress, StatePublished:
	default:
		return Status{}, &Error{Operation: operation, Code: CodeInvalidResponse, Class: Definite}
	}
	return Status{
		ID:           response.ID,
		State:        state,
		ErrorMessage: safeContainerError(response.ErrorMessage),
	}, nil
}

func (client *Meta) Publish(ctx context.Context, containerID string) (Publication, error) {
	const operation = "publish"
	if client == nil {
		return Publication{}, configOperationError(operation)
	}
	if err := validateRequiredID(containerID, operation); err != nil {
		return Publication{}, err
	}
	form := url.Values{
		"access_token": {client.accessToken},
		"creation_id":  {strings.TrimSpace(containerID)},
	}
	body, err := client.doForm(ctx, operation, client.userEndpoint("threads_publish"), form, true)
	if err != nil {
		return Publication{}, err
	}
	id, err := decodeID(body)
	if err != nil {
		// The HTTP request succeeded but the media ID could not be observed. The
		// container status must be checked before retrying publication.
		return Publication{}, &Error{Operation: operation, Code: CodeInvalidResponse, Class: Ambiguous}
	}
	return Publication{ID: id}, nil
}

func (client *Meta) doForm(
	ctx context.Context,
	operation string,
	endpoint string,
	form url.Values,
	mutation bool,
) ([]byte, error) {
	return client.do(ctx, operation, http.MethodPost, endpoint, form.Encode(), mutation)
}

func (client *Meta) do(
	ctx context.Context,
	operation string,
	method string,
	endpoint string,
	encodedForm string,
	mutation bool,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, &Error{Operation: operation, Code: CodeTransport, Class: Definite, cause: normalizedContextError(err)}
	}
	var body io.Reader
	if encodedForm != "" {
		body = strings.NewReader(encodedForm)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, configOperationError(operation)
	}
	request.Header.Set("Accept", "application/json")
	if encodedForm != "" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		class := Definite
		if mutation {
			class = Ambiguous
		}
		return nil, &Error{
			Operation: operation,
			Code:      CodeTransport,
			Class:     class,
			cause:     normalizedTransportError(ctx, err),
		}
	}
	defer response.Body.Close()

	responseBody, readErr := readBounded(response.Body, client.maxResponseBytes)
	if readErr != nil {
		class := responseFailureClass(mutation, response.StatusCode)
		code := CodeInvalidResponse
		if errors.Is(readErr, errResponseTooLarge) {
			code = CodeResponseTooLarge
		}
		return nil, &Error{
			Operation:  operation,
			Code:       code,
			Class:      class,
			HTTPStatus: response.StatusCode,
		}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, metaHTTPError(operation, response.StatusCode, responseBody, mutation)
	}
	return responseBody, nil
}

func (client *Meta) userEndpoint(resource string) string {
	return client.baseURL + "/" + url.PathEscape(client.userID) + "/" + resource
}

func (client *Meta) resourceEndpoint(id string) string {
	return client.baseURL + "/" + url.PathEscape(id)
}

func decodeID(body []byte) (string, error) {
	var response struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", err
	}
	response.ID = strings.TrimSpace(response.ID)
	if !validResourceID(response.ID) {
		return "", errors.New("invalid id")
	}
	return response.ID, nil
}

var errResponseTooLarge = errors.New("threads response too large")

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errResponseTooLarge
	}
	return body, nil
}

func metaHTTPError(operation string, statusCode int, body []byte, mutation bool) error {
	var response struct {
		Error struct {
			Code    int    `json:"code"`
			Subcode int    `json:"error_subcode"`
			TraceID string `json:"fbtrace_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &response)

	code := CodeUpstream
	switch {
	case statusCode >= 300 && statusCode < 400:
		code = CodeRedirect
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden,
		response.Error.Code == 102, response.Error.Code == 190, response.Error.Code == 467:
		code = CodeAuth
	case response.Error.Code == 3, response.Error.Code == 10,
		response.Error.Code >= 200 && response.Error.Code <= 299:
		code = CodePermission
	case statusCode == http.StatusTooManyRequests, response.Error.Code == 4,
		response.Error.Code == 17, response.Error.Code == 613:
		code = CodeRateLimited
	case statusCode == http.StatusBadRequest && response.Error.Code == 100:
		code = CodeInvalidInput
	}

	class := Definite
	if mutation && statusCode >= 500 {
		class = Ambiguous
	}
	return &Error{
		Operation:    operation,
		Code:         code,
		Class:        class,
		HTTPStatus:   statusCode,
		GraphCode:    response.Error.Code,
		GraphSubcode: response.Error.Subcode,
		// Do not retain upstream strings. Even fields normally considered safe,
		// such as fbtrace_id, can be reflected by a hostile or broken endpoint.
		TraceID: "",
	}
}

func responseFailureClass(mutation bool, statusCode int) FailureClass {
	if mutation && (statusCode >= 500 || (statusCode >= 200 && statusCode < 300)) {
		return Ambiguous
	}
	return Definite
}

func normalizedTransportError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrTransport
}

func normalizedContextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrTransport
}

func configError() error {
	return configOperationError("configure")
}

func configOperationError(operation string) error {
	return &Error{Operation: operation, Code: CodeConfig, Class: Definite}
}

func normalizeBaseURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errors.New("invalid base URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid base URL")
	}
	if parsed.Scheme != "https" && !isLoopback(parsed.Hostname()) {
		return "", errors.New("invalid base URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validResourceID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 256 {
		return false
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == ':' {
			continue
		}
		return false
	}
	return true
}

func safeContainerError(message string) string {
	switch strings.TrimSpace(message) {
	case "FAILED_DOWNLOADING_VIDEO",
		"FAILED_PROCESSING_AUDIO",
		"FAILED_PROCESSING_VIDEO",
		"INVALID_ASPEC_RATIO",
		"INVALID_ASPECT_RATIO",
		"INVALID_BIT_RATE",
		"INVALID_DURATION",
		"INVALID_FRAME_RATE",
		"INVALID_AUDIO_CHANNELS",
		"INVALID_AUDIO_CHANNEL_LAYOUT",
		"UNKNOWN":
		return strings.TrimSpace(message)
	default:
		return ""
	}
}

var _ Publisher = (*Meta)(nil)
