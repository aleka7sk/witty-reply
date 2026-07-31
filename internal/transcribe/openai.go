package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	defaultMaxAudioBytes    = 20 << 20
	defaultMaxResponseBytes = 2 << 20
	defaultTimeout          = 60 * time.Second
)

type OpenAIConfig struct {
	BaseURL          string
	APIKey           string
	Model            string
	Timeout          time.Duration
	MaxAudioBytes    int
	MaxResponseBytes int64
	ResponseFormat   string
	HTTPClient       *http.Client
}

type OpenAICompatible struct {
	endpoint         string
	apiKey           string
	model            string
	maxAudioBytes    int
	maxResponseBytes int64
	responseFormat   string
	client           *http.Client
}

type HTTPError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("transcription provider returned HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("transcription provider returned HTTP %d: %s", e.StatusCode, e.Message)
}

func NewOpenAICompatible(cfg OpenAIConfig) (*OpenAICompatible, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("transcription base URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("transcription base URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("transcription base URL must not contain credentials, a query, or a fragment")
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("transcription base URL must use HTTPS (HTTP is allowed only for loopback testing)")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("transcription model is required")
	}
	if !strings.HasSuffix(parsed.Path, "/audio/transcriptions") {
		baseURL += "/audio/transcriptions"
	}
	if cfg.MaxAudioBytes <= 0 {
		cfg.MaxAudioBytes = defaultMaxAudioBytes
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxResponseBytes
	}
	if cfg.ResponseFormat == "" {
		cfg.ResponseFormat = "json"
	}
	if cfg.ResponseFormat != "json" && cfg.ResponseFormat != "verbose_json" {
		return nil, errors.New("response format must be json or verbose_json")
	}
	client := &http.Client{}
	if cfg.HTTPClient == nil {
		if cfg.Timeout <= 0 {
			cfg.Timeout = defaultTimeout
		}
		client.Timeout = cfg.Timeout
	} else {
		clientCopy := *cfg.HTTPClient
		client = &clientCopy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &OpenAICompatible{
		endpoint:         baseURL,
		apiKey:           strings.TrimSpace(cfg.APIKey),
		model:            strings.TrimSpace(cfg.Model),
		maxAudioBytes:    cfg.MaxAudioBytes,
		maxResponseBytes: cfg.MaxResponseBytes,
		responseFormat:   cfg.ResponseFormat,
		client:           client,
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *OpenAICompatible) Transcribe(ctx context.Context, audio Audio) (Result, error) {
	if len(audio.Data) == 0 {
		return Result{}, ErrEmptyAudio
	}
	if len(audio.Data) > t.maxAudioBytes {
		return Result{}, ErrAudioTooLarge
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filename := safeFilename(audio.Filename)
	mediaType := strings.TrimSpace(audio.MediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	} else if parsedType, _, parseErr := mime.ParseMediaType(mediaType); parseErr != nil {
		mediaType = "application/octet-stream"
	} else {
		mediaType = parsedType
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(filename)))
	header.Set("Content-Type", mediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return Result{}, fmt.Errorf("create transcription file part: %w", err)
	}
	if _, err := part.Write(audio.Data); err != nil {
		return Result{}, fmt.Errorf("write transcription file part: %w", err)
	}
	for name, value := range map[string]string{
		"model":           t.model,
		"response_format": t.responseFormat,
		"language":        strings.TrimSpace(audio.Language),
		"prompt":          strings.TrimSpace(audio.Prompt),
	} {
		if value != "" {
			if err := writer.WriteField(name, value); err != nil {
				return Result{}, fmt.Errorf("write transcription field: %w", err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		return Result{}, fmt.Errorf("close transcription request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, &body)
	if err != nil {
		return Result{}, fmt.Errorf("create transcription request: %w", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	if t.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	response, err := t.client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("transcription request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := readBounded(response.Body, t.maxResponseBytes)
	if err != nil {
		return Result{}, fmt.Errorf("read transcription response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Result{}, parseHTTPError(response.StatusCode, responseBody)
	}

	var decoded providerResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return Result{}, fmt.Errorf("decode transcription response: %w", err)
	}
	decoded.Text = strings.TrimSpace(decoded.Text)
	if decoded.Text == "" {
		return Result{}, ErrEmptyTranscript
	}
	return Result{
		Text:       decoded.Text,
		Language:   strings.TrimSpace(decoded.Language),
		Duration:   decoded.Duration,
		Confidence: responseConfidence(decoded),
		Provider:   "openai_compatible",
		Model:      t.model,
	}, nil
}

type providerResponse struct {
	Text       string  `json:"text"`
	Language   string  `json:"language"`
	Duration   float64 `json:"duration"`
	Confidence float64 `json:"confidence"`
	Segments   []struct {
		AvgLogprob float64 `json:"avg_logprob"`
	} `json:"segments"`
}

func responseConfidence(response providerResponse) float64 {
	if response.Confidence > 0 {
		return clamp(response.Confidence, 0, 1)
	}
	if len(response.Segments) == 0 {
		return 0
	}
	total := 0.0
	for _, segment := range response.Segments {
		total += math.Exp(segment.AvgLogprob)
	}
	return clamp(total/float64(len(response.Segments)), 0, 1)
}

func clamp(value, minimum, maximum float64) float64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("response exceeds configured limit")
	}
	return data, nil
}

func parseHTTPError(status int, body []byte) error {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &envelope)
	message := envelope.Error.Message
	if message == "" {
		message = envelope.Message
	}
	if message == "" {
		message = http.StatusText(status)
	}
	code := envelope.Error.Code
	if code == "" {
		code = envelope.Error.Type
	}
	return &HTTPError{StatusCode: status, Code: safeErrorText(code, 80), Message: safeErrorText(message, 512)}
}

func safeFilename(value string) string {
	value = filepath.Base(strings.TrimSpace(value))
	if value == "" || value == "." || value == string(filepath.Separator) {
		return "audio.ogg"
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	runes := []rune(b.String())
	if len(runes) > 128 {
		runes = runes[:128]
	}
	return string(runes)
}

func safeErrorText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes])
	}
	return value
}

func escapeQuotes(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}
