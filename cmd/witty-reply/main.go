package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/bot"
	"github.com/aleka7sk/witty-reply/internal/config"
	"github.com/aleka7sk/witty-reply/internal/httpserver"
	"github.com/aleka7sk/witty-reply/internal/meme"
	"github.com/aleka7sk/witty-reply/internal/observability"
	"github.com/aleka7sk/witty-reply/internal/safety"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
	"github.com/aleka7sk/witty-reply/internal/transcribe"
)

const (
	cleanupInterval   = time.Hour
	cleanupRunTimeout = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("witty-reply stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runContext(ctx)
}

func runContext(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	metrics := observability.NewMetrics()
	dataStore, err := buildStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer dataStore.Close()

	telegramClient, err := telegram.New(cfg.Telegram.Token,
		telegram.WithBaseURL(cfg.Telegram.APIBaseURL),
		telegram.WithHTTPClient(&http.Client{Timeout: cfg.Telegram.RequestTimeout}),
		telegram.WithMaxDownloadBytes(int64(max(cfg.Limits.MaxImageBytes, cfg.Limits.MaxVoiceBytes))),
	)
	if err != nil {
		return fmt.Errorf("create Telegram client: %w", err)
	}
	botIdentity, err := telegramClient.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("authenticate Telegram bot: %w", err)
	}
	logger.Info("Telegram bot authenticated", "bot_id", botIdentity.ID, "bot_username", botIdentity.Username)

	provider, err := buildProvider(cfg)
	if err != nil {
		return err
	}
	threadsPublisher, err := buildThreadsPublisher(cfg)
	if err != nil {
		return err
	}
	transcriber, err := buildTranscriber(cfg)
	if err != nil {
		return err
	}
	usageLocation, err := time.LoadLocation(cfg.Limits.UsageTimezone)
	if err != nil {
		return fmt.Errorf("load usage timezone: %w", err)
	}
	sessions, err := session.New[bot.Interaction](cfg.Limits.SessionTTL)
	if err != nil {
		return fmt.Errorf("create session cache: %w", err)
	}
	defer sessions.Close()
	callbacks, err := session.NewCallbackCodec([]byte(cfg.Telegram.CallbackSecret), cfg.Store.ContentRetention)
	if err != nil {
		return fmt.Errorf("create callback codec: %w", err)
	}
	safetyFilter := safety.New(safety.Config{MaxRunes: 240, CandidateCount: 3})
	renderer, renderErr := meme.NewRenderer(meme.Config{FontPath: cfg.Meme.FontPath, Brand: cfg.Meme.Brand})
	if renderErr != nil {
		logger.Warn("meme renderer disabled", "error", renderErr)
	}

	service, err := bot.NewService(
		telegramClient, provider, transcriber, dataStore, sessions, callbacks, safetyFilter, renderer, metrics, logger,
		bot.Config{
			ProviderTimeout: cfg.AI.Timeout, UsageLocation: usageLocation, CallbackSecret: cfg.Telegram.CallbackSecret,
			PrivacyURL: cfg.PrivacyURL, SpeechProvider: cfg.Speech.Provider,
			BelcantoOperatorIDs: cfg.Belcanto.OperatorIDs, ThreadsPublisher: threadsPublisher,
			Limits: bot.Limits{
				TextDaily: cfg.Limits.TextDaily, MediaDaily: cfg.Limits.MediaDaily, MemeDaily: cfg.Limits.MemeDaily,
				RefinementDaily: cfg.Limits.RefinementDaily, StyleExamples: cfg.Limits.StyleExamples,
				MaxTextRunes: cfg.Limits.MaxTextRunes, MaxImageBytes: cfg.Limits.MaxImageBytes, MaxVoiceBytes: cfg.Limits.MaxVoiceBytes,
			},
		},
	)
	if err != nil {
		return fmt.Errorf("create bot service: %w", err)
	}
	processor, err := bot.NewProcessor(
		ctx, service, dataStore, cfg.Telegram.CallbackSecret,
		cfg.Telegram.WorkerCount, cfg.Telegram.QueueSize, metrics, logger,
	)
	if err != nil {
		return fmt.Errorf("create bot processor: %w", err)
	}
	defer func() {
		processor.Close()
		service.Wait()
	}()

	var webhookHandler http.Handler
	if cfg.Telegram.Mode == "webhook" {
		webhookHandler, err = telegram.NewWebhookHandler(cfg.Telegram.WebhookSecret, telegram.MaxWebhookBodyBytes, processor.Enqueue)
		if err != nil {
			return fmt.Errorf("create webhook handler: %w", err)
		}
	}
	server := httpserver.New(httpserver.Config{
		Address: cfg.HTTP.Address, ReadTimeout: cfg.HTTP.ReadTimeout, WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout: cfg.HTTP.IdleTimeout, WebhookPath: cfg.Telegram.WebhookPath, Environment: cfg.Environment,
	}, webhookHandler, metrics.Handler(), dataStore.Ping, logger)
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("HTTP server listening", "address", cfg.HTTP.Address)
		serverErrors <- server.ListenAndServe()
	}()

	transportErrors := make(chan error, 1)
	if cfg.Telegram.Mode == "webhook" {
		if err := telegramClient.SetWebhook(ctx, telegram.SetWebhookParams{
			URL: cfg.Telegram.WebhookURL + cfg.Telegram.WebhookPath, SecretToken: cfg.Telegram.WebhookSecret,
			AllowedUpdates: telegram.ProductAllowedUpdates(), MaxConnections: 1,
			DropPendingUpdates: cfg.Telegram.DropOldUpdates,
		}); err != nil {
			_ = shutdownServer(server, cfg.HTTP.ShutdownGrace)
			return fmt.Errorf("set Telegram webhook: %w", err)
		}
		logger.Info("Telegram webhook active", "path", cfg.Telegram.WebhookPath)
	} else {
		if err := telegramClient.DeleteWebhook(ctx, telegram.DeleteWebhookParams{DropPendingUpdates: cfg.Telegram.DropOldUpdates}); err != nil {
			_ = shutdownServer(server, cfg.HTTP.ShutdownGrace)
			return fmt.Errorf("delete Telegram webhook for polling: %w", err)
		}
		pollingClient, pollingErr := telegram.New(cfg.Telegram.Token,
			telegram.WithBaseURL(cfg.Telegram.APIBaseURL),
			telegram.WithHTTPClient(&http.Client{Timeout: cfg.Telegram.PollingTimeout + 10*time.Second}),
			telegram.WithMaxDownloadBytes(int64(max(cfg.Limits.MaxImageBytes, cfg.Limits.MaxVoiceBytes))),
		)
		if pollingErr != nil {
			_ = shutdownServer(server, cfg.HTTP.ShutdownGrace)
			return fmt.Errorf("create Telegram polling client: %w", pollingErr)
		}
		go func() { transportErrors <- runPolling(ctx, pollingClient, processor, cfg, logger) }()
		logger.Info("Telegram long polling active")
	}
	go runCleanup(ctx, dataStore, cfg.Store.ContentRetention, metrics, logger)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("HTTP server: %w", err)
		}
	case err := <-transportErrors:
		if err != nil && !errors.Is(err, context.Canceled) {
			runErr = fmt.Errorf("telegram transport: %w", err)
		}
	}
	if err := shutdownServer(server, cfg.HTTP.ShutdownGrace); err != nil && runErr == nil {
		runErr = err
	}
	logger.Info("shutdown complete")
	return runErr
}

func buildStore(ctx context.Context, cfg config.Config) (store.Store, error) {
	if cfg.Store.Driver == "memory" {
		return store.NewMemory(), nil
	}
	postgres, err := store.NewPostgres(ctx, cfg.Store.DatabaseURL, cfg.Store.MigrateOnStart)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL store: %w", err)
	}
	return postgres, nil
}

func buildProvider(cfg config.Config) (ai.Provider, error) {
	switch cfg.AI.Provider {
	case "fake":
		return ai.NewFake(), nil
	case "anthropic":
		provider, err := ai.NewAnthropic(ai.AnthropicConfig{
			APIKey: cfg.AI.AnthropicKey, BaseURL: cfg.AI.AnthropicURL, Model: cfg.AI.Model, Effort: cfg.AI.Effort,
			MaxTokens: cfg.AI.MaxTokens, Timeout: cfg.AI.Timeout, MaxRetries: cfg.AI.MaxRetries,
		})
		if err != nil {
			return nil, fmt.Errorf("create Anthropic provider: %w", err)
		}
		return provider, nil
	case "claude_cli":
		provider, err := ai.NewClaudeCLI(ai.ClaudeCLIConfig{
			Path: cfg.AI.ClaudeCLIPath, APIKey: cfg.AI.AnthropicKey, BaseURL: cfg.AI.AnthropicURL,
			Model: cfg.AI.ClaudeCLIModel, Timeout: cfg.AI.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("create Claude CLI provider: %w", err)
		}
		return provider, nil
	default:
		return nil, fmt.Errorf("unknown AI provider %q", cfg.AI.Provider)
	}
}

func buildThreadsPublisher(cfg config.Config) (threadspub.Publisher, error) {
	switch cfg.Belcanto.ThreadsProvider {
	case "disabled":
		return threadspub.NewDisabled(), nil
	case "fake":
		return threadspub.NewFake(), nil
	case "meta":
		publisher, err := threadspub.NewMeta(threadspub.Config{
			UserID: cfg.Belcanto.ThreadsUserID, AccessToken: cfg.Belcanto.ThreadsAccessToken,
			BaseURL: cfg.Belcanto.ThreadsBaseURL, Timeout: cfg.Belcanto.ThreadsTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("create Threads publisher: %w", err)
		}
		return publisher, nil
	default:
		return nil, fmt.Errorf("unknown Threads provider %q", cfg.Belcanto.ThreadsProvider)
	}
}

func buildTranscriber(cfg config.Config) (transcribe.Transcriber, error) {
	if cfg.Speech.Provider == "disabled" {
		return transcribe.NewDisabled(), nil
	}
	provider, err := transcribe.NewOpenAICompatible(transcribe.OpenAIConfig{
		BaseURL: cfg.Speech.BaseURL, APIKey: cfg.Speech.APIKey, Model: cfg.Speech.Model, Timeout: cfg.Speech.Timeout,
		MaxAudioBytes: cfg.Limits.MaxVoiceBytes, ResponseFormat: "verbose_json",
	})
	if err != nil {
		return nil, fmt.Errorf("create speech provider: %w", err)
	}
	return provider, nil
}

func runPolling(ctx context.Context, client *telegram.Client, processor *bot.Processor, cfg config.Config, logger *slog.Logger) error {
	var offset int64
	backoff := time.Second
	timeoutSeconds := max(1, int(cfg.Telegram.PollingTimeout/time.Second))
	for {
		updates, err := client.GetUpdates(ctx, telegram.GetUpdatesParams{
			Offset: offset, Limit: 100, TimeoutSeconds: timeoutSeconds, AllowedUpdates: telegram.ProductAllowedUpdates(),
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Warn("Telegram polling failed", "error", fmt.Sprintf("%T", err), "retry_in", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(10*time.Second, backoff*2)
			continue
		}
		backoff = time.Second
		for _, update := range updates {
			if err := processor.Enqueue(ctx, update); err != nil {
				return err
			}
			offset = max(offset, update.UpdateID+1)
		}
	}
}

func runCleanup(ctx context.Context, dataStore store.Store, retention time.Duration, metrics *observability.Metrics, logger *slog.Logger) {
	runCleanupSchedule(ctx, dataStore, retention, cleanupInterval, cleanupRunTimeout, metrics, logger)
}

func runCleanupSchedule(ctx context.Context, dataStore store.Store, retention, interval, timeout time.Duration, metrics *observability.Metrics, logger *slog.Logger) {
	if interval <= 0 {
		interval = cleanupInterval
	}
	if timeout <= 0 {
		timeout = cleanupRunTimeout
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		deleted, err := dataStore.Cleanup(cleanupCtx, time.Now().UTC().Add(-retention))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			metrics.Inc("cleanup_errors")
			logger.Error("retention cleanup failed", "error", fmt.Sprintf("%T", err))
			return
		}
		metrics.Add("cleanup_deleted", deleted)
		metrics.Set("cleanup_last_success_unixtime", time.Now().UTC().Unix())
	}
	cleanup()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func shutdownServer(server *http.Server, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown HTTP server: %w", err)
	}
	return nil
}
