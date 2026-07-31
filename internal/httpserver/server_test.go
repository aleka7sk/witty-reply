package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthAndReadiness(t *testing.T) {
	server := New(Config{WebhookPath: "/telegram/webhook", Environment: "production"}, nil, nil, func(context.Context) error { return nil }, nil)
	health := httptest.NewRecorder()
	server.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("health response = %d, headers=%v", health.Code, health.Header())
	}

	server = New(Config{}, nil, nil, func(context.Context) error { return errors.New("down") }, nil)
	ready := httptest.NewRecorder()
	server.Handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness response = %d", ready.Code)
	}
}

func TestHealthRejectsPost(t *testing.T) {
	server := New(Config{}, nil, nil, nil, nil)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("response = %d", response.Code)
	}
}
