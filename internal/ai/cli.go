package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

// ClaudeCLIConfig is intended for an operator-controlled prototype. A public
// service should use AnthropicProvider so each request has normal API billing,
// observability, and lifecycle semantics.
type ClaudeCLIConfig struct {
	Path           string
	APIKey         string
	BaseURL        string
	Model          string
	Timeout        time.Duration
	MaxTurns       int
	MaxBudgetUSD   float64
	TempRoot       string
	MaxOutputBytes int
}

type ClaudeCLIProvider struct {
	path           string
	apiKey         string
	baseURL        string
	model          string
	timeout        time.Duration
	maxTurns       int
	maxBudgetUSD   float64
	tempRoot       string
	maxOutputBytes int
}

func NewClaudeCLI(config ClaudeCLIConfig) (*ClaudeCLIProvider, error) {
	config.Path = strings.TrimSpace(config.Path)
	if config.Path == "" {
		config.Path = "claude"
	}
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		config.APIKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("%w: Anthropic API key is required in CLI bare mode", ErrConfiguration)
	}
	if config.BaseURL != "" {
		validated, err := validateBaseURL(config.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("%w: CLI base URL: %v", ErrConfiguration, err)
		}
		config.BaseURL = validated
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = defaultModel
	}
	if !validModelName(config.Model) {
		return nil, fmt.Errorf("%w: invalid Claude CLI model name", ErrConfiguration)
	}
	if config.Timeout == 0 {
		config.Timeout = 45 * time.Second
	}
	if config.Timeout < time.Second {
		return nil, fmt.Errorf("%w: timeout must be at least one second", ErrConfiguration)
	}
	if config.MaxTurns == 0 {
		config.MaxTurns = 2
	}
	if config.MaxTurns < 1 || config.MaxTurns > 4 {
		return nil, fmt.Errorf("%w: CLI max turns must be between 1 and 4", ErrConfiguration)
	}
	if config.MaxBudgetUSD == 0 {
		config.MaxBudgetUSD = 0.05
	}
	if config.MaxBudgetUSD < 0.001 || config.MaxBudgetUSD > 5 {
		return nil, fmt.Errorf("%w: CLI budget must be between 0.001 and 5 USD", ErrConfiguration)
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultMaxCLIOutput
	}
	if config.MaxOutputBytes < 1024 || config.MaxOutputBytes > defaultMaxCLIOutput {
		return nil, fmt.Errorf("%w: invalid CLI output limit", ErrConfiguration)
	}

	return &ClaudeCLIProvider{
		path:           config.Path,
		apiKey:         config.APIKey,
		baseURL:        config.BaseURL,
		model:          config.Model,
		timeout:        config.Timeout,
		maxTurns:       config.MaxTurns,
		maxBudgetUSD:   config.MaxBudgetUSD,
		tempRoot:       config.TempRoot,
		maxOutputBytes: config.MaxOutputBytes,
	}, nil
}

func NewClaudeCLIProvider(config ClaudeCLIConfig) (*ClaudeCLIProvider, error) {
	return NewClaudeCLI(config)
}

func (provider *ClaudeCLIProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	if provider == nil {
		return domain.GenerationResult{}, fmt.Errorf("%w: nil Claude CLI provider", ErrConfiguration)
	}

	temporaryDirectory, err := os.MkdirTemp(provider.tempRoot, "witty-reply-claude-")
	if err != nil {
		return domain.GenerationResult{}, &ProviderError{Provider: providerClaudeCLI, Code: "temp_dir", Err: err}
	}
	defer os.RemoveAll(temporaryDirectory)
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		return domain.GenerationResult{}, &ProviderError{Provider: providerClaudeCLI, Code: "temp_permissions", Err: err}
	}

	imageName := ""
	if request.Input.Kind == domain.InputImage {
		normalized, normalizeErr := normalizeRequest(request)
		if normalizeErr != nil {
			return domain.GenerationResult{}, normalizeErr
		}
		imageName = "input" + imageExtension(normalized.Input.MediaType)
		imagePath := filepath.Join(temporaryDirectory, imageName)
		if writeErr := os.WriteFile(imagePath, normalized.Input.Image, 0o600); writeErr != nil {
			return domain.GenerationResult{}, &ProviderError{Provider: providerClaudeCLI, Code: "write_image", Err: writeErr}
		}
	}

	normalized, prompt, err := prepareRequest(request, imageName)
	if err != nil {
		return domain.GenerationResult{}, err
	}
	args := provider.arguments(normalized.Input.Kind == domain.InputImage, imageName)

	commandCtx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, provider.path, args...)
	command.Dir = temporaryDirectory
	command.Env = provider.environment(temporaryDirectory)
	command.Stdin = strings.NewReader(prompt)
	stdout := newBoundedBuffer(provider.maxOutputBytes)
	stderr := newBoundedBuffer(64 << 10)
	command.Stdout = stdout
	command.Stderr = stderr

	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return domain.GenerationResult{}, ctx.Err()
		}
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return domain.GenerationResult{}, &ProviderError{Provider: providerClaudeCLI, Code: "timeout", Retryable: true, Err: context.DeadlineExceeded}
		}
		code := "process_error"
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = "exit_" + strconv.Itoa(exitErr.ExitCode())
		}
		return domain.GenerationResult{}, &ProviderError{Provider: providerClaudeCLI, Code: code, Retryable: false, Err: errors.New("claude CLI did not complete successfully")}
	}
	if stdout.Exceeded() {
		return domain.GenerationResult{}, &ProviderError{Provider: providerClaudeCLI, Code: "output_limit", Err: ErrOutputTooLarge}
	}

	result, err := parseCLIResult(stdout.Bytes(), normalized.Tone, provider.model, normalized.Mode)
	if err != nil {
		return domain.GenerationResult{}, newInvalidOutputError(providerClaudeCLI, "", err)
	}
	if err := validateFreshReplies(result.Replies, normalized.PreviousReplies); err != nil {
		return domain.GenerationResult{}, newInvalidOutputError(providerClaudeCLI, "", err)
	}
	return result, nil
}

func (provider *ClaudeCLIProvider) arguments(withImage bool, imageName string) []string {
	args := []string{
		"--bare",
		"--print",
		"--model", provider.model,
		"--permission-mode", "dontAsk",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--output-format", "json",
		"--json-schema", generationSchema,
		"--append-system-prompt", systemPrompt,
		"--max-turns", strconv.Itoa(provider.maxTurns),
		"--max-budget-usd", strconv.FormatFloat(provider.maxBudgetUSD, 'f', -1, 64),
	}
	if withImage {
		args = append(args,
			"--tools", "Read",
			"--allowedTools", "Read(./"+imageName+")",
		)
	} else {
		args = append(args, "--tools", "")
	}
	return args
}

func (provider *ClaudeCLIProvider) environment(temporaryDirectory string) []string {
	allowed := map[string]bool{
		"PATH": true, "TMPDIR": true, "LANG": true, "LC_ALL": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true,
	}
	environment := make([]string, 0, len(allowed)+8)
	for _, pair := range os.Environ() {
		key, _, ok := strings.Cut(pair, "=")
		if ok && allowed[key] {
			environment = append(environment, pair)
		}
	}
	environment = append(environment,
		"HOME="+temporaryDirectory,
		"XDG_CONFIG_HOME="+temporaryDirectory,
		"XDG_CACHE_HOME="+temporaryDirectory,
		"CLAUDE_CONFIG_DIR="+temporaryDirectory,
		"CLAUDE_CODE_SKIP_PROMPT_HISTORY=1",
		"ANTHROPIC_API_KEY="+provider.apiKey,
	)
	if provider.baseURL != "" {
		environment = append(environment, "ANTHROPIC_BASE_URL="+provider.baseURL)
	}
	return environment
}

func parseCLIResult(raw []byte, requestedTone domain.Tone, fallbackModel string, requestedModes ...domain.ScenarioMode) (domain.GenerationResult, error) {
	if len(raw) == 0 {
		return domain.GenerationResult{}, fmt.Errorf("%w: empty CLI output", ErrInvalidResponse)
	}
	if len(raw) > defaultMaxCLIOutput {
		return domain.GenerationResult{}, ErrOutputTooLarge
	}
	var envelope cliEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&envelope); err != nil {
		return domain.GenerationResult{}, fmt.Errorf("%w: decode CLI envelope: %v", ErrInvalidResponse, err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return domain.GenerationResult{}, fmt.Errorf("%w: trailing CLI JSON value", ErrInvalidResponse)
	} else if !errors.Is(err, io.EOF) {
		return domain.GenerationResult{}, fmt.Errorf("%w: malformed trailing CLI data", ErrInvalidResponse)
	}
	if envelope.IsError || envelope.Subtype != "success" {
		if strings.Contains(strings.ToLower(envelope.Result), "refus") {
			return domain.GenerationResult{}, ErrRefused
		}
		return domain.GenerationResult{}, fmt.Errorf("%w: CLI result subtype %q", ErrInvalidResponse, envelope.Subtype)
	}
	if len(envelope.PermissionDenials) > 0 {
		return domain.GenerationResult{}, fmt.Errorf("%w: CLI reported permission denials", ErrInvalidResponse)
	}
	if len(envelope.StructuredOutput) == 0 || bytes.Equal(bytes.TrimSpace(envelope.StructuredOutput), []byte("null")) {
		return domain.GenerationResult{}, fmt.Errorf("%w: CLI omitted structured_output", ErrInvalidResponse)
	}
	requestedMode := domain.ScenarioAuto
	if len(requestedModes) > 0 {
		requestedMode = requestedModes[0]
	}
	result, err := decodeGenerationResult(envelope.StructuredOutput, requestedTone, requestedMode)
	if err != nil {
		return domain.GenerationResult{}, err
	}
	result.Provider = providerClaudeCLI
	result.Model = fallbackModel
	if envelope.Usage.InputTokens < 0 || envelope.Usage.OutputTokens < 0 || envelope.Usage.CacheCreationInputTokens < 0 || envelope.Usage.CacheReadInputTokens < 0 {
		return domain.GenerationResult{}, fmt.Errorf("%w: negative CLI usage", ErrInvalidResponse)
	}
	result.Usage = domain.Usage{
		InputTokens:  envelope.Usage.InputTokens + envelope.Usage.CacheCreationInputTokens + envelope.Usage.CacheReadInputTokens,
		OutputTokens: envelope.Usage.OutputTokens,
	}

	if len(envelope.ModelUsage) > 0 {
		models := make([]string, 0, len(envelope.ModelUsage))
		result.Usage = domain.Usage{}
		for model, usage := range envelope.ModelUsage {
			if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheCreationInputTokens < 0 || usage.CacheReadInputTokens < 0 {
				return domain.GenerationResult{}, fmt.Errorf("%w: negative CLI model usage", ErrInvalidResponse)
			}
			models = append(models, model)
			result.Usage.InputTokens += usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
			result.Usage.OutputTokens += usage.OutputTokens
		}
		sort.Strings(models)
		if len(models) == 1 {
			result.Model = models[0]
		}
	}
	return result, nil
}

func imageExtension(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".image"
	}
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return originalLength, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.buffer.Write(data)
	return originalLength, nil
}

func (buffer *boundedBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

func (buffer *boundedBuffer) Exceeded() bool {
	return buffer.exceeded
}

type cliEnvelope struct {
	Subtype           string                   `json:"subtype"`
	IsError           bool                     `json:"is_error"`
	Result            string                   `json:"result"`
	StructuredOutput  json.RawMessage          `json:"structured_output"`
	Usage             cliUsage                 `json:"usage"`
	ModelUsage        map[string]cliModelUsage `json:"modelUsage"`
	PermissionDenials []json.RawMessage        `json:"permission_denials"`
}

type cliUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type cliModelUsage struct {
	InputTokens              int `json:"inputTokens"`
	OutputTokens             int `json:"outputTokens"`
	CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int `json:"cacheReadInputTokens"`
}
