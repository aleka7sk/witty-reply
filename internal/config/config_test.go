package config

import (
	"strings"
	"testing"
)

func TestLoadMinimalFakeConfig(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("STORE_DRIVER", "memory")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AI.Model != "claude-sonnet-5" {
		t.Fatalf("unexpected model: %q", cfg.AI.Model)
	}
	if cfg.Limits.UsageTimezone != "Asia/Almaty" {
		t.Fatalf("unexpected timezone: %q", cfg.Limits.UsageTimezone)
	}
	if cfg.Belcanto.ThreadsProvider != "disabled" {
		t.Fatalf("Threads provider = %q, want disabled", cfg.Belcanto.ThreadsProvider)
	}
}

func TestLoadBelcantoOperatorsAndFakeThreads(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("STORE_DRIVER", "memory")
	t.Setenv("BELCANTO_OPERATOR_IDS", " 42,77,42 ")
	t.Setenv("THREADS_PROVIDER", "fake")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Belcanto.OperatorIDs) != 2 || cfg.Belcanto.OperatorIDs[0] != 42 || cfg.Belcanto.OperatorIDs[1] != 77 {
		t.Fatalf("OperatorIDs = %#v", cfg.Belcanto.OperatorIDs)
	}
}

func TestLoadRejectsInvalidBelcantoOperator(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("BELCANTO_OPERATOR_IDS", "42,not-an-id")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "BELCANTO_OPERATOR_IDS") {
		t.Fatalf("Load() error = %v, want operator validation", err)
	}
}

func TestMetaThreadsRequiresCredentialsAndOperators(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("STORE_DRIVER", "memory")
	t.Setenv("THREADS_PROVIDER", "meta")

	_, err := Load()
	if err == nil {
		t.Fatal("meta Threads config without credentials was accepted")
	}
	for _, expected := range []string{"BELCANTO_OPERATOR_IDS", "THREADS_USER_ID", "THREADS_ACCESS_TOKEN"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("Load() error %q missing %q", err, expected)
		}
	}
}

func TestMetaThreadsTextOnlyConfigDoesNotRequirePublicMediaOrigin(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("STORE_DRIVER", "memory")
	t.Setenv("BELCANTO_OPERATOR_IDS", "42")
	t.Setenv("THREADS_PROVIDER", "meta")
	t.Setenv("THREADS_USER_ID", "17840000000000000")
	t.Setenv("THREADS_ACCESS_TOKEN", "token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("text-only Meta config was rejected: %v", err)
	}
	if cfg.Belcanto.ThreadsMediaBaseURL != "" {
		t.Fatalf("media base URL = %q, want empty", cfg.Belcanto.ThreadsMediaBaseURL)
	}
}

func TestThreadsMediaBaseURLUsesWebhookOriginAndRejectsPaths(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("STORE_DRIVER", "memory")
	t.Setenv("TELEGRAM_WEBHOOK_URL", "https://bot.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Belcanto.ThreadsMediaBaseURL != "https://bot.example.com" {
		t.Fatalf("media base URL = %q", cfg.Belcanto.ThreadsMediaBaseURL)
	}

	t.Setenv("THREADS_MEDIA_BASE_URL", "https://bot.example.com/private/path")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "THREADS_MEDIA_BASE_URL") {
		t.Fatalf("invalid media base URL error = %v", err)
	}
}

func TestDisabledThreadsIgnoresPathfulWebhookForMediaFallback(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("STORE_DRIVER", "memory")
	t.Setenv("TELEGRAM_WEBHOOK_URL", "https://bot.example.com/base")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("disabled Threads config rejected a pathful webhook base: %v", err)
	}
	if cfg.Belcanto.ThreadsMediaBaseURL != "" {
		t.Fatalf("media base URL = %q, want no unsafe fallback", cfg.Belcanto.ThreadsMediaBaseURL)
	}
}

func TestValidateProductionRequirements(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("AI_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("Load() error = %v, want missing API key", err)
	}
}

func TestProductionFailsClosed(t *testing.T) {
	t.Setenv("APP_ENV", "Production")
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("x", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("y", 32))
	t.Setenv("TELEGRAM_MODE", "polling")
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("STORE_DRIVER", "memory")

	_, err := Load()
	if err == nil {
		t.Fatal("unsafe production config was accepted")
	}
	for _, expected := range []string{"AI_PROVIDER=anthropic", "STORE_DRIVER=postgres", "TELEGRAM_MODE=webhook"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("production error %q missing %q", err, expected)
		}
	}
}

func TestProductionRequiresIndependentCallbackSecret(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("TELEGRAM_CALLBACK_SECRET", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_CALLBACK_SECRET") {
		t.Fatalf("Load() error = %v, want missing independent callback secret", err)
	}
}

func TestLoadValidProductionConfig(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("APP_ENV", "Production")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Environment != "production" {
		t.Fatalf("Environment = %q, want production", cfg.Environment)
	}
	if cfg.PrivacyURL == "" {
		t.Fatal("PrivacyURL was not loaded")
	}
}

func TestProductionRejectsUnsafeTransportAndPrivacyConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T)
		want   string
	}{
		{
			name: "Telegram plaintext",
			mutate: func(t *testing.T) {
				t.Setenv("TELEGRAM_API_BASE_URL", "http://api.telegram.org")
			},
			want: "TELEGRAM_API_BASE_URL",
		},
		{
			name: "Telegram host outside allowlist",
			mutate: func(t *testing.T) {
				t.Setenv("TELEGRAM_API_BASE_URL", "https://api.telegram.org.example.com")
			},
			want: "TELEGRAM_API_BASE_URL",
		},
		{
			name: "webhook credentials in URL",
			mutate: func(t *testing.T) {
				t.Setenv("TELEGRAM_WEBHOOK_URL", "https://user:password@bot.example.com")
			},
			want: "TELEGRAM_WEBHOOK_URL",
		},
		{
			name: "Anthropic plaintext",
			mutate: func(t *testing.T) {
				t.Setenv("ANTHROPIC_BASE_URL", "http://api.anthropic.com")
			},
			want: "ANTHROPIC_BASE_URL",
		},
		{
			name: "Anthropic credentials in URL",
			mutate: func(t *testing.T) {
				t.Setenv("ANTHROPIC_BASE_URL", "https://user:password@api.anthropic.com")
			},
			want: "ANTHROPIC_BASE_URL",
		},
		{
			name: "speech plaintext",
			mutate: func(t *testing.T) {
				t.Setenv("SPEECH_PROVIDER", "openai_compatible")
				t.Setenv("SPEECH_API_KEY", "speech-secret")
				t.Setenv("SPEECH_BASE_URL", "http://speech.example.com/v1")
			},
			want: "SPEECH_BASE_URL",
		},
		{
			name: "Threads host outside allowlist",
			mutate: func(t *testing.T) {
				t.Setenv("THREADS_PROVIDER", "meta")
				t.Setenv("BELCANTO_OPERATOR_IDS", "42")
				t.Setenv("THREADS_USER_ID", "17840000000000000")
				t.Setenv("THREADS_ACCESS_TOKEN", "threads-secret")
				t.Setenv("THREADS_API_BASE_URL", "https://graph.threads.net.example.com/v1.0")
			},
			want: "THREADS_API_BASE_URL",
		},
		{
			name: "database permits plaintext fallback",
			mutate: func(t *testing.T) {
				t.Setenv("DATABASE_URL", "postgres://witty:secret@db.example/witty?sslmode=prefer")
			},
			want: "DATABASE_URL must require TLS",
		},
		{
			name: "privacy URL missing",
			mutate: func(t *testing.T) {
				t.Setenv("PRIVACY_URL", "")
			},
			want: "production requires PRIVACY_URL",
		},
		{
			name: "privacy URL plaintext",
			mutate: func(t *testing.T) {
				t.Setenv("PRIVACY_URL", "http://example.com/privacy")
			},
			want: "PRIVACY_URL must be an absolute HTTPS URL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setValidProductionEnvironment(t)
			test.mutate(t)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsUnknownEnvironment(t *testing.T) {
	setValidProductionEnvironment(t)
	t.Setenv("APP_ENV", "staging")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "APP_ENV must be") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestDatabaseRequiresTLS(t *testing.T) {
	t.Setenv("PGSSLMODE", "prefer")
	tests := []struct {
		name       string
		connection string
		want       bool
	}{
		{name: "require", connection: "postgres://witty:secret@db.example/witty?sslmode=require", want: true},
		{name: "verify CA", connection: "postgres://witty:secret@db.example/witty?sslmode=verify-ca", want: true},
		{name: "verify full", connection: "postgres://witty:secret@db.example/witty?sslmode=verify-full", want: true},
		{name: "keyword value", connection: "host=db.example user=witty password=secret dbname=witty sslmode=require", want: true},
		{name: "prefer", connection: "postgres://witty:secret@db.example/witty?sslmode=prefer", want: false},
		{name: "disable", connection: "postgres://witty:secret@db.example/witty?sslmode=disable", want: false},
		{name: "malformed", connection: "%not-a-connection", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := databaseRequiresTLS(test.connection); got != test.want {
				t.Fatalf("databaseRequiresTLS() = %v, want %v", got, test.want)
			}
		})
	}
}

func setValidProductionEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENV", "production")
	t.Setenv("TELEGRAM_BOT_TOKEN", strings.Repeat("t", 32))
	t.Setenv("TELEGRAM_CALLBACK_SECRET", strings.Repeat("c", 32))
	t.Setenv("TELEGRAM_API_BASE_URL", "https://api.telegram.org")
	t.Setenv("TELEGRAM_MODE", "webhook")
	t.Setenv("TELEGRAM_WEBHOOK_URL", "https://bot.example.com")
	t.Setenv("TELEGRAM_WEBHOOK_PATH", "/telegram/webhook")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", strings.Repeat("w", 32))
	t.Setenv("AI_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	t.Setenv("STORE_DRIVER", "postgres")
	t.Setenv("DATABASE_URL", "postgres://witty:secret@db.example/witty?sslmode=require")
	t.Setenv("SPEECH_PROVIDER", "disabled")
	t.Setenv("SPEECH_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("PRIVACY_URL", "https://github.com/aleka7sk/witty-reply/blob/main/docs/privacy-policy.md")
}
