package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/telegram"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
)

func TestThreadsCommandRejectsNonOperatorBeforeAIOrMeta(t *testing.T) {
	fake := ai.NewFake()
	provider := &countingThreadProvider{base: fake, threads: fake}
	publisher := &recordingThreadPublisher{}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.threadPublisher = publisher
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}

	if err := service.HandleUpdate(ctx, textUpdate(1, "/threads")); err != nil {
		t.Fatal(err)
	}
	if provider.threadCalls.Load() != 0 {
		t.Fatalf("unauthorized AI calls = %d", provider.threadCalls.Load())
	}
	createCalls, statusCalls, publishCalls, _ := publisher.snapshot()
	if createCalls != 0 || statusCalls != 0 || publishCalls != 0 {
		t.Fatalf("unauthorized Meta calls = create:%d status:%d publish:%d", createCalls, statusCalls, publishCalls)
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 || !strings.Contains(messages[0].Text, "только команде Belcanto") {
		t.Fatalf("unauthorized response = %+v", messages)
	}
}

func TestThreadsDisabledPublisherKeepsApprovedPreviewReady(t *testing.T) {
	service, telegramClient, memory, codec := newThreadService(
		t, "Готовый пост при отключённой публикации.", threadspub.NewDisabled(),
	)
	ctx := context.Background()
	generateThreadDraftForTest(t, service, memory, 10)
	messages := telegramClient.snapshotMessages()
	callbackData := threadPublishCallback(t, messages)
	payload, err := codec.DecodeForUser(callbackData, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(11, "disabled", callbackData)); err != nil {
		t.Fatal(err)
	}
	draft, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if draft.State != domain.ThreadDraftReady || draft.ClaimToken != "" || draft.ContainerID != "" {
		t.Fatalf("disabled publisher mutated draft = %+v", draft)
	}
	messages = telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "Threads пока не подключён") {
		t.Fatalf("disabled response = %q", messages[len(messages)-1].Text)
	}
}

func TestThreadsOldRevisionCannotReachMetaAfterRefinement(t *testing.T) {
	publisher := &recordingThreadPublisher{}
	service, telegramClient, memory, _ := newThreadService(t, "Первый готовый пост.", publisher)
	ctx := context.Background()
	generateThreadDraftForTest(t, service, memory, 20)
	messages := telegramClient.snapshotMessages()
	oldPublish := threadPublishCallback(t, messages)
	refine := threadButtonCallback(t, messages[0].ReplyMarkup, "🔄 Другой сценарий")
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(21, "refine", refine)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(22, "stale", oldPublish)); err != nil {
		t.Fatal(err)
	}
	createCalls, statusCalls, publishCalls, _ := publisher.snapshot()
	if createCalls != 0 || statusCalls != 0 || publishCalls != 0 {
		t.Fatalf("stale revision reached Meta = create:%d status:%d publish:%d", createCalls, statusCalls, publishCalls)
	}
	messages = telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "устарела") {
		t.Fatalf("stale response = %q", messages[len(messages)-1].Text)
	}
}

func threadButtonCallback(t *testing.T, keyboard *telegram.InlineKeyboardMarkup, label string) string {
	t.Helper()
	if keyboard != nil {
		for _, row := range keyboard.InlineKeyboard {
			for _, button := range row {
				if button.Text == label {
					return button.CallbackData
				}
			}
		}
	}
	t.Fatalf("Threads button %q not found", label)
	return ""
}
