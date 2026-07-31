package bot

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/observability"
	"github.com/aleka7sk/witty-reply/internal/safety"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
	"github.com/aleka7sk/witty-reply/internal/transcribe"
)

type fakeTelegram struct {
	mu        sync.Mutex
	messages  []telegram.SendMessageParams
	photos    []telegram.SendPhotoParams
	callbacks []telegram.AnswerCallbackQueryParams
	files     map[string][]byte
}

func (f *fakeTelegram) SendMessage(_ context.Context, params telegram.SendMessageParams) (telegram.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, params)
	return telegram.Message{MessageID: int64(len(f.messages)), Chat: telegram.Chat{ID: params.ChatID, Type: "private"}, Text: params.Text}, nil
}

func (f *fakeTelegram) SendPhoto(_ context.Context, params telegram.SendPhotoParams) (telegram.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.photos = append(f.photos, params)
	return telegram.Message{MessageID: int64(len(f.photos)), Chat: telegram.Chat{ID: params.ChatID, Type: "private"}}, nil
}

func (f *fakeTelegram) SendChatAction(context.Context, telegram.SendChatActionParams) error {
	return nil
}

func (f *fakeTelegram) AnswerCallbackQuery(_ context.Context, params telegram.AnswerCallbackQueryParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callbacks = append(f.callbacks, params)
	return nil
}

func (f *fakeTelegram) DownloadFileLimit(_ context.Context, fileID string, limit int64) (telegram.DownloadedFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[fileID]
	if !ok {
		return telegram.DownloadedFile{}, errors.New("missing test file")
	}
	if int64(len(data)) > limit {
		return telegram.DownloadedFile{}, errors.New("test file too large")
	}
	return telegram.DownloadedFile{File: telegram.File{FileID: fileID, FileSize: int64(len(data))}, Data: append([]byte(nil), data...)}, nil
}

func (f *fakeTelegram) snapshotMessages() []telegram.SendMessageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telegram.SendMessageParams(nil), f.messages...)
}

type countingProvider struct {
	calls atomic.Int64
	next  ai.Provider
	fn    func(context.Context, domain.GenerationRequest) (domain.GenerationResult, error)
}

type recordingTranscriber struct {
	audio  transcribe.Audio
	result transcribe.Result
	err    error
}

type blockingTranscriber struct {
	started chan struct{}
	once    sync.Once
}

func (t *blockingTranscriber) Transcribe(ctx context.Context, _ transcribe.Audio) (transcribe.Result, error) {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return transcribe.Result{}, ctx.Err()
}

func (t *recordingTranscriber) Transcribe(_ context.Context, audio transcribe.Audio) (transcribe.Result, error) {
	t.audio = audio
	return t.result, t.err
}

func (p *countingProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	p.calls.Add(1)
	if p.fn != nil {
		return p.fn(ctx, request)
	}
	return p.next.Generate(ctx, request)
}

func newTestService(t *testing.T, provider ai.Provider) (*Service, *fakeTelegram, *store.Memory, *session.Cache[interaction], *session.CallbackCodec) {
	t.Helper()
	telegramClient := &fakeTelegram{files: make(map[string][]byte)}
	memory := store.NewMemory()
	sessions, err := session.New[interaction](time.Hour, session.WithoutJanitor())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sessions.Close)
	codec, err := session.NewCallbackCodec([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		telegramClient, provider, transcribe.NewDisabled(), memory, sessions, codec,
		safety.New(safety.Config{MaxRunes: 240, CandidateCount: 3}), nil,
		observability.NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config{
			ProviderTimeout: 2 * time.Second, UsageLocation: time.FixedZone("Asia/Almaty", 5*60*60), CallbackSecret: "0123456789abcdef",
			Limits: Limits{TextDaily: 10, MediaDaily: 3, MemeDaily: 1, RefinementDaily: 10, StyleExamples: 5, MaxTextRunes: 6000, MaxImageBytes: 10 << 20, MaxVoiceBytes: 20 << 20},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, telegramClient, memory, sessions, codec
}

func testUser() telegram.User {
	return telegram.User{ID: 42, FirstName: "Test", Username: "private-name", LanguageCode: "ru"}
}

func textUpdate(id int64, text string) telegram.Update {
	user := testUser()
	return telegram.Update{UpdateID: id, Message: &telegram.Message{
		MessageID: id, From: &user, Chat: telegram.Chat{ID: user.ID, Type: "private"}, Text: text,
	}}
}

func TestConsentGatesGeneration(t *testing.T) {
	provider := &countingProvider{next: ai.NewFake()}
	service, telegramClient, _, _, _ := newTestService(t, provider)
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, textUpdate(1, "Тебя никто не спрашивал")); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 0 {
		t.Fatal("provider called before consent")
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 || messages[0].ReplyMarkup == nil {
		t.Fatalf("consent message = %+v", messages)
	}
	consentData := messages[0].ReplyMarkup.InlineKeyboard[0][0].CallbackData
	user := testUser()
	callbackMessage := telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: user.ID, Type: "private"}}
	if err := service.HandleUpdate(ctx, telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{
		ID: "callback-1", From: user, Message: &callbackMessage, Data: consentData,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(3, "Тебя никто не спрашивал")); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls = %d", provider.calls.Load())
	}
	messages = telegramClient.snapshotMessages()
	result := messages[len(messages)-1]
	if result.ReplyMarkup == nil || len(result.ReplyMarkup.InlineKeyboard[0]) != 3 {
		t.Fatalf("result keyboard = %+v", result.ReplyMarkup)
	}
	for _, button := range result.ReplyMarkup.InlineKeyboard[0] {
		if button.CopyText == nil || button.CopyText.Text == "" {
			t.Fatalf("copy button = %+v", button)
		}
	}
}

func TestProviderFailureRefundsQuota(t *testing.T) {
	provider := &countingProvider{fn: func(context.Context, domain.GenerationRequest) (domain.GenerationResult, error) {
		return domain.GenerationResult{}, errors.New("provider unavailable")
	}}
	service, _, memory, _, _ := newTestService(t, provider)
	ctx := context.Background()
	_, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(1, "message")); err != nil {
		t.Fatal(err)
	}
	stats, err := memory.Stats(ctx, 42, service.now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UsedToday != 0 {
		t.Fatalf("used after failed provider = %d", stats.UsedToday)
	}
}

func TestNewerInputMakesLateResultObsolete(t *testing.T) {
	firstStarted := make(chan struct{})
	provider := &countingProvider{fn: func(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
		if request.Input.Text == "first" {
			close(firstStarted)
			<-ctx.Done()
			return domain.GenerationResult{}, ctx.Err()
		}
		return ai.NewFake().Generate(ctx, request)
	}}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)

	done := make(chan error, 1)
	go func() { done <- service.HandleUpdate(ctx, textUpdate(1, "first")) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first generation did not start")
	}
	if err := service.HandleUpdate(ctx, textUpdate(2, "second")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 || !strings.Contains(messages[0].Text, "Какой вариант") {
		t.Fatalf("late result was delivered or second result missing: %+v", messages)
	}
}

func TestPersistedNewInputPreemptsVoiceBeforeAI(t *testing.T) {
	provider := &countingProvider{next: ai.NewFake()}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	transcriber := &blockingTranscriber{started: make(chan struct{})}
	service.transcriber = transcriber
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)
	telegramClient.files["voice"] = []byte("ogg-test")
	user := testUser()
	voice := telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 1, From: &user, Chat: telegram.Chat{ID: 42, Type: "private"},
		Voice: &telegram.Voice{FileID: "voice", Duration: 2, MIMEType: "audio/ogg", FileSize: 8},
	}}
	done := make(chan error, 1)
	go func() { done <- service.HandleUpdate(ctx, voice) }()
	select {
	case <-transcriber.started:
	case <-time.After(time.Second):
		t.Fatal("voice transcription did not start")
	}
	replacement := textUpdate(2, "second")
	service.PreemptUpdate(replacement)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("preempted voice processing did not stop")
	}
	if provider.calls.Load() != 0 {
		t.Fatal("preempted voice reached AI provider")
	}
	if err := service.HandleUpdate(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("replacement provider calls = %d", provider.calls.Load())
	}
}

func TestFeedbackStyleRefinementAndDeletionLifecycle(t *testing.T) {
	provider := &countingProvider{next: ai.NewFake()}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)
	user := testUser()
	callbackMessage := telegram.Message{MessageID: 100, Chat: telegram.Chat{ID: user.ID, Type: "private"}}

	if err := service.HandleUpdate(ctx, textUpdate(1, "Исходное сообщение")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	resultKeyboard := messages[len(messages)-1].ReplyMarkup
	if resultKeyboard == nil || len(resultKeyboard.InlineKeyboard) < 5 {
		t.Fatalf("unexpected result keyboard: %+v", resultKeyboard)
	}

	feedbackData := resultKeyboard.InlineKeyboard[1][0].CallbackData
	if err := service.HandleUpdate(ctx, telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{ID: "feedback", From: user, Message: &callbackMessage, Data: feedbackData}}); err != nil {
		t.Fatal(err)
	}

	styleData := resultKeyboard.InlineKeyboard[4][0].CallbackData
	if err := service.HandleUpdate(ctx, telegram.Update{UpdateID: 3, CallbackQuery: &telegram.CallbackQuery{ID: "style", From: user, Message: &callbackMessage, Data: styleData}}); err != nil {
		t.Fatal(err)
	}
	examples, err := memory.ListStyleExamples(ctx, 42, 10)
	if err != nil || len(examples) != 1 {
		t.Fatalf("style examples = %v, %v", examples, err)
	}

	funnierData := resultKeyboard.InlineKeyboard[2][0].CallbackData
	if err := service.HandleUpdate(ctx, telegram.Update{UpdateID: 4, CallbackQuery: &telegram.CallbackQuery{ID: "funnier", From: user, Message: &callbackMessage, Data: funnierData}}); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 2 {
		t.Fatalf("provider calls after refinement = %d", provider.calls.Load())
	}
	if err := service.HandleUpdate(ctx, telegram.Update{UpdateID: 5, CallbackQuery: &telegram.CallbackQuery{ID: "stale", From: user, Message: &callbackMessage, Data: funnierData}}); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 2 {
		t.Fatal("stale refinement reached provider")
	}

	if err := service.HandleUpdate(ctx, textUpdate(6, "/style")); err != nil {
		t.Fatal(err)
	}
	messages = telegramClient.snapshotMessages()
	styleKeyboard := messages[len(messages)-1].ReplyMarkup
	toneData := styleKeyboard.InlineKeyboard[0][0].CallbackData
	if err := service.HandleUpdate(ctx, telegram.Update{UpdateID: 7, CallbackQuery: &telegram.CallbackQuery{ID: "tone", From: user, Message: &callbackMessage, Data: toneData}}); err != nil {
		t.Fatal(err)
	}
	storedUser, err := memory.GetUser(ctx, 42)
	if err != nil || storedUser.DefaultTone != domain.ToneSmart {
		t.Fatalf("stored user = %+v, %v", storedUser, err)
	}

	if err := service.HandleUpdate(ctx, textUpdate(8, "/delete_me")); err != nil {
		t.Fatal(err)
	}
	messages = telegramClient.snapshotMessages()
	deleteMarkup := messages[len(messages)-1].ReplyMarkup
	deleteData := deleteMarkup.InlineKeyboard[0][0].CallbackData
	if err := service.HandleUpdate(ctx, telegram.Update{UpdateID: 9, CallbackQuery: &telegram.CallbackQuery{ID: "delete", From: user, Message: &callbackMessage, Data: deleteData}}); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetUser(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user was not deleted: %v", err)
	}
}

func TestScreenshotIsNormalizedBeforeProvider(t *testing.T) {
	var received domain.GenerationRequest
	provider := &countingProvider{fn: func(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
		received = request
		return ai.NewFake().Generate(ctx, request)
	}}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)

	canvas := image.NewRGBA(image.Rect(0, 0, 20, 10))
	canvas.Set(1, 1, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	telegramClient.files["screenshot"] = encoded.Bytes()
	user := testUser()
	update := telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 1, From: &user, Chat: telegram.Chat{ID: 42, Type: "private"}, Caption: "ответь на последнее сообщение",
		Photo: []telegram.PhotoSize{{FileID: "screenshot", Width: 20, Height: 10, FileSize: int64(encoded.Len())}},
	}}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}
	if received.Input.Kind != domain.InputImage || received.Input.MediaType != "image/jpeg" || len(received.Input.Image) == 0 {
		t.Fatalf("provider input = %+v", received.Input)
	}
	if bytes.Equal(received.Input.Image, encoded.Bytes()) {
		t.Fatal("source image was not normalized")
	}
}

func TestTextGenerationUsesSourceLanguageInsteadOfTelegramLocale(t *testing.T) {
	var received domain.GenerationRequest
	provider := &countingProvider{fn: func(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
		received = request
		return ai.NewFake().Generate(ctx, request)
	}}
	service, _, memory, _, _ := newTestService(t, provider)
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)

	if err := service.HandleUpdate(ctx, textUpdate(1, "That was quite a plot twist")); err != nil {
		t.Fatal(err)
	}
	if received.Language != "en" {
		t.Fatalf("generation language = %q, want source language en", received.Language)
	}
}

func TestVoiceUsesAutomaticTranscriptionLanguage(t *testing.T) {
	var received domain.GenerationRequest
	provider := &countingProvider{fn: func(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
		received = request
		return ai.NewFake().Generate(ctx, request)
	}}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	recorder := &recordingTranscriber{result: transcribe.Result{Text: "That is not what I said", Language: "en", Confidence: 0.99}}
	service.transcriber = recorder
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)
	telegramClient.files["voice-en"] = []byte("ogg-test")
	user := testUser()
	update := telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 1, From: &user, Chat: telegram.Chat{ID: 42, Type: "private"},
		Voice: &telegram.Voice{FileID: "voice-en", Duration: 2, MIMEType: "audio/ogg", FileSize: 8},
	}}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}
	if recorder.audio.Language != "" {
		t.Fatalf("STT language hint = %q, want automatic detection", recorder.audio.Language)
	}
	if received.Input.Text != recorder.result.Text || received.Language != "en" {
		t.Fatalf("generation request = %+v", received)
	}
}

func TestDisabledVoiceRefundsMediaQuota(t *testing.T) {
	provider := &countingProvider{next: ai.NewFake()}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)
	telegramClient.files["voice"] = []byte("ogg-test")
	user := testUser()
	update := telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 1, From: &user, Chat: telegram.Chat{ID: 42, Type: "private"},
		Voice: &telegram.Voice{FileID: "voice", Duration: 2, MIMEType: "audio/ogg", FileSize: 8},
	}}
	if err := service.HandleUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 0 {
		t.Fatal("provider called when transcription is disabled")
	}
	decision, err := memory.ConsumeQuota(ctx, 42, update.UpdateID, domain.QuotaMedia, 1, service.now())
	if err != nil || !decision.Allowed || decision.Used != 1 {
		t.Fatalf("quota was not refunded: %+v, %v", decision, err)
	}
}

func TestRetryAfterSuccessfulDeliveryDoesNotDoubleChargeQuota(t *testing.T) {
	provider := &countingProvider{next: ai.NewFake()}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	ctx := context.Background()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(ctx, 42, true)
	update := textUpdate(77, "Повтори доставку после неоднозначного завершения")

	// This models Telegram accepting the first reply while durable job
	// completion fails. The queue can then invoke Service with the same update.
	for attempt := 0; attempt < 2; attempt++ {
		if err := service.HandleUpdate(ctx, update); err != nil {
			t.Fatalf("HandleUpdate attempt %d: %v", attempt+1, err)
		}
	}
	stats, err := memory.Stats(ctx, 42, service.now(), service.config.Limits.TextDaily)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UsedToday != 1 {
		t.Fatalf("used quota = %d, want one reservation for both attempts", stats.UsedToday)
	}
	if provider.calls.Load() != 2 || len(telegramClient.snapshotMessages()) != 2 {
		t.Fatalf("retry was not exercised: provider=%d messages=%d", provider.calls.Load(), len(telegramClient.snapshotMessages()))
	}
}
