package httpserver

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Config struct {
	Address      string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	WebhookPath  string
	Environment  string
}

type Readiness func(context.Context) error

func New(config Config, webhook http.Handler, metrics http.Handler, readiness Readiness, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", method(http.MethodGet, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	}))
	mux.HandleFunc("/readyz", method(http.MethodGet, func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		if readiness != nil && readiness(ctx) != nil {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ready\n"))
	}))
	if metrics != nil {
		mux.Handle("/metrics", methodHandler(http.MethodGet, metrics))
	}
	if webhook != nil && config.WebhookPath != "" {
		mux.Handle(config.WebhookPath, webhook)
	}

	handler := securityHeaders(mux, config.Environment)
	return &http.Server{
		Addr: config.Address, Handler: handler, ReadTimeout: config.ReadTimeout, WriteTimeout: config.WriteTimeout,
		IdleTimeout: config.IdleTimeout, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

func method(allowed string, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if subtle.ConstantTimeCompare([]byte(request.Method), []byte(allowed)) != 1 {
			writer.Header().Set("Allow", allowed)
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(writer, request)
	}
}

func methodHandler(allowed string, next http.Handler) http.Handler {
	return method(allowed, next.ServeHTTP)
}

func securityHeaders(next http.Handler, environment string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Cache-Control", "no-store")
		if strings.EqualFold(environment, "production") {
			writer.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(writer, request)
	})
}
