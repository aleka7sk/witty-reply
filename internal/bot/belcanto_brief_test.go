package bot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
)

type briefCaptureProvider struct {
	mu             sync.Mutex
	threadRequests []ai.ThreadPostRequest
	ordinaryCalls  int
}

type refinementFailureProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *refinementFailureProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	return ai.NewFake().Generate(ctx, request)
}

func (p *refinementFailureProvider) GenerateThreadPost(_ context.Context, request ai.ThreadPostRequest) (ai.ThreadPostResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls > 1 {
		return ai.ThreadPostResult{}, errors.New("forced refinement failure")
	}
	return validTestThreadResultForRequest(
		request,
		"У каждого стола в караоке есть человек, который обещает не петь, а потом не отдаёт микрофон. Кто это у вас?",
		"refinement-test",
		"refinement-test",
	), nil
}

func (p *briefCaptureProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	p.mu.Lock()
	p.ordinaryCalls++
	p.mu.Unlock()
	return ai.NewFake().Generate(ctx, request)
}

func (p *briefCaptureProvider) GenerateThreadPost(_ context.Context, request ai.ThreadPostRequest) (ai.ThreadPostResult, error) {
	p.mu.Lock()
	p.threadRequests = append(p.threadRequests, request)
	p.mu.Unlock()
	return validTestThreadResultForRequest(
		request,
		"У каждого стола в караоке есть человек, который дольше всех говорит «я не буду», а потом не отдаёт микрофон. Кто это у вас?",
		"brief-test",
		"brief-test",
	), nil
}

func (p *briefCaptureProvider) snapshot() ([]ai.ThreadPostRequest, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ai.ThreadPostRequest(nil), p.threadRequests...), p.ordinaryCalls
}

func newBriefFlowService(t *testing.T) (*Service, *fakeTelegram, *store.Memory, *briefCaptureProvider) {
	t.Helper()
	provider := &briefCaptureProvider{}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	return service, telegramClient, memory, provider
}

func TestThreadsBriefCollectsGoalAndMaterialBeforeOneGeneration(t *testing.T) {
	service, telegramClient, memory, provider := newBriefFlowService(t)
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "full"
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, textUpdate(100, "/threads")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 || !strings.Contains(messages[0].Text, "выбери задачу") {
		t.Fatalf("objective prompt = %+v", messages)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 0 {
		t.Fatalf("AI ran before brief was complete: threads=%d ordinary=%d", len(requests), ordinary)
	}

	replies := threadButtonCallback(t, messages[0].ReplyMarkup, "💬 Ответы")
	if err := service.HandleUpdate(ctx, callbackUpdate(101, replies)); err != nil {
		t.Fatal(err)
	}
	messages = telegramClient.snapshotMessages()
	if len(messages) != 2 || !strings.Contains(messages[1].Text, "Материал дня") && !strings.Contains(messages[1].Text, "материал дня") {
		t.Fatalf("material prompt = %+v", messages)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 0 {
		t.Fatalf("AI ran after goal only: threads=%d ordinary=%d", len(requests), ordinary)
	}

	material := "Подтверждённый материал UNIQUE-BRIEF: ученик спросил, почему свой голос на записи кажется чужим."
	if err := service.HandleUpdate(ctx, textUpdate(102, material)); err != nil {
		t.Fatal(err)
	}
	requests, ordinary := provider.snapshot()
	if len(requests) != 1 || ordinary != 0 {
		t.Fatalf("generation calls: threads=%d ordinary=%d", len(requests), ordinary)
	}
	request := requests[0]
	if request.Objective != domain.ThreadObjectiveReplies || request.MaterialKind != domain.ThreadMaterialText || request.Material != material || request.Voice != domain.ThreadVoiceBelcanto {
		t.Fatalf("AI request = %+v", request)
	}
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	brief, err := memory.GetThreadBrief(ctx, draft.BriefID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Objective != domain.ThreadObjectiveReplies || draft.ScenarioID == "" || draft.GenerationUpdateID != 102 ||
		brief.State != domain.ThreadBriefDraftReady || brief.MaterialText != material || brief.MaterialKind != domain.ThreadMaterialText {
		t.Fatalf("draft=%+v brief=%+v", draft, brief)
	}
	messages = telegramClient.snapshotMessages()
	preview := messages[len(messages)-1].Text
	if !strings.Contains(preview, "Цель: содержательные ответы") ||
		!strings.Contains(preview, "Сценарий: ") || !strings.Contains(preview, "пяти разных сценариев") ||
		!strings.Contains(preview, "использован подтверждённый материал дня") {
		t.Fatalf("preview = %q", preview)
	}
	if strings.Contains(preview, "UNIQUE-BRIEF") || strings.Contains(logs.String(), "UNIQUE-BRIEF") {
		t.Fatalf("raw material leaked to preview/log: preview=%q log=%q", preview, logs.String())
	}
}

func TestThreadsTrialRequiresVerifiedMaterialAndHidesEvergreenShortcut(t *testing.T) {
	service, telegramClient, memory, provider := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(200, "/threads")); err != nil {
		t.Fatal(err)
	}
	trial := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "🎟 Пробное занятие")
	if err := service.HandleUpdate(ctx, callbackUpdate(201, trial)); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	materialPrompt := messages[len(messages)-1]
	if !strings.Contains(materialPrompt.Text, "точные подтверждённые условия") {
		t.Fatalf("trial prompt = %q", materialPrompt.Text)
	}
	for _, row := range materialPrompt.ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(button.Text, "Без материала") {
				t.Fatalf("trial exposed evergreen shortcut: %+v", button)
			}
		}
	}
	brief, err := memory.GetCurrentThreadBrief(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := service.callbacks.Encode(sessionPayloadForThreadBrief(brief.ID, brief.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, callbackUpdate(202, forged)); err != nil {
		t.Fatal(err)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 0 {
		t.Fatalf("trial without material reached AI: threads=%d ordinary=%d", len(requests), ordinary)
	}
	brief, err = memory.GetCurrentThreadBrief(ctx, 42)
	if err != nil || brief.State != domain.ThreadBriefAwaitingMaterial {
		t.Fatalf("trial brief = %+v, %v", brief, err)
	}
}

func TestThreadsEvergreenShortcutGeneratesOnlyAfterExplicitConfirmation(t *testing.T) {
	service, telegramClient, memory, provider := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(250, "/threads")); err != nil {
		t.Fatal(err)
	}
	reach := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "👀 Охват")
	if err := service.HandleUpdate(ctx, callbackUpdate(251, reach)); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	withoutMaterial := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, "✨ Без материала дня")
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 0 {
		t.Fatalf("AI ran before evergreen confirmation: threads=%d ordinary=%d", len(requests), ordinary)
	}
	if err := service.HandleUpdate(ctx, callbackUpdate(252, withoutMaterial)); err != nil {
		t.Fatal(err)
	}
	requests, ordinary := provider.snapshot()
	if len(requests) != 1 || ordinary != 0 || requests[0].Objective != domain.ThreadObjectiveReach ||
		requests[0].MaterialKind != domain.ThreadMaterialNone || requests[0].Material != "" {
		t.Fatalf("evergreen request=%+v ordinary=%d", requests, ordinary)
	}
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil || draft.Objective != domain.ThreadObjectiveReach || draft.BriefID <= 0 {
		t.Fatalf("evergreen draft=%+v err=%v", draft, err)
	}
}

func TestThreadsMaterialRejectsInvisibleFormatCharactersBeforeStoreTransition(t *testing.T) {
	service, telegramClient, memory, provider := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(275, "/threads")); err != nil {
		t.Fatal(err)
	}
	replies := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "💬 Ответы")
	if err := service.HandleUpdate(ctx, callbackUpdate(276, replies)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(277, "Материал с невидимым\u200b символом")); err != nil {
		t.Fatal(err)
	}
	brief, err := memory.GetCurrentThreadBrief(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if brief.State != domain.ThreadBriefAwaitingMaterial || brief.MaterialText != "" {
		t.Fatalf("invalid material changed brief: %+v", brief)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 0 {
		t.Fatalf("invalid material reached AI: threads=%d ordinary=%d", len(requests), ordinary)
	}
	messages := telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "Нужен один текстовый материал") {
		t.Fatalf("invalid material response = %q", messages[len(messages)-1].Text)
	}
}

func sessionPayloadForThreadBrief(briefID int64, revision uint32) session.CallbackPayload {
	return session.CallbackPayload{
		Action: session.ActionThreadMaterialNone, UserID: 42,
		InteractionID: briefID, Revision: revision, Candidate: -1,
	}
}

func TestThreadsMaterialRetryRedeliversCommittedDraftWithoutSecondAIRequest(t *testing.T) {
	service, telegramClient, _, provider := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(300, "/threads")); err != nil {
		t.Fatal(err)
	}
	replies := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "💬 Ответы")
	if err := service.HandleUpdate(ctx, callbackUpdate(301, replies)); err != nil {
		t.Fatal(err)
	}
	flaky := &failOnceThreadTelegram{base: telegramClient}
	service.telegram = flaky
	flaky.failMessage()
	materialUpdate := textUpdate(302, "Реальная фраза для повторной доставки превью.")
	if err := service.HandleUpdate(ctx, materialUpdate); err == nil {
		t.Fatal("expected first preview send to fail")
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 1 || ordinary != 0 {
		t.Fatalf("first attempt calls: threads=%d ordinary=%d", len(requests), ordinary)
	}
	if err := service.HandleUpdate(ctx, materialUpdate); err != nil {
		t.Fatal(err)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 1 || ordinary != 0 {
		t.Fatalf("replay repeated work: threads=%d ordinary=%d", len(requests), ordinary)
	}
	messages := telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "Belcanto Threads") {
		t.Fatalf("replayed preview = %+v", messages[len(messages)-1])
	}
}

func TestThreadsCancelCommandCancelsPendingBriefWithoutGeneration(t *testing.T) {
	service, _, memory, provider := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(400, "/threads")); err != nil {
		t.Fatal(err)
	}
	brief, err := memory.GetCurrentThreadBrief(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(401, "/cancel")); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetCurrentThreadBrief(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current brief survived /cancel: %v", err)
	}
	cancelled, err := memory.GetThreadBrief(ctx, brief.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != domain.ThreadBriefCancelled || cancelled.Current {
		t.Fatalf("cancelled brief = %+v", cancelled)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 0 {
		t.Fatalf("/cancel reached AI: threads=%d ordinary=%d", len(requests), ordinary)
	}
}

func TestThreadsNewCommandAlsoClearsPendingBrief(t *testing.T) {
	service, _, memory, provider := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(450, "/threads")); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(451, "/new")); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetCurrentThreadBrief(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current brief survived /new: %v", err)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 0 {
		t.Fatalf("/new reached AI: threads=%d ordinary=%d", len(requests), ordinary)
	}
	if err := service.HandleUpdate(ctx, textUpdate(452, "Это уже обычное сообщение, а не материал Belcanto.")); err != nil {
		t.Fatal(err)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 0 || ordinary != 1 {
		t.Fatalf("post-/new routing: threads=%d ordinary=%d", len(requests), ordinary)
	}
}

func TestThreadsDraftMetricsAreSegmentedByObjectiveAndScenario(t *testing.T) {
	service, telegramClient, _, _ := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(500, "/threads")); err != nil {
		t.Fatal(err)
	}
	replies := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "💬 Ответы")
	if err := service.HandleUpdate(ctx, callbackUpdate(501, replies)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(502, "Подтверждённый материал для метрики сценария.")); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	service.metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		"witty_reply_belcanto_drafts_ready_total 1",
		"witty_reply_belcanto_drafts_ready_objective_replies_total 1",
		"witty_reply_belcanto_drafts_ready_scenario_karaoke_archetype_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestThreadsFailedRefinementKeepsUsableCurrentDraftControls(t *testing.T) {
	provider := &refinementFailureProvider{}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	before := generateThreadDraftForTest(t, service, memory, 600)
	messages := telegramClient.snapshotMessages()
	warmer := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, "❤️ Теплее")
	if err := service.HandleUpdate(ctx, callbackUpdate(601, warmer)); err != nil {
		t.Fatal(err)
	}
	after, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.Revision != before.Revision || after.Text != before.Text {
		t.Fatalf("failed refinement changed current draft: before=%+v after=%+v", before, after)
	}
	messages = telegramClient.snapshotMessages()
	failure := messages[len(messages)-1]
	if !strings.Contains(failure.Text, "текущий черновик сохранён") {
		t.Fatalf("refinement failure text = %q", failure.Text)
	}
	_ = threadButtonCallback(t, failure.ReplyMarkup, "❤️ Теплее")
}
