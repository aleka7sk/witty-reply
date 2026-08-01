// Package telegram provides a small, direct client for the Telegram Bot API
// and a verified webhook decoder. It deliberately keeps the bot token private
// and never includes tokenized endpoint URLs in returned errors.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultAPIBaseURL       = "https://api.telegram.org"
	defaultMaxResponseBytes = int64(8 << 20)
	defaultRateLimitRetries = 2
	maxAutomaticRetryDelay  = 30 * time.Second
)

type Option func(*Client) error

type Client struct {
	token               string
	baseURL             string
	httpClient          *http.Client
	maxResponseBytes    int64
	maxDownloadBytes    int64
	maxPhotoUploadBytes int64
	maxRateLimitRetries int
	sleep               func(context.Context, time.Duration) error
}

func New(token string, options ...Option) (*Client, error) {
	token = strings.TrimSpace(token)
	if err := validateToken(token); err != nil {
		return nil, err
	}

	client := &Client{
		token:               token,
		baseURL:             defaultAPIBaseURL,
		httpClient:          &http.Client{},
		maxResponseBytes:    defaultMaxResponseBytes,
		maxDownloadBytes:    MaxCloudDownloadBytes,
		maxPhotoUploadBytes: MaxCloudPhotoUploadBytes,
		maxRateLimitRetries: defaultRateLimitRetries,
		sleep:               sleepContext,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(client); err != nil {
			return nil, err
		}
	}

	baseURL, err := normalizeBaseURL(client.baseURL)
	if err != nil {
		return nil, err
	}
	client.baseURL = baseURL
	if client.httpClient == nil {
		return nil, validation("http_client", "must not be nil")
	}
	// A redirect to another host can disclose the tokenized source URL through
	// Referer. Telegram's API endpoints do not require redirects, so fail closed.
	httpClient := *client.httpClient
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.httpClient = &httpClient
	return client, nil
}

// NewClient is an explicit alias for New.
func NewClient(token string, options ...Option) (*Client, error) {
	return New(token, options...)
}

func WithBaseURL(baseURL string) Option {
	return func(client *Client) error {
		client.baseURL = baseURL
		return nil
	}
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(client *Client) error {
		if httpClient == nil {
			return validation("http_client", "must not be nil")
		}
		client.httpClient = httpClient
		return nil
	}
}

func WithMaxResponseBytes(limit int64) Option {
	return func(client *Client) error {
		if limit < 1 {
			return validation("max_response_bytes", "must be positive")
		}
		client.maxResponseBytes = limit
		return nil
	}
}

func WithMaxDownloadBytes(limit int64) Option {
	return func(client *Client) error {
		if limit < 1 || limit > MaxCloudDownloadBytes {
			return validation("max_download_bytes", fmt.Sprintf("must be between 1 and %d", MaxCloudDownloadBytes))
		}
		client.maxDownloadBytes = limit
		return nil
	}
}

func WithMaxPhotoUploadBytes(limit int64) Option {
	return func(client *Client) error {
		if limit < 1 || limit > MaxCloudPhotoUploadBytes {
			return validation("max_photo_upload_bytes", fmt.Sprintf("must be between 1 and %d", MaxCloudPhotoUploadBytes))
		}
		client.maxPhotoUploadBytes = limit
		return nil
	}
}

func WithMaxRateLimitRetries(retries int) Option {
	return func(client *Client) error {
		if retries < 0 || retries > 10 {
			return validation("max_rate_limit_retries", "must be between 0 and 10")
		}
		client.maxRateLimitRetries = retries
		return nil
	}
}

func (c *Client) GetUpdates(ctx context.Context, params GetUpdatesParams) ([]Update, error) {
	if len(params.AllowedUpdates) == 0 {
		params.AllowedUpdates = ProductAllowedUpdates()
	}
	if err := params.validate(); err != nil {
		return nil, err
	}
	var updates []Update
	if err := c.callJSON(ctx, "getUpdates", params, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (c *Client) SetWebhook(ctx context.Context, params SetWebhookParams) error {
	if len(params.AllowedUpdates) == 0 {
		params.AllowedUpdates = ProductAllowedUpdates()
	}
	if err := params.validate(); err != nil {
		return err
	}
	return c.callTrue(ctx, "setWebhook", params)
}

func (c *Client) DeleteWebhook(ctx context.Context, params DeleteWebhookParams) error {
	return c.callTrue(ctx, "deleteWebhook", params)
}

func (c *Client) GetWebhookInfo(ctx context.Context) (WebhookInfo, error) {
	var info WebhookInfo
	if err := c.callJSON(ctx, "getWebhookInfo", struct{}{}, &info); err != nil {
		return WebhookInfo{}, err
	}
	return info, nil
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var user User
	if err := c.callJSON(ctx, "getMe", struct{}{}, &user); err != nil {
		return User{}, err
	}
	return user, nil
}

func (c *Client) SendMessage(ctx context.Context, params SendMessageParams) (Message, error) {
	if err := params.validate(); err != nil {
		return Message{}, err
	}
	var message Message
	if err := c.callJSON(ctx, "sendMessage", params, &message); err != nil {
		return Message{}, err
	}
	return message, nil
}

func (c *Client) SendPhoto(ctx context.Context, params SendPhotoParams) (Message, error) {
	if err := params.validate(c.maxPhotoUploadBytes); err != nil {
		return Message{}, err
	}

	var message Message
	if params.Photo.Reference != "" {
		request := sendPhotoJSON{
			ChatID:              params.ChatID,
			Photo:               params.Photo.Reference,
			Caption:             params.Caption,
			ParseMode:           params.ParseMode,
			DisableNotification: params.DisableNotification,
			ProtectContent:      params.ProtectContent,
			ReplyMarkup:         params.ReplyMarkup,
		}
		if err := c.callJSON(ctx, "sendPhoto", request, &message); err != nil {
			return Message{}, err
		}
		return message, nil
	}

	body, contentType, err := encodePhotoMultipart(params)
	if err != nil {
		return Message{}, err
	}
	if err := c.call(ctx, "sendPhoto", contentType, body, &message); err != nil {
		return Message{}, err
	}
	return message, nil
}

func (c *Client) SendChatAction(ctx context.Context, params SendChatActionParams) error {
	if err := params.validate(); err != nil {
		return err
	}
	return c.callTrue(ctx, "sendChatAction", params)
}

func (c *Client) AnswerCallbackQuery(ctx context.Context, params AnswerCallbackQueryParams) error {
	if err := params.validate(); err != nil {
		return err
	}
	return c.callTrue(ctx, "answerCallbackQuery", params)
}

func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return File{}, validation("file_id", "is required")
	}
	var file File
	if err := c.callJSON(ctx, "getFile", struct {
		FileID string `json:"file_id"`
	}{FileID: fileID}, &file); err != nil {
		return File{}, err
	}
	return file, nil
}

// DownloadFile downloads a file using the client's configured upper bound.
func (c *Client) DownloadFile(ctx context.Context, fileID string) (DownloadedFile, error) {
	return c.DownloadFileLimit(ctx, fileID, c.maxDownloadBytes)
}

// DownloadFileLimit downloads no more than limit bytes. The client's own
// configured bound remains an absolute ceiling.
func (c *Client) DownloadFileLimit(ctx context.Context, fileID string, limit int64) (DownloadedFile, error) {
	if limit < 1 {
		return DownloadedFile{}, validation("download_limit", "must be positive")
	}
	if limit > c.maxDownloadBytes {
		limit = c.maxDownloadBytes
	}
	file, err := c.GetFile(ctx, fileID)
	if err != nil {
		return DownloadedFile{}, err
	}
	if file.FileSize > limit {
		return DownloadedFile{}, &FileTooLargeError{Size: file.FileSize, Limit: limit}
	}
	filePath, err := safeFilePath(file.FilePath)
	if err != nil {
		return DownloadedFile{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileURL(filePath), nil)
	if err != nil {
		return DownloadedFile{}, &TransportError{Method: "downloadFile", Cause: normalizeTransportError(err)}
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return DownloadedFile{}, &TransportError{Method: "downloadFile", Cause: normalizeTransportError(err)}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return DownloadedFile{}, &HTTPError{Method: "downloadFile", StatusCode: response.StatusCode}
	}
	if response.ContentLength > limit {
		return DownloadedFile{}, &FileTooLargeError{Size: response.ContentLength, Limit: limit}
	}
	data, exceeded, err := readBounded(response.Body, limit)
	if err != nil {
		return DownloadedFile{}, &TransportError{Method: "downloadFile", Cause: normalizeTransportError(err)}
	}
	if exceeded {
		return DownloadedFile{}, &FileTooLargeError{Limit: limit}
	}
	return DownloadedFile{
		File:        file,
		Data:        data,
		ContentType: response.Header.Get("Content-Type"),
	}, nil
}

type sendPhotoJSON struct {
	ChatID              int64                 `json:"chat_id"`
	Photo               string                `json:"photo"`
	Caption             string                `json:"caption,omitempty"`
	ParseMode           string                `json:"parse_mode,omitempty"`
	DisableNotification bool                  `json:"disable_notification,omitempty"`
	ProtectContent      bool                  `json:"protect_content,omitempty"`
	ReplyMarkup         *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type responseParameters struct {
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
	RetryAfter      int   `json:"retry_after,omitempty"`
}

type responseEnvelope struct {
	OK          *bool               `json:"ok"`
	Result      json.RawMessage     `json:"result"`
	Description string              `json:"description,omitempty"`
	ErrorCode   int                 `json:"error_code,omitempty"`
	Parameters  *responseParameters `json:"parameters,omitempty"`
}

func (c *Client) callTrue(ctx context.Context, method string, payload any) error {
	var result bool
	if err := c.callJSON(ctx, method, payload, &result); err != nil {
		return err
	}
	if !result {
		return &DecodeError{Method: method, Reason: "false success result"}
	}
	return nil
}

func (c *Client) callJSON(ctx context.Context, method string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return &RequestError{Method: method, Reason: "could not encode JSON"}
	}
	return c.call(ctx, method, "application/json", body, result)
}

func (c *Client) call(ctx context.Context, method, contentType string, body []byte, result any) error {
	for attempt := 0; ; attempt++ {
		err := c.callOnce(ctx, method, contentType, body, result)
		var apiError *APIError
		if !errors.As(err, &apiError) || apiError.Code != http.StatusTooManyRequests || attempt >= c.maxRateLimitRetries {
			return err
		}

		delay := apiError.RetryAfter
		if delay <= 0 {
			delay = time.Second
		}
		if delay > maxAutomaticRetryDelay {
			return err
		}
		if err := c.sleep(ctx, delay); err != nil {
			return &TransportError{Method: method, Cause: normalizeTransportError(err)}
		}
	}
}

func (c *Client) callOnce(ctx context.Context, method, contentType string, body []byte, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), bytes.NewReader(body))
	if err != nil {
		return &TransportError{Method: method, Cause: normalizeTransportError(err)}
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return &TransportError{Method: method, Cause: normalizeTransportError(err)}
	}
	defer response.Body.Close()

	raw, exceeded, err := readBounded(response.Body, c.maxResponseBytes)
	if err != nil {
		return &TransportError{Method: method, Cause: normalizeTransportError(err)}
	}
	if exceeded {
		return &ResponseTooLargeError{Method: method, Limit: c.maxResponseBytes}
	}

	envelope, decodeErr := decodeEnvelope(raw)
	if decodeErr != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return &HTTPError{Method: method, StatusCode: response.StatusCode}
		}
		return &DecodeError{Method: method, Reason: decodeErr.Error()}
	}
	if envelope.OK == nil {
		return &DecodeError{Method: method, Reason: "response is missing ok status"}
	}
	if !*envelope.OK {
		code := envelope.ErrorCode
		if code == 0 {
			code = response.StatusCode
		}
		apiError := &APIError{
			Method:      method,
			Code:        code,
			Description: sanitizeUpstreamText(envelope.Description, c.token),
		}
		if envelope.Parameters != nil {
			apiError.MigrateToChatID = envelope.Parameters.MigrateToChatID
			if envelope.Parameters.RetryAfter > 0 {
				if envelope.Parameters.RetryAfter > int(maxAutomaticRetryDelay/time.Second) {
					apiError.RetryAfter = maxAutomaticRetryDelay + time.Second
				} else {
					apiError.RetryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
				}
			}
		}
		if apiError.RetryAfter == 0 && code == http.StatusTooManyRequests {
			apiError.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		}
		return apiError
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{Method: method, StatusCode: response.StatusCode}
	}
	if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
		return &DecodeError{Method: method, Reason: "successful response is missing a result"}
	}
	if err := decodeResult(envelope.Result, result); err != nil {
		return &DecodeError{Method: method, Reason: err.Error()}
	}
	return nil
}

func decodeEnvelope(raw []byte) (responseEnvelope, error) {
	var envelope responseEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&envelope); err != nil {
		return responseEnvelope{}, errors.New("malformed JSON")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return responseEnvelope{}, err
	}
	return envelope, nil
}

func decodeResult(raw json.RawMessage, result any) error {
	if result == nil {
		return errors.New("missing result destination")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(result); err != nil {
		return errors.New("result does not match expected type")
	}
	return requireJSONEOF(decoder)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON value")
	}
	return errors.New("malformed trailing JSON")
}

func readBounded(reader io.Reader, limit int64) ([]byte, bool, error) {
	readLimit := limit
	if limit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(reader, readLimit))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

func encodePhotoMultipart(params SendPhotoParams) ([]byte, string, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	writeField := func(name, value string) error {
		if value == "" {
			return nil
		}
		return writer.WriteField(name, value)
	}

	if err := writeField("chat_id", strconv.FormatInt(params.ChatID, 10)); err != nil {
		return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode multipart fields"}
	}
	if err := writeField("caption", params.Caption); err != nil {
		return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode multipart fields"}
	}
	if err := writeField("parse_mode", params.ParseMode); err != nil {
		return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode multipart fields"}
	}
	if params.DisableNotification {
		if err := writeField("disable_notification", "true"); err != nil {
			return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode multipart fields"}
		}
	}
	if params.ProtectContent {
		if err := writeField("protect_content", "true"); err != nil {
			return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode multipart fields"}
		}
	}
	if params.ReplyMarkup != nil {
		replyMarkup, err := json.Marshal(params.ReplyMarkup)
		if err != nil {
			return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode reply markup"}
		}
		if err := writeField("reply_markup", string(replyMarkup)); err != nil {
			return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode multipart fields"}
		}
	}

	part, err := writer.CreateFormFile("photo", safeFilename(params.Photo.Filename))
	if err != nil {
		return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode file part"}
	}
	if _, err := part.Write(params.Photo.Data); err != nil {
		return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not encode file part"}
	}
	if err := writer.Close(); err != nil {
		return nil, "", &RequestError{Method: "sendPhoto", Reason: "could not finish multipart request"}
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

func safeFilename(filename string) string {
	filename = strings.ReplaceAll(filename, "\\", "/")
	filename = path.Base(filename)
	if filename == "." || filename == "/" || filename == "" {
		return "photo.jpg"
	}
	filename = strings.Map(func(char rune) rune {
		switch {
		case char >= 'a' && char <= 'z':
			return char
		case char >= 'A' && char <= 'Z':
			return char
		case char >= '0' && char <= '9':
			return char
		case char == '.', char == '_', char == '-':
			return char
		default:
			return '_'
		}
	}, filename)
	if len(filename) > 128 {
		filename = filename[:128]
	}
	if strings.Trim(filename, "._-") == "" {
		return "photo.jpg"
	}
	return filename
}

func safeFilePath(filePath string) (string, error) {
	if filePath == "" {
		return "", validation("file_path", "is missing from getFile response")
	}
	if !utf8.ValidString(filePath) || strings.ContainsAny(filePath, "\\?#") || strings.HasPrefix(filePath, "/") {
		return "", validation("file_path", "is invalid")
	}
	segments := strings.Split(filePath, "/")
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", validation("file_path", "is invalid")
		}
		escaped = append(escaped, url.PathEscape(segment))
	}
	return strings.Join(escaped, "/"), nil
}

func validateToken(token string) error {
	if token == "" {
		return validation("token", "is required")
	}
	for _, char := range token {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == ':' || char == '_' || char == '-' {
			continue
		}
		return validation("token", "has an invalid format")
	}
	return nil
}

func normalizeBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", validation("base_url", "must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", validation("base_url", "must use HTTP or HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", validation("base_url", "must not contain credentials, a query, or a fragment")
	}
	return value, nil
}

func (c *Client) methodURL(method string) string {
	return c.baseURL + "/bot" + c.token + "/" + method
}

func (c *Client) fileURL(filePath string) string {
	return c.baseURL + "/file/bot" + c.token + "/" + filePath
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		if seconds > int(maxAutomaticRetryDelay/time.Second) {
			return maxAutomaticRetryDelay + time.Second
		}
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return retryAt.Sub(now)
	}
	return 0
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
