package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testWebhookSecret = "Webhook_secret-123"

func TestWebhookDecoderAuthenticatesAndDecodes(t *testing.T) {
	decoder, err := NewWebhookDecoder(testWebhookSecret, 1024)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(
		`{"update_id":77,"future_field":{"safe":true},"message":{"message_id":8,"date":1,"chat":{"id":42,"type":"private"},"text":"hello"}}`,
	))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set(SecretTokenHeader, testWebhookSecret)

	update, err := decoder.Decode(request)
	if err != nil {
		t.Fatal(err)
	}
	if update.UpdateID != 77 || update.Message == nil || update.Message.Text != "hello" {
		t.Fatalf("update = %+v", update)
	}
}

func TestWebhookDecoderRejectsInvalidRequests(t *testing.T) {
	decoder, err := NewWebhookDecoder(testWebhookSecret, 32)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		method      string
		secret      string
		contentType string
		body        string
		wantCode    WebhookErrorCode
		wantStatus  int
	}{
		{
			name: "method", method: http.MethodGet, secret: testWebhookSecret,
			contentType: "application/json", body: `{}`,
			wantCode: WebhookMethodNotAllowed, wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name: "secret", method: http.MethodPost, secret: "wrong",
			contentType: "application/json", body: `{}`,
			wantCode: WebhookUnauthorized, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "media", method: http.MethodPost, secret: testWebhookSecret,
			contentType: "text/plain", body: `{}`,
			wantCode: WebhookUnsupportedMedia, wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "oversized", method: http.MethodPost, secret: testWebhookSecret,
			contentType: "application/json", body: strings.Repeat("x", 33),
			wantCode: WebhookBodyTooLarge, wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name: "trailing JSON", method: http.MethodPost, secret: testWebhookSecret,
			contentType: "application/json", body: `{} {}`,
			wantCode: WebhookInvalidJSON, wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/telegram/webhook", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set(SecretTokenHeader, test.secret)
			_, err := decoder.Decode(request)
			var webhookError *WebhookError
			if !errors.As(err, &webhookError) {
				t.Fatalf("error = %T %v", err, err)
			}
			if webhookError.Code != test.wantCode || webhookError.StatusCode != test.wantStatus {
				t.Fatalf("error = %+v", webhookError)
			}
			if strings.Contains(err.Error(), testWebhookSecret) || strings.Contains(err.Error(), test.body) {
				t.Fatalf("error retained sensitive request data: %v", err)
			}
		})
	}
}

func TestWebhookHandlerAcknowledgesOnlySuccess(t *testing.T) {
	var handled int64
	handler, err := NewWebhookHandler(testWebhookSecret, 1024, func(_ context.Context, update Update) error {
		handled = update.UpdateID
		if update.UpdateID == 2 {
			return errors.New("queue unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		method     string
		secret     string
		body       string
		wantStatus int
	}{
		{name: "accepted", method: http.MethodPost, secret: testWebhookSecret, body: `{"update_id":1}`, wantStatus: http.StatusNoContent},
		{name: "retry", method: http.MethodPost, secret: testWebhookSecret, body: `{"update_id":2}`, wantStatus: http.StatusServiceUnavailable},
		{name: "unauthorized", method: http.MethodPost, secret: "wrong", body: `{"update_id":3}`, wantStatus: http.StatusUnauthorized},
		{name: "method", method: http.MethodGet, secret: testWebhookSecret, body: `{"update_id":4}`, wantStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/telegram/webhook", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(SecretTokenHeader, test.secret)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if response.Body.Len() != 0 {
				t.Fatalf("response unexpectedly contains a body: %q", response.Body.String())
			}
			if test.wantStatus == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow = %q", response.Header().Get("Allow"))
			}
		})
	}
	if handled != 2 {
		t.Fatalf("last handled update = %d", handled)
	}
}
