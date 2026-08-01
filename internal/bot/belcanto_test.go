package bot

import (
	"context"
	"errors"
	"net/http/httptest"
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

func (p *threadTestProvider) GenerateThreadPost(_ context.Context, request ai.ThreadPostRequest) (ai.ThreadPostResult, error) {
	return validTestThreadResultForRequest(request, p.text, "test", "thread-test"), nil
}

func validTestThreadResult(text, provider, model string) ai.ThreadPostResult {
	return validTestThreadResultForRequest(ai.ThreadPostRequest{Objective: domain.ThreadObjectiveReplies}, text, provider, model)
}

func validTestThreadResultForRequest(request ai.ThreadPostRequest, text, provider, model string) ai.ThreadPostResult {
	objective := request.Objective
	if !objective.Selectable() {
		objective = domain.ThreadObjectiveReplies
	}
	texts := []string{
		text,
		"Какую песню вы узнаете раньше, чем вспоминаете её название?",
		"Тихий голос тоже умеет держать внимание. Ему просто приходится выбирать слова и ноты точнее.",
		"Какой знакомый звук первым выдаёт начало любимой песни?",
		"Припев иногда помнит настроение точнее календаря.",
	}
	scenarios := []string{"karaoke_archetype", "song_memory", "astana_soundtrack", "audience_choice", "adult_beginner"}
	mechanisms := []string{"conversation_humor", "music_memory", "local_identity", "participation", "recognition"}
	materialBasis, evidence := "none", ""
	if strings.TrimSpace(request.Material) != "" {
		materialBasis = "material"
		evidence = request.Material
	}
	candidates := make([]ai.ThreadPostCandidateAudit, 0, len(texts))
	for index, candidateText := range texts {
		id := string(rune('A' + index))
		candidates = append(candidates, ai.ThreadPostCandidateAudit{
			Attempt: 1, SourceSlot: id, ReviewerID: id, Goal: string(objective), Objective: objective,
			ScenarioID: scenarios[index], Mechanism: mechanisms[index], MaterialBasis: materialBasis,
			Evidence: evidence, Text: candidateText,
			Eligible: true, Considered: true, Selected: index == 0,
		})
	}
	return ai.ThreadPostResult{
		Goal: string(objective), Objective: objective, ScenarioID: scenarios[0], Mechanism: mechanisms[0],
		MaterialBasis: materialBasis, Evidence: evidence, Text: text, Provider: provider, Model: model,
		Audit: ai.ThreadPostAudit{
			Objective: objective, ScenarioID: scenarios[0], Mechanism: mechanisms[0],
			ExplorationGoal: 12, GenerationCalls: 3, ConceptCalls: 1, WriterCalls: 1,
			GeneratorProvider: provider, GeneratorModel: model,
			SelectionMode: "test", DeliveredWinnerID: "A", Candidates: candidates,
		},
	}
}

// generateThreadDraftForTest enters the durable workflow below the Telegram
// goal/material prompts. Most legacy publication/media tests are concerned
// with the exact draft after generation; this helper keeps those assertions
// focused while dedicated tests exercise the public three-step UX.
func generateThreadDraftForTest(t *testing.T, service *Service, dataStore store.Store, startUpdateID int64) domain.ThreadDraft {
	t.Helper()
	user, brief, generationUpdateID := readyThreadBriefForTest(t, dataStore, startUpdateID)
	ctx := context.Background()
	if err := service.generateThreadBrief(ctx, generationUpdateID, 42, user, brief); err != nil {
		t.Fatal(err)
	}
	draft, err := dataStore.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func readyThreadBriefForTest(
	t *testing.T,
	dataStore store.Store,
	startUpdateID int64,
) (domain.User, domain.ThreadBrief, int64) {
	t.Helper()
	ctx := context.Background()
	user, err := dataStore.GetUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	brief, _, err := dataStore.StartThreadBrief(ctx, 42, startUpdateID, domain.ThreadVoiceBelcanto)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = dataStore.SetThreadBriefObjective(ctx, brief.ID, 42, brief.Revision, domain.ThreadObjectiveReplies)
	if err != nil {
		t.Fatal(err)
	}
	generationUpdateID := startUpdateID + 1_000_000
	brief, err = dataStore.SetThreadBriefMaterial(
		ctx, brief.ID, 42, brief.Revision, generationUpdateID, domain.ThreadMaterialNone, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	return user, brief, generationUpdateID
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
	provider := &countingThreadProvider{
		base: fake,
		threads: &threadTestProvider{
			text: "Какую песню вы узнаёте раньше, чем вспоминаете её название?",
		},
	}
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
	generateThreadDraftForTest(t, service, memory, 1)
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
	recorder := httptest.NewRecorder()
	service.metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		"witty_reply_belcanto_posts_published_total 1",
		"witty_reply_belcanto_posts_published_objective_replies_total 1",
		"witty_reply_belcanto_posts_published_scenario_karaoke_archetype_total 1",
	} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("published metrics missing %q:\n%s", want, recorder.Body.String())
		}
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
	service, telegramClient, memory, _ := newThreadService(t, "Пост для проверки двойного нажатия.", publisher)
	ctx := context.Background()
	generateThreadDraftForTest(t, service, memory, 10)
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
	generateThreadDraftForTest(t, service, memory, 20)
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
	generateThreadDraftForTest(t, service, memory, 30)
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
