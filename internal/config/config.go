package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	productionTelegramAPIHost = "api.telegram.org"
	productionThreadsAPIHost  = "graph.threads.net"
	productionPexelsAPIHost   = "api.pexels.com"
)

type Config struct {
	Environment string
	LogLevel    slog.Level
	PrivacyURL  string

	Telegram Telegram
	AI       AI
	Store    Store
	HTTP     HTTP
	Limits   Limits
	Meme     Meme
	Speech   Speech
	Belcanto Belcanto
}

type Telegram struct {
	Token          string
	APIBaseURL     string
	Mode           string
	WebhookURL     string
	WebhookPath    string
	WebhookSecret  string
	CallbackSecret string
	PollingTimeout time.Duration
	DropOldUpdates bool
	WorkerCount    int
	QueueSize      int
	RequestTimeout time.Duration
}

type AI struct {
	Provider       string
	AnthropicKey   string
	AnthropicURL   string
	Model          string
	MaxTokens      int
	Effort         string
	Timeout        time.Duration
	MaxRetries     int
	ClaudeCLIPath  string
	ClaudeCLIModel string
}

type Store struct {
	Driver           string
	DatabaseURL      string
	MigrateOnStart   bool
	ContentRetention time.Duration
}

type HTTP struct {
	Address       string
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
	ShutdownGrace time.Duration
}

type Limits struct {
	TextDaily       int
	MediaDaily      int
	MemeDaily       int
	RefinementDaily int
	StyleExamples   int
	MaxTextRunes    int
	MaxImageBytes   int
	MaxVoiceBytes   int
	SessionTTL      time.Duration
	UsageTimezone   string
}

type Meme struct {
	FontPath string
	Brand    string
}

type Speech struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
	Timeout  time.Duration
}

// Belcanto configures the operator-only Threads copilot. It is optional so the
// existing Witty Reply product can run without any Meta credentials.
type Belcanto struct {
	OperatorIDs         []int64
	GenerationTimeout   time.Duration
	ReviewLogMode       string
	ThreadsProvider     string
	ThreadsUserID       string
	ThreadsAccessToken  string
	ThreadsBaseURL      string
	ThreadsMediaBaseURL string
	ThreadsTimeout      time.Duration
	PhotoProvider       string
	PexelsAPIKey        string
	PexelsBaseURL       string
	PexelsTimeout       time.Duration
}

func Load() (Config, error) {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL: %w", err)
	}

	operatorIDs, err := int64List("BELCANTO_OPERATOR_IDS")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Environment: strings.ToLower(get("APP_ENV", "development")),
		LogLevel:    level,
		PrivacyURL:  get("PRIVACY_URL", ""),
		Telegram: Telegram{
			Token: get("TELEGRAM_BOT_TOKEN", ""), APIBaseURL: strings.TrimRight(get("TELEGRAM_API_BASE_URL", "https://api.telegram.org"), "/"),
			Mode: get("TELEGRAM_MODE", "polling"), WebhookURL: strings.TrimRight(get("TELEGRAM_WEBHOOK_URL", ""), "/"),
			WebhookPath: get("TELEGRAM_WEBHOOK_PATH", "/telegram/webhook"), WebhookSecret: get("TELEGRAM_WEBHOOK_SECRET", ""),
			CallbackSecret: get("TELEGRAM_CALLBACK_SECRET", ""), PollingTimeout: duration("TELEGRAM_POLL_TIMEOUT", 45*time.Second),
			DropOldUpdates: boolean("TELEGRAM_DROP_OLD_UPDATES", false), WorkerCount: integer("BOT_WORKERS", 4), QueueSize: integer("BOT_QUEUE_SIZE", 128),
			RequestTimeout: duration("TELEGRAM_REQUEST_TIMEOUT", 20*time.Second),
		},
		AI: AI{
			Provider: get("AI_PROVIDER", "fake"), AnthropicKey: get("ANTHROPIC_API_KEY", ""),
			AnthropicURL: strings.TrimRight(get("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/"), Model: get("ANTHROPIC_MODEL", "claude-sonnet-5"),
			MaxTokens: integer("AI_MAX_TOKENS", 1800), Effort: get("AI_EFFORT", "low"), Timeout: duration("AI_TIMEOUT", 45*time.Second),
			MaxRetries: integer("AI_MAX_RETRIES", 2), ClaudeCLIPath: get("CLAUDE_CLI_PATH", "claude"), ClaudeCLIModel: get("CLAUDE_CLI_MODEL", ""),
		},
		Store: Store{
			Driver: get("STORE_DRIVER", "memory"), DatabaseURL: get("DATABASE_URL", ""),
			MigrateOnStart: boolean("MIGRATE_ON_START", true), ContentRetention: duration("CONTENT_RETENTION", 7*24*time.Hour),
		},
		HTTP: HTTP{
			Address: get("HTTP_ADDRESS", ":8080"), ReadTimeout: duration("HTTP_READ_TIMEOUT", 10*time.Second),
			WriteTimeout: duration("HTTP_WRITE_TIMEOUT", 15*time.Second), IdleTimeout: duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownGrace: duration("SHUTDOWN_GRACE", 15*time.Second),
		},
		Limits: Limits{
			TextDaily: integer("FREE_TEXT_DAILY_LIMIT", 10), MediaDaily: integer("FREE_MEDIA_DAILY_LIMIT", 3),
			MemeDaily: integer("FREE_MEME_DAILY_LIMIT", 1), RefinementDaily: integer("FREE_REFINEMENT_DAILY_LIMIT", 10),
			StyleExamples: integer("FREE_STYLE_EXAMPLES_LIMIT", 5), MaxTextRunes: integer("MAX_TEXT_RUNES", 6000),
			MaxImageBytes: integer("MAX_IMAGE_BYTES", 10<<20), MaxVoiceBytes: integer("MAX_VOICE_BYTES", 20<<20),
			SessionTTL:    duration("SESSION_TTL", 30*time.Minute),
			UsageTimezone: get("USAGE_TIMEZONE", "Asia/Almaty"),
		},
		Meme: Meme{FontPath: get("MEME_FONT_PATH", ""), Brand: get("MEME_BRAND", "WITTY REPLY")},
		Speech: Speech{
			Provider: get("SPEECH_PROVIDER", "disabled"), BaseURL: strings.TrimRight(get("SPEECH_BASE_URL", "https://api.openai.com/v1"), "/"),
			APIKey: get("SPEECH_API_KEY", ""), Model: get("SPEECH_MODEL", "whisper-1"), Timeout: duration("SPEECH_TIMEOUT", 60*time.Second),
		},
		Belcanto: Belcanto{
			OperatorIDs: operatorIDs, GenerationTimeout: duration("THREADS_AI_TIMEOUT", 120*time.Second),
			ReviewLogMode:   strings.ToLower(get("BELCANTO_REVIEW_LOG_MODE", "full")),
			ThreadsProvider: strings.ToLower(get("THREADS_PROVIDER", "disabled")),
			ThreadsUserID:   get("THREADS_USER_ID", ""), ThreadsAccessToken: get("THREADS_ACCESS_TOKEN", ""),
			ThreadsBaseURL:      strings.TrimRight(get("THREADS_API_BASE_URL", "https://graph.threads.net/v1.0"), "/"),
			ThreadsMediaBaseURL: strings.TrimRight(get("THREADS_MEDIA_BASE_URL", ""), "/"),
			ThreadsTimeout:      duration("THREADS_TIMEOUT", 20*time.Second),
			PhotoProvider:       strings.ToLower(get("THREADS_PHOTO_PROVIDER", "disabled")),
			PexelsAPIKey:        get("PEXELS_API_KEY", ""),
			PexelsBaseURL:       strings.TrimRight(get("PEXELS_API_BASE_URL", "https://api.pexels.com/v1"), "/"),
			PexelsTimeout:       duration("PEXELS_TIMEOUT", 10*time.Second),
		},
	}
	if cfg.Belcanto.ThreadsMediaBaseURL == "" && isAbsoluteHTTPSOrigin(cfg.Telegram.WebhookURL) {
		cfg.Belcanto.ThreadsMediaBaseURL = cfg.Telegram.WebhookURL
	}

	// Reusing the bot token is convenient for a local smoke test, but production
	// must use an independently rotatable secret for callbacks, queue encryption,
	// pseudonymous metrics, and source digests.
	if cfg.Telegram.CallbackSecret == "" && cfg.Telegram.Token != "" && cfg.Environment != "production" {
		cfg.Telegram.CallbackSecret = cfg.Telegram.Token
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	environment := strings.ToLower(strings.TrimSpace(c.Environment))
	switch environment {
	case "development", "test", "production":
	default:
		errs = append(errs, errors.New("APP_ENV must be development, test, or production"))
	}
	if c.Telegram.Token == "" {
		errs = append(errs, errors.New("TELEGRAM_BOT_TOKEN is required"))
	}
	if c.Telegram.Mode != "polling" && c.Telegram.Mode != "webhook" {
		errs = append(errs, errors.New("TELEGRAM_MODE must be polling or webhook"))
	}
	if c.Telegram.Mode == "webhook" {
		if c.Telegram.WebhookURL == "" {
			errs = append(errs, errors.New("TELEGRAM_WEBHOOK_URL is required in webhook mode"))
		} else if !isAbsoluteHTTPSURL(c.Telegram.WebhookURL, false) {
			errs = append(errs, errors.New("TELEGRAM_WEBHOOK_URL must be an absolute HTTPS URL without credentials, query, or fragment"))
		}
		if !strings.HasPrefix(c.Telegram.WebhookPath, "/") || c.Telegram.WebhookPath == "/" {
			errs = append(errs, errors.New("TELEGRAM_WEBHOOK_PATH must start with / and must not be /"))
		}
		if len(c.Telegram.WebhookSecret) < 16 {
			errs = append(errs, errors.New("TELEGRAM_WEBHOOK_SECRET must contain at least 16 characters"))
		}
	}
	if len(c.Telegram.CallbackSecret) < 16 {
		errs = append(errs, errors.New("TELEGRAM_CALLBACK_SECRET must contain at least 16 characters (development may fall back to the bot token)"))
	}
	if c.AI.Provider != "fake" && c.AI.Provider != "anthropic" && c.AI.Provider != "claude_cli" {
		errs = append(errs, errors.New("AI_PROVIDER must be fake, anthropic, or claude_cli"))
	}
	if c.AI.Provider == "anthropic" {
		if c.AI.AnthropicKey == "" {
			errs = append(errs, errors.New("ANTHROPIC_API_KEY is required for anthropic provider"))
		}
		if c.AI.Model == "" {
			errs = append(errs, errors.New("ANTHROPIC_MODEL is required for anthropic provider"))
		}
	}
	if c.AI.Provider == "claude_cli" && c.AI.AnthropicKey == "" {
		errs = append(errs, errors.New("ANTHROPIC_API_KEY is required for secure Claude CLI bare mode"))
	}
	if c.Store.Driver != "memory" && c.Store.Driver != "postgres" {
		errs = append(errs, errors.New("STORE_DRIVER must be memory or postgres"))
	}
	if c.Store.Driver == "postgres" && c.Store.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required for postgres store"))
	}
	if c.Store.ContentRetention <= 0 || c.Limits.SessionTTL <= 0 {
		errs = append(errs, errors.New("CONTENT_RETENTION and SESSION_TTL must be positive"))
	}
	if c.Speech.Provider != "disabled" && c.Speech.Provider != "openai_compatible" {
		errs = append(errs, errors.New("SPEECH_PROVIDER must be disabled or openai_compatible"))
	}
	if c.Speech.Provider == "openai_compatible" && c.Speech.APIKey == "" {
		errs = append(errs, errors.New("SPEECH_API_KEY is required for openai_compatible speech"))
	}
	switch c.Belcanto.ThreadsProvider {
	case "disabled", "fake":
	case "meta":
		if len(c.Belcanto.OperatorIDs) == 0 {
			errs = append(errs, errors.New("BELCANTO_OPERATOR_IDS is required when THREADS_PROVIDER=meta"))
		}
		if c.Belcanto.ThreadsUserID == "" {
			errs = append(errs, errors.New("THREADS_USER_ID is required when THREADS_PROVIDER=meta"))
		} else if !digitsOnly(c.Belcanto.ThreadsUserID) {
			errs = append(errs, errors.New("THREADS_USER_ID must contain only digits"))
		}
		if c.Belcanto.ThreadsAccessToken == "" {
			errs = append(errs, errors.New("THREADS_ACCESS_TOKEN is required when THREADS_PROVIDER=meta"))
		}
	default:
		errs = append(errs, errors.New("THREADS_PROVIDER must be disabled, fake, or meta"))
	}
	if c.Belcanto.ThreadsTimeout < time.Second {
		errs = append(errs, errors.New("THREADS_TIMEOUT must be at least one second"))
	}
	if c.Belcanto.GenerationTimeout < 5*time.Second {
		errs = append(errs, errors.New("THREADS_AI_TIMEOUT must be at least five seconds"))
	}
	switch c.Belcanto.ReviewLogMode {
	case "off", "metadata", "full":
	default:
		errs = append(errs, errors.New("BELCANTO_REVIEW_LOG_MODE must be off, metadata, or full"))
	}
	switch c.Belcanto.PhotoProvider {
	case "disabled":
	case "pexels":
		if c.Belcanto.PexelsAPIKey == "" {
			errs = append(errs, errors.New("PEXELS_API_KEY is required when THREADS_PHOTO_PROVIDER=pexels"))
		}
	default:
		errs = append(errs, errors.New("THREADS_PHOTO_PROVIDER must be disabled or pexels"))
	}
	if c.Belcanto.PexelsTimeout < time.Second {
		errs = append(errs, errors.New("PEXELS_TIMEOUT must be at least one second"))
	}
	if !isAbsoluteHTTPSURL(c.Belcanto.PexelsBaseURL, false) {
		errs = append(errs, errors.New("PEXELS_API_BASE_URL must be an absolute HTTPS URL without credentials, query, or fragment"))
	}
	if c.Belcanto.ThreadsMediaBaseURL != "" && !isAbsoluteHTTPSOrigin(c.Belcanto.ThreadsMediaBaseURL) {
		errs = append(errs, errors.New("THREADS_MEDIA_BASE_URL must be an absolute HTTPS origin without credentials, path, query, or fragment"))
	}
	if c.Telegram.WorkerCount < 1 || c.Telegram.QueueSize < 1 {
		errs = append(errs, errors.New("BOT_WORKERS and BOT_QUEUE_SIZE must be positive"))
	}
	if c.Limits.TextDaily < 1 || c.Limits.MediaDaily < 1 || c.Limits.MemeDaily < 1 || c.Limits.RefinementDaily < 1 || c.Limits.MaxTextRunes < 1 || c.Limits.MaxImageBytes < 1 {
		errs = append(errs, errors.New("usage and input limits must be positive"))
	}
	if c.AI.Effort != "low" && c.AI.Effort != "medium" && c.AI.Effort != "high" {
		errs = append(errs, errors.New("AI_EFFORT must be low, medium, or high"))
	}
	if _, err := time.LoadLocation(c.Limits.UsageTimezone); err != nil {
		errs = append(errs, fmt.Errorf("USAGE_TIMEZONE: %w", err))
	}
	if c.PrivacyURL != "" && !isAbsoluteHTTPSURL(c.PrivacyURL, true) {
		errs = append(errs, errors.New("PRIVACY_URL must be an absolute HTTPS URL without credentials or fragment"))
	}
	if environment == "production" {
		if c.AI.Provider != "anthropic" {
			errs = append(errs, errors.New("production requires AI_PROVIDER=anthropic"))
		}
		if c.Store.Driver != "postgres" {
			errs = append(errs, errors.New("production requires STORE_DRIVER=postgres"))
		}
		if c.Telegram.Mode != "webhook" {
			errs = append(errs, errors.New("production requires TELEGRAM_MODE=webhook"))
		}
		if c.PrivacyURL == "" {
			errs = append(errs, errors.New("production requires PRIVACY_URL"))
		}
		if !isProductionTelegramAPIURL(c.Telegram.APIBaseURL) {
			errs = append(errs, errors.New("production TELEGRAM_API_BASE_URL must be https://api.telegram.org"))
		}
		if !isAbsoluteHTTPSURL(c.AI.AnthropicURL, false) {
			errs = append(errs, errors.New("production ANTHROPIC_BASE_URL must be an absolute HTTPS URL without credentials, query, or fragment"))
		}
		if c.Speech.Provider == "openai_compatible" && !isAbsoluteHTTPSURL(c.Speech.BaseURL, false) {
			errs = append(errs, errors.New("production SPEECH_BASE_URL must be an absolute HTTPS URL without credentials, query, or fragment"))
		}
		if c.Belcanto.ThreadsProvider == "meta" && !isProductionThreadsAPIURL(c.Belcanto.ThreadsBaseURL) {
			errs = append(errs, errors.New("production THREADS_API_BASE_URL must use https://graph.threads.net with a version path"))
		}
		if c.Belcanto.PhotoProvider == "pexels" && !isProductionPexelsAPIURL(c.Belcanto.PexelsBaseURL) {
			errs = append(errs, errors.New("production PEXELS_API_BASE_URL must use https://api.pexels.com/v1"))
		}
		if c.Store.Driver == "postgres" && c.Store.DatabaseURL != "" && !databaseRequiresTLS(c.Store.DatabaseURL) {
			errs = append(errs, errors.New("production DATABASE_URL must require TLS (sslmode=require, verify-ca, or verify-full)"))
		}
	}
	return errors.Join(errs...)
}

func isAbsoluteHTTPSURL(raw string, allowQuery bool) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	return allowQuery || parsed.RawQuery == ""
}

func isAbsoluteHTTPSOrigin(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	return true
}

func isProductionTelegramAPIURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), productionTelegramAPIHost) {
		return false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	return parsed.Port() == "" || parsed.Port() == "443"
}

func isProductionThreadsAPIURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), productionThreadsAPIHost) {
		return false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	path := strings.Trim(parsed.Path, "/")
	return strings.HasPrefix(path, "v") && len(path) > 1
}

func isProductionPexelsAPIURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), productionPexelsAPIHost) {
		return false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	return strings.Trim(parsed.Path, "/") == "v1"
}

func databaseRequiresTLS(databaseURL string) bool {
	parsed, err := pgxpool.ParseConfig(databaseURL)
	if err != nil || parsed.ConnConfig.TLSConfig == nil {
		return false
	}
	for _, fallback := range parsed.ConnConfig.Fallbacks {
		if fallback == nil || fallback.TLSConfig == nil {
			return false
		}
	}
	return true
}

func RandomSecret(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func get(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func integer(key string, fallback int) int {
	value := get(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func boolean(key string, fallback bool) bool {
	value := get(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func duration(key string, fallback time.Duration) time.Duration {
	value := get(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func int64List(key string) ([]int64, error) {
	raw := get(key, "")
	if raw == "" {
		return nil, nil
	}
	seen := make(map[int64]struct{})
	values := make([]int64, 0)
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("%s must be a comma-separated list of positive Telegram IDs", key)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}

func digitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
