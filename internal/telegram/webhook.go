package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
)

const (
	SecretTokenHeader         = "X-Telegram-Bot-Api-Secret-Token"
	MaxWebhookBodyBytes int64 = 1 << 20
)

type UpdateHandler func(context.Context, Update) error

type WebhookDecoder struct {
	secretHash   [sha256.Size]byte
	maxBodyBytes int64
}

func NewWebhookDecoder(secret string, maxBodyBytes int64) (*WebhookDecoder, error) {
	if err := validateWebhookSecret(secret, false); err != nil {
		return nil, err
	}
	if maxBodyBytes == 0 {
		maxBodyBytes = MaxWebhookBodyBytes
	}
	if maxBodyBytes < 1 {
		return nil, validation("webhook_max_body_bytes", "must be positive")
	}
	return &WebhookDecoder{
		secretHash:   sha256.Sum256([]byte(secret)),
		maxBodyBytes: maxBodyBytes,
	}, nil
}

// Decode authenticates and bounds the request before decoding it. Unknown JSON
// fields are accepted so Bot API additions do not break existing deployments.
func (d *WebhookDecoder) Decode(request *http.Request) (Update, error) {
	if request == nil || request.Method != http.MethodPost {
		return Update{}, webhookError(WebhookMethodNotAllowed, http.StatusMethodNotAllowed)
	}
	providedHash := sha256.Sum256([]byte(request.Header.Get(SecretTokenHeader)))
	if subtle.ConstantTimeCompare(d.secretHash[:], providedHash[:]) != 1 {
		return Update{}, webhookError(WebhookUnauthorized, http.StatusUnauthorized)
	}

	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Update{}, webhookError(WebhookUnsupportedMedia, http.StatusUnsupportedMediaType)
	}
	if request.ContentLength > d.maxBodyBytes {
		return Update{}, webhookError(WebhookBodyTooLarge, http.StatusRequestEntityTooLarge)
	}
	if request.Body == nil {
		return Update{}, webhookError(WebhookInvalidJSON, http.StatusBadRequest)
	}

	body, exceeded, err := readBounded(request.Body, d.maxBodyBytes)
	if err != nil {
		return Update{}, webhookError(WebhookInvalidJSON, http.StatusBadRequest)
	}
	if exceeded {
		return Update{}, webhookError(WebhookBodyTooLarge, http.StatusRequestEntityTooLarge)
	}
	var update Update
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&update); err != nil {
		return Update{}, webhookError(WebhookInvalidJSON, http.StatusBadRequest)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Update{}, webhookError(WebhookInvalidJSON, http.StatusBadRequest)
	}
	return update, nil
}

// Handler acknowledges an update only after next returns nil. In production,
// next should return after the update has been durably queued or fully handled;
// a non-nil result produces 503 so Telegram will retry delivery.
func (d *WebhookDecoder) Handler(next UpdateHandler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		update, err := d.Decode(request)
		if err != nil {
			var webhookErr *WebhookError
			status := http.StatusBadRequest
			if errors.As(err, &webhookErr) {
				status = webhookErr.StatusCode
			}
			if status == http.StatusMethodNotAllowed {
				writer.Header().Set("Allow", http.MethodPost)
			}
			writeWebhookStatus(writer, status)
			return
		}
		if next == nil || next(request.Context(), update) != nil {
			writeWebhookStatus(writer, http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusNoContent)
	})
}

func NewWebhookHandler(secret string, maxBodyBytes int64, next UpdateHandler) (http.Handler, error) {
	decoder, err := NewWebhookDecoder(secret, maxBodyBytes)
	if err != nil {
		return nil, err
	}
	if next == nil {
		return nil, validation("webhook_handler", "must not be nil")
	}
	return decoder.Handler(next), nil
}

func webhookError(code WebhookErrorCode, status int) error {
	return &WebhookError{Code: code, StatusCode: status}
}

func writeWebhookStatus(writer http.ResponseWriter, status int) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", "0")
	writer.WriteHeader(status)
}
