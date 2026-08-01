package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
)

type briefCaptureProvider struct {
	mu                 sync.Mutex
	threadRequests     []ai.ThreadPostRequest
	ordinaryCalls      int
	mixedMaterialBasis bool
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
	mixedMaterialBasis := p.mixedMaterialBasis
	p.mu.Unlock()
	result := validTestThreadResultForRequest(
		request,
		"У каждого стола в караоке есть человек, который дольше всех говорит «я не буду», а потом не отдаёт микрофон. Кто это у вас?",
		"brief-test",
		"brief-test",
	)
	if mixedMaterialBasis && strings.TrimSpace(request.Material) != "" {
		result.Audit.Candidates[1].MaterialBasis = "none"
		result.Audit.Candidates[1].Evidence = ""
	}
	return result, nil
}

func (p *briefCaptureProvider) snapshot() ([]ai.ThreadPostRequest, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ai.ThreadPostRequest(nil), p.threadRequests...), p.ordinaryCalls
}

func (p *briefCaptureProvider) useMixedMaterialBasis() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mixedMaterialBasis = true
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
	set, err := memory.GetThreadFinalistSetByGenerationUpdate(ctx, 42, 102)
	if err != nil {
		t.Fatal(err)
	}
	if set.State != domain.ThreadFinalistSetReady || len(set.Candidates) != 5 || set.SelectedPosition != -1 {
		t.Fatalf("finalist set = %+v", set)
	}
	if _, err := memory.GetCurrentThreadDraft(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("publishable draft exists before operator choice: %v", err)
	}
	messages = telegramClient.snapshotMessages()
	portfolio := messages[len(messages)-1]
	if !strings.Contains(portfolio.Text, "пять вариантов") ||
		!strings.Contains(portfolio.Text, "Цель: содержательные ответы") ||
		!strings.Contains(portfolio.Text, "⭐ выбор редактора") {
		t.Fatalf("portfolio = %q", portfolio.Text)
	}
	chooseSecond := threadButtonCallback(t, portfolio.ReplyMarkup, "Выбрать 2")
	if err := service.HandleUpdate(ctx, callbackUpdate(103, chooseSecond)); err != nil {
		t.Fatal(err)
	}
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	set, err = memory.GetThreadFinalistSet(ctx, set.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	brief, err := memory.GetThreadBrief(ctx, draft.BriefID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if set.State != domain.ThreadFinalistSetSelected || set.SelectedPosition != 1 || set.SelectedDraftID != draft.ID ||
		set.SelectionUpdateID != 103 || draft.Text != set.Candidates[1].Text || draft.GenerationUpdateID != 102 ||
		brief.State != domain.ThreadBriefDraftReady || brief.MaterialText != material || brief.MaterialKind != domain.ThreadMaterialText {
		t.Fatalf("set=%+v draft=%+v brief=%+v", set, draft, brief)
	}
	messages = telegramClient.snapshotMessages()
	preview := messages[len(messages)-1].Text
	if !strings.Contains(preview, draft.Text) || !strings.Contains(preview, "использован подтверждённый материал дня") {
		t.Fatalf("selected preview = %q", preview)
	}
	if strings.Contains(portfolio.Text, "UNIQUE-BRIEF") || strings.Contains(preview, "UNIQUE-BRIEF") || strings.Contains(logs.String(), "UNIQUE-BRIEF") {
		t.Fatalf("raw material leaked: portfolio=%q preview=%q log=%q", portfolio.Text, preview, logs.String())
	}
	if err := service.HandleUpdate(ctx, callbackUpdate(103, chooseSecond)); err != nil {
		t.Fatal(err)
	}
	replayed, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != draft.ID || replayed.Revision != draft.Revision || replayed.Text != draft.Text {
		t.Fatalf("selection replay changed draft: before=%+v after=%+v", draft, replayed)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 1 || ordinary != 0 {
		t.Fatalf("selection replay repeated AI: threads=%d ordinary=%d", len(requests), ordinary)
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

func TestThreadsMaterialPortfolioMakesEvergreenOverrideExplicit(t *testing.T) {
	service, telegramClient, memory, provider := newBriefFlowService(t)
	provider.useMixedMaterialBasis()
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, textUpdate(700, "/threads")); err != nil {
		t.Fatal(err)
	}
	replies := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "💬 Ответы")
	if err := service.HandleUpdate(ctx, callbackUpdate(701, replies)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(702, "Педагог заметил: взрослый ученик впервые спокойно дослушал запись своего голоса до конца.")); err != nil {
		t.Fatal(err)
	}

	set, err := memory.GetThreadFinalistSetByGenerationUpdate(ctx, 42, 702)
	if err != nil {
		t.Fatal(err)
	}
	if set.Candidates[0].MaterialBasis != "material" || set.Candidates[1].MaterialBasis != "none" {
		t.Fatalf("mixed finalist provenance = %+v", set.Candidates)
	}
	portfolio := telegramClient.snapshotMessages()[len(telegramClient.snapshotMessages())-1]
	if !strings.Contains(portfolio.Text, "по материалу дня") ||
		!strings.Contains(portfolio.Text, "evergreen, материал не используется") {
		t.Fatalf("portfolio hid material provenance: %q", portfolio.Text)
	}

	chooseEvergreen := threadButtonCallback(t, portfolio.ReplyMarkup, "Выбрать 2")
	if err := service.HandleUpdate(ctx, callbackUpdate(703, chooseEvergreen)); err != nil {
		t.Fatal(err)
	}
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	preview := telegramClient.snapshotMessages()[len(telegramClient.snapshotMessages())-1].Text
	if draft.Text != set.Candidates[1].Text || !strings.Contains(preview, "без материала — только evergreen") ||
		strings.Contains(preview, "использован подтверждённый материал дня") {
		t.Fatalf("evergreen preview = %q, draft=%+v", preview, draft)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 1 || ordinary != 0 {
		t.Fatalf("evergreen selection repeated AI: threads=%d ordinary=%d", len(requests), ordinary)
	}
}

func TestThreadFinalistSelectionReplayNeverRestoresStaleControls(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		makeStale func(*testing.T, context.Context, *Service, *store.Memory, domain.User, domain.ThreadDraft)
		startID   int64
	}{
		{
			name:    "after refinement",
			startID: 800,
			makeStale: func(t *testing.T, ctx context.Context, service *Service, _ *store.Memory, user domain.User, draft domain.ThreadDraft) {
				t.Helper()
				if err := service.prepareThreadDraft(ctx, 900, 42, user, draft.Voice, "shorter", &draft, nil); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:    "after publication",
			startID: 1_000,
			makeStale: func(t *testing.T, ctx context.Context, _ *Service, memory *store.Memory, _ domain.User, draft domain.ThreadDraft) {
				t.Helper()
				now := time.Now().UTC()
				const claimToken = "stale-selection-replay"
				if _, claimed, err := memory.ClaimThreadDraft(ctx, draft.ID, 42, draft.Revision, claimToken, now, time.Minute); err != nil || !claimed {
					t.Fatalf("claim draft: claimed=%v err=%v", claimed, err)
				}
				if err := memory.SetThreadContainer(ctx, draft.ID, 42, claimToken, "container-stale-replay"); err != nil {
					t.Fatal(err)
				}
				if err := memory.BeginThreadPublish(ctx, draft.ID, 42, claimToken, now, time.Minute); err != nil {
					t.Fatal(err)
				}
				if err := memory.CompleteThreadDraft(ctx, draft.ID, 42, claimToken, "post-stale-replay", ""); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, telegramClient, memory, _ := newBriefFlowService(t)
			ctx := context.Background()
			draft := generateThreadDraftForTest(t, service, memory, testCase.startID)
			set, err := memory.GetThreadFinalistSet(ctx, draft.FinalistSetID, 42)
			if err != nil {
				t.Fatal(err)
			}
			user, err := memory.GetUser(ctx, 42)
			if err != nil {
				t.Fatal(err)
			}
			testCase.makeStale(t, ctx, service, memory, user, draft)

			before := len(telegramClient.snapshotMessages())
			if err := service.selectThreadFinalist(ctx, set.SelectionUpdateID, 42, user, session.CallbackPayload{
				Action: session.ActionThreadSelectFinalist, UserID: 42,
				InteractionID: set.ID, Revision: set.Revision - 1, Candidate: int8(set.SelectedPosition),
			}); err != nil {
				t.Fatal(err)
			}
			messages := telegramClient.snapshotMessages()
			if len(messages) != before+1 || messages[len(messages)-1].Text != threadDraftStaleText() ||
				messages[len(messages)-1].ReplyMarkup != nil {
				t.Fatalf("stale selection replay exposed controls: %+v", messages[len(messages)-1])
			}
		})
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
	set, err := memory.GetThreadFinalistSetByGenerationUpdate(ctx, 42, 252)
	if err != nil || set.Objective != domain.ThreadObjectiveReach || set.BriefID <= 0 || len(set.Candidates) != 5 {
		t.Fatalf("evergreen finalist set=%+v err=%v", set, err)
	}
	if _, err := memory.GetCurrentThreadDraft(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("evergreen draft existed before choice: %v", err)
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

func TestThreadsCancelCommandAlsoCancelsUnselectedFinalistPortfolio(t *testing.T) {
	service, telegramClient, memory, provider := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(420, "/threads")); err != nil {
		t.Fatal(err)
	}
	replies := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "💬 Ответы")
	if err := service.HandleUpdate(ctx, callbackUpdate(421, replies)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(422, "Подтверждённая сцена занятия для отменяемой подборки.")); err != nil {
		t.Fatal(err)
	}
	set, err := memory.GetThreadFinalistSetByGenerationUpdate(ctx, 42, 422)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(423, "/cancel")); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetCurrentThreadBrief(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current candidates-ready brief survived /cancel: %v", err)
	}
	set, err = memory.GetThreadFinalistSet(ctx, set.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if set.State != domain.ThreadFinalistSetCancelled || set.Current {
		t.Fatalf("cancelled finalist set = %+v", set)
	}
	if _, err := memory.GetCurrentThreadDraft(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft appeared while cancelling finalists: %v", err)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 1 || ordinary != 0 {
		t.Fatalf("portfolio cancel calls: threads=%d ordinary=%d", len(requests), ordinary)
	}
}

func TestThreadsStaleInitialFinalistCancelDoesNotClaimToCancelNewWorkflow(t *testing.T) {
	service, telegramClient, memory, _ := newBriefFlowService(t)
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(430, "/threads")); err != nil {
		t.Fatal(err)
	}
	replies := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "💬 Ответы")
	if err := service.HandleUpdate(ctx, callbackUpdate(431, replies)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(432, "Подтверждённая сцена занятия для отменяемой подборки.")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	staleCancel := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, "🗑 Отменить варианты")
	if err := service.HandleUpdate(ctx, callbackUpdate(433, staleCancel)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, textUpdate(434, "/threads")); err != nil {
		t.Fatal(err)
	}
	newBrief, err := memory.GetCurrentThreadBrief(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, callbackUpdate(435, staleCancel)); err != nil {
		t.Fatal(err)
	}
	current, err := memory.GetCurrentThreadBrief(ctx, 42)
	if err != nil || current.ID != newBrief.ID || !current.Current {
		t.Fatalf("new workflow changed by stale cancel: before=%+v after=%+v err=%v", newBrief, current, err)
	}
	messages = telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "устарела") {
		t.Fatalf("stale cancel response = %q", messages[len(messages)-1].Text)
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
	service, telegramClient, memory, _ := newBriefFlowService(t)
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
	set, err := memory.GetThreadFinalistSetByGenerationUpdate(ctx, 42, 502)
	if err != nil {
		t.Fatal(err)
	}
	position := -1
	for _, candidate := range set.Candidates {
		if candidate.Recommended {
			position = candidate.Position
			break
		}
	}
	messages := telegramClient.snapshotMessages()
	choose := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, fmt.Sprintf("⭐ Выбрать %d", position+1))
	if err := service.HandleUpdate(ctx, callbackUpdate(503, choose)); err != nil {
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

func TestThreadsRefinementPortfolioCancelRestoresExactBaseDraft(t *testing.T) {
	service, telegramClient, memory, provider := newBriefFlowService(t)
	ctx := context.Background()
	base := generateThreadDraftForTest(t, service, memory, 700)
	messages := telegramClient.snapshotMessages()
	shorter := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, "✂️ Короче")
	if err := service.HandleUpdate(ctx, callbackUpdate(701, shorter)); err != nil {
		t.Fatal(err)
	}
	set, err := memory.GetThreadFinalistSetByGenerationUpdate(ctx, 42, 701)
	if err != nil {
		t.Fatal(err)
	}
	if set.BaseDraftID != base.ID || set.SourceRevision != base.Revision ||
		set.TargetDraftRevision != base.Revision+1 || set.State != domain.ThreadFinalistSetReady {
		t.Fatalf("refinement set = %+v", set)
	}
	if _, err := memory.GetCurrentThreadDraft(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("base stayed current while portfolio awaits choice: %v", err)
	}
	messages = telegramClient.snapshotMessages()
	cancel := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, "🗑 Отменить варианты")
	if err := service.HandleUpdate(ctx, callbackUpdate(702, cancel)); err != nil {
		t.Fatal(err)
	}
	restored, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	set, err = memory.GetThreadFinalistSet(ctx, set.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != base.ID || restored.Revision != base.Revision || restored.Text != base.Text ||
		set.State != domain.ThreadFinalistSetCancelled || set.Current {
		t.Fatalf("restored=%+v base=%+v set=%+v", restored, base, set)
	}
	if requests, ordinary := provider.snapshot(); len(requests) != 2 || ordinary != 0 {
		t.Fatalf("refinement/cancel calls: threads=%d ordinary=%d", len(requests), ordinary)
	}
}

func TestThreadsCancelAndNewCommandsRestoreBaseFromRefinementPortfolio(t *testing.T) {
	for index, command := range []string{"/cancel", "/new"} {
		t.Run(command, func(t *testing.T) {
			service, telegramClient, memory, provider := newBriefFlowService(t)
			ctx := context.Background()
			startUpdateID := int64(750 + index*10)
			base := generateThreadDraftForTest(t, service, memory, startUpdateID)
			messages := telegramClient.snapshotMessages()
			shorter := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, "✂️ Короче")
			refinementUpdateID := startUpdateID + 1
			if err := service.HandleUpdate(ctx, callbackUpdate(refinementUpdateID, shorter)); err != nil {
				t.Fatal(err)
			}
			set, err := memory.GetCurrentThreadFinalistSet(ctx, 42)
			if err != nil || set.GenerationUpdateID != refinementUpdateID || set.BaseDraftID != base.ID {
				t.Fatalf("current refinement set = %+v, %v", set, err)
			}
			if err := service.HandleUpdate(ctx, textUpdate(startUpdateID+2, command)); err != nil {
				t.Fatal(err)
			}
			restored, err := memory.GetCurrentThreadDraft(ctx, 42)
			if err != nil {
				t.Fatal(err)
			}
			set, err = memory.GetThreadFinalistSet(ctx, set.ID, 42)
			if err != nil {
				t.Fatal(err)
			}
			if restored.ID != base.ID || restored.Revision != base.Revision || restored.Text != base.Text ||
				set.State != domain.ThreadFinalistSetCancelled || set.Current {
				t.Fatalf("command=%s restored=%+v base=%+v set=%+v", command, restored, base, set)
			}
			if _, err := memory.GetCurrentThreadFinalistSet(ctx, 42); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("command=%s left current finalist set: %v", command, err)
			}
			messages = telegramClient.snapshotMessages()
			if len(messages) < 2 || !strings.Contains(messages[len(messages)-2].Text, "Возвращаю предыдущий черновик") ||
				!strings.Contains(messages[len(messages)-1].Text, base.Text) {
				t.Fatalf("command=%s cancellation delivery = %+v", command, messages)
			}
			if requests, ordinary := provider.snapshot(); len(requests) != 2 || ordinary != 0 {
				t.Fatalf("command=%s calls: threads=%d ordinary=%d", command, len(requests), ordinary)
			}
		})
	}
}
