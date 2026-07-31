package bot

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
)

type consentGuardThreadsPublisher struct {
	calls atomic.Int64
}

func (publisher *consentGuardThreadsPublisher) Enabled() bool {
	publisher.calls.Add(1)
	return true
}

func (publisher *consentGuardThreadsPublisher) CreateText(context.Context, string, string) (string, error) {
	publisher.calls.Add(1)
	return "consent-guard-container", nil
}

func (publisher *consentGuardThreadsPublisher) ContainerStatus(context.Context, string) (threadspub.Status, error) {
	publisher.calls.Add(1)
	return threadspub.Status{State: threadspub.StateFinished}, nil
}

func (publisher *consentGuardThreadsPublisher) Publish(context.Context, string) (threadspub.Publication, error) {
	publisher.calls.Add(1)
	return threadspub.Publication{ID: "consent-guard-publication"}, nil
}

func TestAllThreadCallbacksStopBeforeAIAndMetaAfterConsentRevocation(t *testing.T) {
	actions := []struct {
		name   string
		action session.Action
	}{
		{name: "new_belcanto", action: session.ActionThreadNewBelcanto},
		{name: "new_alisher", action: session.ActionThreadNewAlisher},
		{name: "wittier", action: session.ActionThreadWittier},
		{name: "warmer", action: session.ActionThreadWarmer},
		{name: "shorter", action: session.ActionThreadShorter},
		{name: "different_angle", action: session.ActionThreadDifferentAngle},
		{name: "no_sell", action: session.ActionThreadNoSell},
		{name: "publish", action: session.ActionThreadPublish},
		{name: "cancel", action: session.ActionThreadCancel},
	}

	for _, test := range actions {
		t.Run(test.name, func(t *testing.T) {
			publisher := &consentGuardThreadsPublisher{}
			service, telegramClient, provider := authorizedThreadsService(t, publisher)
			ctx := context.Background()

			if err := service.HandleUpdate(ctx, textUpdate(1, "/threads")); err != nil {
				t.Fatal(err)
			}
			if provider.threadCalls.Load() != 1 {
				t.Fatalf("initial generation calls = %d", provider.threadCalls.Load())
			}
			messages := telegramClient.snapshotMessages()
			publishData := messages[0].ReplyMarkup.InlineKeyboard[0][0].CallbackData
			publishedPayload, err := service.callbacks.DecodeForUser(publishData, 42)
			if err != nil {
				t.Fatal(err)
			}
			before, err := service.store.GetThreadDraft(ctx, publishedPayload.InteractionID, 42)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.store.SetConsent(ctx, 42, false); err != nil {
				t.Fatal(err)
			}

			callbackData, err := service.callbacks.Encode(session.CallbackPayload{
				Action:        test.action,
				UserID:        42,
				InteractionID: before.ID,
				Revision:      before.Revision,
				Candidate:     -1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.HandleUpdate(ctx, callbackUpdate(2, callbackData)); err != nil {
				t.Fatal(err)
			}

			if got := provider.threadCalls.Load(); got != 1 {
				t.Fatalf("callback reached AI after consent revocation: calls = %d", got)
			}
			if got := publisher.calls.Load(); got != 0 {
				t.Fatalf("callback reached Meta publisher after consent revocation: calls = %d", got)
			}
			after, err := service.store.GetThreadDraft(ctx, before.ID, 42)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != before.State || after.Current != before.Current || after.Revision != before.Revision || after.Text != before.Text {
				t.Fatalf("consent guard mutated draft: before=%+v after=%+v", before, after)
			}
			messages = telegramClient.snapshotMessages()
			last := messages[len(messages)-1]
			if last.ReplyMarkup == nil || !strings.Contains(strings.ToLower(last.Text), "соглас") {
				t.Fatalf("consent guard response = %+v", last)
			}
		})
	}
}

func TestThreadCallbackWithoutPriorConsentStopsBeforeAIAndMeta(t *testing.T) {
	fake := ai.NewFake()
	provider := &countingThreadProvider{base: fake, threads: fake}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.belcantoOperators = map[int64]struct{}{42: {}}
	publisher := &consentGuardThreadsPublisher{}
	service.threadPublisher = publisher
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	callbackData, err := service.callbacks.Encode(session.CallbackPayload{
		Action: session.ActionThreadPublish, UserID: 42, InteractionID: 999, Revision: 1, Candidate: -1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.HandleUpdate(ctx, callbackUpdate(1, callbackData)); err != nil {
		t.Fatal(err)
	}
	if got := provider.threadCalls.Load(); got != 0 {
		t.Fatalf("callback reached AI without consent: calls = %d", got)
	}
	if got := publisher.calls.Load(); got != 0 {
		t.Fatalf("callback reached Meta publisher without consent: calls = %d", got)
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 || messages[0].ReplyMarkup == nil || !strings.Contains(strings.ToLower(messages[0].Text), "соглас") {
		t.Fatalf("consent guard response = %+v", messages)
	}
}

var _ threadspub.Publisher = (*consentGuardThreadsPublisher)(nil)
