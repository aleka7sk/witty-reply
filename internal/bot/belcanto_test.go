package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
)

type threadTestProvider struct {
	text string
}

type countingThreadProvider struct {
	base        ai.Provider
	threads     ai.ThreadPostGenerator
	threadCalls atomic.Int64
}

func (p *countingThreadProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	return p.base.Generate(ctx, request)
}

func (p *countingThreadProvider) GenerateThreadPost(ctx context.Context, request ai.ThreadPostRequest) (ai.ThreadPostResult, error) {
	p.threadCalls.Add(1)
	return p.threads.GenerateThreadPost(ctx, request)
}

func (p *threadTestProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	return ai.NewFake().Generate(ctx, request)
}

func (p *threadTestProvider) GenerateThreadPost(context.Context, ai.ThreadPostRequest) (ai.ThreadPostResult, error) {
	return ai.ThreadPostResult{
		Goal: "discussion", Text: p.text, Provider: "test", Model: "thread-test",
	}, nil
}

type recordingThreadPublisher struct {
	mu sync.Mutex

	createCalls  int
	statusCalls  int
	publishCalls int
	createdText  string
	statuses     []threadspub.Status
	statusIndex  int
	publication  threadspub.Publication
	publishErr   error

	publishStarted chan struct{}
	releasePublish chan struct{}
	startedOnce    sync.Once
}

func (*recordingThreadPublisher) Enabled() bool { return true }

func (p *recordingThreadPublisher) CreateText(_ context.Context, text, _ string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createCalls++
	p.createdText = text
	return "container-test", nil
}

func (p *recordingThreadPublisher) ContainerStatus(_ context.Context, id string) (threadspub.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statusCalls++
	if p.statusIndex < len(p.statuses) {
		status := p.statuses[p.statusIndex]
		p.statusIndex++
		if status.ID == "" {
			status.ID = id
		}
		return status, nil
	}
	return threadspub.Status{ID: id, State: threadspub.StateFinished}, nil
}

func (p *recordingThreadPublisher) Publish(ctx context.Context, _ string) (threadspub.Publication, error) {
	p.mu.Lock()
	p.publishCalls++
	started := p.publishStarted
	release := p.releasePublish
	publication := p.publication
	err := p.publishErr
	p.mu.Unlock()
	if started != nil {
		p.startedOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return threadspub.Publication{}, ctx.Err()
		case <-release:
		}
	}
	if publication.ID == "" {
		publication.ID = "post-test"
	}
	return publication, err
}

func (p *recordingThreadPublisher) snapshot() (createCalls, statusCalls, publishCalls int, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.createCalls, p.statusCalls, p.publishCalls, p.createdText
}

func (p *recordingThreadPublisher) setStatuses(statuses ...threadspub.Status) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses = append([]threadspub.Status(nil), statuses...)
	p.statusIndex = 0
}

type failingThreadFinalizationStore struct {
	store.Store
	failFailure bool
}

func (s *failingThreadFinalizationStore) FailThreadDraft(
	context.Context,
	int64,
	int64,
	string,
	string,
	bool,
) error {
	if s.failFailure {
		return errors.New("test finalization unavailable")
	}
	return errors.New("unexpected finalization call")
}

func newThreadService(
	t *testing.T,
	text string,
	publisher threadspub.Publisher,
) (*Service, *fakeTelegram, *store.Memory, *session.CallbackCodec) {
	t.Helper()
	service, telegramClient, memory, _, codec := newTestService(t, &threadTestProvider{text: text})
	service.threadPublisher = publisher
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	return service, telegramClient, memory, codec
}

func authorizedThreadsService(
	t *testing.T,
	publisher threadspub.Publisher,
) (*Service, *fakeTelegram, *countingThreadProvider) {
	t.Helper()
	fake := ai.NewFake()
	provider := &countingThreadProvider{base: fake, threads: fake}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.threadPublisher = publisher
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	return service, telegramClient, provider
}

func threadPublishCallback(t *testing.T, messages []telegram.SendMessageParams) string {
	t.Helper()
	for index := len(messages) - 1; index >= 0; index-- {
		keyboard := messages[index].ReplyMarkup
		if keyboard == nil {
			continue
		}
		for _, row := range keyboard.InlineKeyboard {
			for _, button := range row {
				if button.Text == "✅ Опубликовать" {
					return button.CallbackData
				}
			}
		}
	}
	t.Fatal("publish callback not found")
	return ""
}

func threadCallbackUpdate(updateID int64, callbackID, data string) telegram.Update {
	user := testUser()
	return telegram.Update{UpdateID: updateID, CallbackQuery: &telegram.CallbackQuery{
		ID: callbackID, From: user,
		Message: &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: user.ID, Type: "private"}},
		Data:    data,
	}}
}

func callbackUpdate(updateID int64, data string) telegram.Update {
	return threadCallbackUpdate(updateID, "thread-callback", data)
}

func TestThreadsPublishesExactPreviewAndSequentialDoubleTapIsIdempotent(t *testing.T) {
	exact := "Когда голос перестаёт просить разрешения,\nкомната становится тише. 🎼 <>&"
	publisher := &recordingThreadPublisher{publication: threadspub.Publication{
		ID: "post-exact", Permalink: "https://www.threads.net/@belcanto/post/exact",
	}}
	service, telegramClient, memory, codec := newThreadService(t, exact, publisher)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(1, "/threads")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 {
		t.Fatalf("preview messages = %+v", messages)
	}
	callbackData := threadPublishCallback(t, messages)
	payload, err := codec.DecodeForUser(callbackData, 42)
	if err != nil {
		t.Fatal(err)
	}
	draftBefore, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil || !strings.Contains(messages[0].Text, draftBefore.Text) ||
		!strings.Contains(draftBefore.Text, "🎼 <>&") {
		t.Fatalf("stored preview = %+v, %v", draftBefore, err)
	}

	if err := service.HandleUpdate(ctx, threadCallbackUpdate(2, "publish-1", callbackData)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(3, "publish-2", callbackData)); err != nil {
		t.Fatal(err)
	}
	createCalls, _, publishCalls, createdText := publisher.snapshot()
	if createCalls != 1 || publishCalls != 1 || createdText != draftBefore.Text {
		t.Fatalf("publisher calls create=%d publish=%d text=%q", createCalls, publishCalls, createdText)
	}
	draftAfter, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil || draftAfter.State != domain.ThreadDraftPublished ||
		draftAfter.PublishStartedAt == nil || draftAfter.ClaimToken != "" {
		t.Fatalf("published draft = %+v, %v", draftAfter, err)
	}
}

func TestThreadContainerPollingUsesMetaSafeBounds(t *testing.T) {
	if threadContainerPollInterval < time.Minute {
		t.Fatalf("container poll interval = %s", threadContainerPollInterval)
	}
	if threadContainerReadyTimeout < 5*time.Minute {
		t.Fatalf("container ready timeout = %s", threadContainerReadyTimeout)
	}
	if threadDraftClaimLease <= threadContainerReadyTimeout {
		t.Fatalf("claim lease %s does not cover ready timeout %s", threadDraftClaimLease, threadContainerReadyTimeout)
	}
}

func TestThreadsConcurrentDoubleTapCannotCrossPublishMarker(t *testing.T) {
	publisher := &recordingThreadPublisher{
		publishStarted: make(chan struct{}),
		releasePublish: make(chan struct{}),
	}
	service, telegramClient, _, _ := newThreadService(t, "Пост для проверки двойного нажатия.", publisher)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(10, "/threads")); err != nil {
		t.Fatal(err)
	}
	callbackData := threadPublishCallback(t, telegramClient.snapshotMessages())
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- service.HandleUpdate(ctx, threadCallbackUpdate(11, "publish-first", callbackData))
	}()
	select {
	case <-publisher.publishStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first publish did not start")
	}
	secondErr := service.HandleUpdate(ctx, threadCallbackUpdate(12, "publish-second", callbackData))
	if !errors.Is(secondErr, errThreadPublishBusy) {
		t.Fatalf("concurrent callback error = %v", secondErr)
	}
	createCalls, _, publishCalls, _ := publisher.snapshot()
	if createCalls != 1 || publishCalls != 1 {
		t.Fatalf("concurrent calls create=%d publish=%d", createCalls, publishCalls)
	}
	close(publisher.releasePublish)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestThreadsAmbiguousPublishIsUnknownUntilStatusProvesPublished(t *testing.T) {
	publisher := &recordingThreadPublisher{
		statuses: []threadspub.Status{
			{State: threadspub.StateFinished},
			{State: threadspub.StateFinished},
		},
		publishErr: &threadspub.Error{
			Operation: "publish", Code: threadspub.CodeTransport, Class: threadspub.Ambiguous,
		},
	}
	service, telegramClient, memory, codec := newThreadService(t, "Пост с неоднозначным ответом Meta.", publisher)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(20, "/threads")); err != nil {
		t.Fatal(err)
	}
	callbackData := threadPublishCallback(t, telegramClient.snapshotMessages())
	payload, err := codec.DecodeForUser(callbackData, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(21, "ambiguous", callbackData)); err != nil {
		t.Fatal(err)
	}
	draft, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil || draft.State != domain.ThreadDraftUnknown || draft.PublishStartedAt == nil {
		t.Fatalf("ambiguous draft = %+v, %v", draft, err)
	}
	_, _, publishCalls, _ := publisher.snapshot()
	if publishCalls != 1 {
		t.Fatalf("publish calls = %d", publishCalls)
	}

	publisher.setStatuses(threadspub.Status{State: threadspub.StatePublished})
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(22, "reconcile", callbackData)); err != nil {
		t.Fatal(err)
	}
	draft, err = memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil || draft.State != domain.ThreadDraftPublished {
		t.Fatalf("reconciled draft = %+v, %v", draft, err)
	}
	_, _, publishCalls, _ = publisher.snapshot()
	if publishCalls != 1 {
		t.Fatalf("reconciliation repeated publish: %d", publishCalls)
	}
}

func TestThreadsFinalizationFailureDoesNotClaimUnknownWasSaved(t *testing.T) {
	publisher := &recordingThreadPublisher{
		statuses: []threadspub.Status{
			{State: threadspub.StateFinished},
			{State: threadspub.StateFinished},
		},
		publishErr: &threadspub.Error{
			Operation: "publish", Code: threadspub.CodeTransport, Class: threadspub.Ambiguous,
		},
	}
	service, telegramClient, memory, codec := newThreadService(t, "Пост с отказом БД при финализации.", publisher)
	service.store = &failingThreadFinalizationStore{Store: memory, failFailure: true}
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(30, "/threads")); err != nil {
		t.Fatal(err)
	}
	callbackData := threadPublishCallback(t, telegramClient.snapshotMessages())
	payload, err := codec.DecodeForUser(callbackData, 42)
	if err != nil {
		t.Fatal(err)
	}
	messagesBefore := len(telegramClient.snapshotMessages())
	err = service.HandleUpdate(ctx, threadCallbackUpdate(31, "finalization-fails", callbackData))
	if err == nil {
		t.Fatal("finalization failure was swallowed")
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != messagesBefore {
		t.Fatalf("bot reported a terminal state that was not saved: %+v", messages[messagesBefore:])
	}
	draft, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil || draft.State != domain.ThreadDraftPublishing || draft.PublishStartedAt == nil {
		t.Fatalf("unfinalized draft = %+v, %v", draft, err)
	}

	recovered, claimed, err := memory.ClaimThreadDraft(
		ctx, draft.ID, 42, draft.Revision, "recovery-worker",
		time.Now().UTC().Add(threadDraftClaimLease+time.Minute), time.Minute,
	)
	if err != nil || claimed || recovered.State != domain.ThreadDraftUnknown {
		t.Fatalf("lease recovery = %+v, %v, %v", recovered, claimed, err)
	}
	publisher.setStatuses(threadspub.Status{State: threadspub.StatePublished})
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(32, "recovered-status", callbackData)); err != nil {
		t.Fatal(err)
	}
	_, _, publishCalls, _ := publisher.snapshot()
	if publishCalls != 1 {
		t.Fatalf("recovery repeated publish: %d", publishCalls)
	}
}
