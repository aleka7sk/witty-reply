package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
)

const (
	// Meta recommends polling an in-progress Threads container no more than
	// once per minute for up to five minutes. The pre-publish claim must outlive
	// that entire bounded wait; BeginThreadPublish renews it before the
	// irreversible call.
	threadDraftClaimLease       = 6 * time.Minute
	threadPublisherCallTimeout  = 20 * time.Second
	threadContainerReadyTimeout = 5 * time.Minute
	threadContainerPollInterval = time.Minute
	threadFinalizationTimeout   = 5 * time.Second
)

var errThreadPublishBusy = errors.New("threads draft publish is already in progress")

func (b *Service) handleBelcantoCommand(ctx context.Context, chatID int64, user domain.User, lang language) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		b.metrics.Inc("belcanto_access_denied")
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(lang), nil)
	}
	if !user.HasConsent() {
		keyboard, err := consentKeyboard(b.callbacks, user.TelegramID, lang)
		if err != nil {
			return err
		}
		return b.sendText(ctx, chatID, consentRequiredText(lang), keyboard)
	}
	return b.prepareThreadDraft(ctx, chatID, user, domain.ThreadVoiceBelcanto, "", nil)
}

func (b *Service) isBelcantoOperator(telegramID int64) bool {
	_, ok := b.belcantoOperators[telegramID]
	return ok
}

func (b *Service) prepareThreadDraft(
	ctx context.Context,
	chatID int64,
	user domain.User,
	voice domain.ThreadVoice,
	transform string,
	previous *domain.ThreadDraft,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	if b.threadGenerator == nil {
		return b.sendText(ctx, chatID, belcantoGeneratorUnavailableText(userLanguage(user.Language)), nil)
	}

	stopAction := b.startChatAction(ctx, chatID, telegram.ChatActionTyping)
	defer stopAction()
	recent, err := b.store.ListRecentThreadTexts(ctx, user.TelegramID, 20)
	if err != nil {
		return fmt.Errorf("list recent Threads drafts: %w", err)
	}
	request := ai.ThreadPostRequest{
		Voice: voice, Transform: transform, RecentTexts: recent,
		Language: "ru", Date: b.now(), Seed: uint32(b.now().UnixNano()),
	}
	revision := uint32(1)
	if previous != nil {
		request.PreviousText = previous.Text
		revision = previous.Revision + 1
	}
	providerCtx, cancel := context.WithTimeout(ctx, b.config.ProviderTimeout)
	result, err := b.threadGenerator.GenerateThreadPost(providerCtx, request)
	cancel()
	if err != nil {
		b.metrics.Inc("belcanto_generation_errors")
		b.logError("Belcanto post generation failed", user.TelegramID, "error", safeErrorCode(err))
		return b.sendText(ctx, chatID, belcantoGenerationErrorText(userLanguage(user.Language)), nil)
	}
	decision := b.threadSafety.FilterText(result.Text)
	if !decision.Allowed || strings.TrimSpace(decision.Text) == "" {
		b.metrics.Inc("belcanto_safety_rejections")
		return b.sendText(ctx, chatID, belcantoGenerationErrorText(userLanguage(user.Language)), nil)
	}
	draft := domain.ThreadDraft{
		TelegramID: user.TelegramID, Voice: voice, Goal: result.Goal, Text: decision.Text,
		Provider: result.Provider, Model: result.Model, Revision: revision,
		State: domain.ThreadDraftReady, Current: true,
	}
	draftID, err := b.store.CreateThreadDraft(ctx, draft)
	if err != nil {
		return fmt.Errorf("save Threads draft: %w", err)
	}
	draft.ID = draftID
	keyboard, err := threadDraftKeyboard(b.callbacks, user.TelegramID, draft)
	if err != nil {
		return err
	}
	b.metrics.Inc("belcanto_drafts_ready")
	b.metrics.Add("input_tokens", int64(result.Usage.InputTokens))
	b.metrics.Add("output_tokens", int64(result.Usage.OutputTokens))
	return b.sendText(ctx, chatID, threadDraftText(draft), keyboard)
}

func (b *Service) refineThreadDraft(
	ctx context.Context,
	chatID int64,
	user domain.User,
	payload session.CallbackPayload,
	voice domain.ThreadVoice,
	transform string,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	draft, err := b.store.GetThreadDraft(ctx, payload.InteractionID, user.TelegramID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadDraftState) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	if !draft.Current || draft.Revision != payload.Revision || (draft.State != domain.ThreadDraftReady && draft.State != domain.ThreadDraftFailed) {
		return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
	}
	if voice == "" {
		voice = draft.Voice
	}
	return b.prepareThreadDraft(ctx, chatID, user, voice, transform, &draft)
}

func (b *Service) publishThreadDraft(
	ctx context.Context,
	chatID int64,
	user domain.User,
	payload session.CallbackPayload,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	if b.threadPublisher == nil || !b.threadPublisher.Enabled() {
		return b.sendText(ctx, chatID, threadsNotConnectedText(), nil)
	}

	claimToken, err := newThreadClaimToken()
	if err != nil {
		return fmt.Errorf("create Threads publish claim: %w", err)
	}
	draft, claimed, err := b.store.ClaimThreadDraft(
		ctx, payload.InteractionID, user.TelegramID, payload.Revision,
		claimToken, time.Now().UTC(), threadDraftClaimLease,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadDraftState) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	if !claimed {
		if draft.State == domain.ThreadDraftPublishing {
			// Returning an error keeps the durable Telegram update retryable. A
			// later attempt will either observe the first worker's terminal state
			// or recover its expired lease; it must never claim success early.
			return fmt.Errorf("publish Threads draft %d: %w", draft.ID, errThreadPublishBusy)
		}
		if draft.State == domain.ThreadDraftUnknown {
			return b.reconcileUnknownThreadDraft(ctx, chatID, user, draft)
		}
		return b.sendText(ctx, chatID, threadDraftStateText(draft), nil)
	}

	containerID := draft.ContainerID
	if containerID == "" {
		if err := validateUpdateLease(ctx); err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, threadPublisherCallTimeout)
		containerID, err = b.threadPublisher.CreateText(callCtx, draft.Text, "")
		cancel()
		if err != nil {
			if finalizeErr := b.failThreadDraft(draft.ID, user.TelegramID, claimToken, threadspub.SafeCode(err), false); finalizeErr != nil {
				return fmt.Errorf("finalize Threads container failure: %w", finalizeErr)
			}
			b.logError("Threads container creation failed", user.TelegramID, "error", threadspub.SafeCode(err))
			return b.sendText(ctx, chatID, threadPublishFailedText(), nil)
		}
		if err := b.store.SetThreadContainer(ctx, draft.ID, user.TelegramID, claimToken, containerID); err != nil {
			return fmt.Errorf("save Threads container: %w", err)
		}
	}

	status, err := b.waitForThreadContainer(ctx, containerID)
	if err != nil {
		if finalizeErr := b.failThreadDraft(draft.ID, user.TelegramID, claimToken, threadspub.SafeCode(err), false); finalizeErr != nil {
			return fmt.Errorf("finalize Threads status failure: %w", finalizeErr)
		}
		b.logError("Threads container status failed", user.TelegramID, "error", threadspub.SafeCode(err))
		return b.sendText(ctx, chatID, threadPublishFailedText(), nil)
	}
	if status.State == threadspub.StateError || status.State == threadspub.StateExpired {
		code := "container_" + strings.ToLower(string(status.State))
		if finalizeErr := b.failThreadDraft(draft.ID, user.TelegramID, claimToken, code, false); finalizeErr != nil {
			return fmt.Errorf("finalize unusable Threads container: %w", finalizeErr)
		}
		return b.sendText(ctx, chatID, threadPublishFailedText(), nil)
	}
	if err := validateUpdateLease(ctx); err != nil {
		return err
	}
	if err := b.store.BeginThreadPublish(
		ctx, draft.ID, user.TelegramID, claimToken, time.Now().UTC(), threadDraftClaimLease,
	); err != nil {
		return fmt.Errorf("mark Threads publish attempt: %w", err)
	}
	if status.State == threadspub.StatePublished {
		if err := b.store.ConfirmThreadDraftPublished(ctx, draft.ID, user.TelegramID, containerID); err != nil {
			return fmt.Errorf("reconcile already published Threads container: %w", err)
		}
		b.metrics.Inc("belcanto_posts_published")
		return b.sendText(ctx, chatID, threadPublishedText(threadspub.Publication{}), nil)
	}

	callCtx, cancel := context.WithTimeout(ctx, threadPublisherCallTimeout)
	publication, err := b.threadPublisher.Publish(callCtx, containerID)
	cancel()
	if err != nil {
		unknown := threadPublishFailureIsAmbiguous(err)
		if unknown {
			if reconciled, reconcileErr := b.reconcilePublishedContainer(ctx, draft, containerID); reconcileErr != nil {
				return reconcileErr
			} else if reconciled {
				b.metrics.Inc("belcanto_posts_published")
				return b.sendText(ctx, chatID, threadPublishedText(threadspub.Publication{}), nil)
			}
		}
		if finalizeErr := b.failThreadDraft(draft.ID, user.TelegramID, claimToken, threadspub.SafeCode(err), unknown); finalizeErr != nil {
			return fmt.Errorf("finalize Threads publication failure: %w", finalizeErr)
		}
		b.logError("Threads publication failed", user.TelegramID, "error", threadspub.SafeCode(err))
		if unknown {
			return b.sendText(ctx, chatID, threadPublishUnknownText(), nil)
		}
		return b.sendText(ctx, chatID, threadPublishFailedText(), nil)
	}
	if err := b.store.CompleteThreadDraft(ctx, draft.ID, user.TelegramID, claimToken, publication.ID, publication.Permalink); err != nil {
		return fmt.Errorf("complete Threads publication: %w", err)
	}
	b.metrics.Inc("belcanto_posts_published")
	return b.sendText(ctx, chatID, threadPublishedText(publication), nil)
}

func threadPublishFailureIsAmbiguous(err error) bool {
	// Once BeginThreadPublish is durable, an unclassified error is uncertain:
	// only a publisher error explicitly marked Definite proves that retrying the
	// same container cannot duplicate public content.
	var publishErr *threadspub.Error
	if errors.As(err, &publishErr) {
		return publishErr.Class != threadspub.Definite
	}
	return true
}

func (b *Service) failThreadDraft(draftID, telegramID int64, claimToken, code string, unknown bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), threadFinalizationTimeout)
	defer cancel()
	if err := b.store.FailThreadDraft(ctx, draftID, telegramID, claimToken, code, unknown); err != nil {
		b.metrics.Inc("belcanto_draft_finalization_errors")
		b.logError("Threads draft finalization failed", telegramID, "error", fmt.Sprintf("%T", err))
		return err
	}
	return nil
}

func newThreadClaimToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (b *Service) waitForThreadContainer(ctx context.Context, containerID string) (threadspub.Status, error) {
	statusCtx, cancel := context.WithTimeout(ctx, threadContainerReadyTimeout)
	defer cancel()
	for {
		if err := validateUpdateLease(statusCtx); err != nil {
			return threadspub.Status{}, err
		}
		status, err := b.threadPublisher.ContainerStatus(statusCtx, containerID)
		if err != nil {
			return threadspub.Status{}, err
		}
		switch status.State {
		case threadspub.StateFinished, threadspub.StatePublished, threadspub.StateError, threadspub.StateExpired:
			return status, nil
		case threadspub.StateInProgress:
		default:
			return threadspub.Status{}, errors.New("invalid Threads container state")
		}
		timer := time.NewTimer(threadContainerPollInterval)
		select {
		case <-statusCtx.Done():
			timer.Stop()
			return threadspub.Status{}, statusCtx.Err()
		case <-timer.C:
		}
	}
}

func (b *Service) reconcilePublishedContainer(ctx context.Context, draft domain.ThreadDraft, containerID string) (bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, threadPublisherCallTimeout)
	status, err := b.threadPublisher.ContainerStatus(callCtx, containerID)
	cancel()
	if err != nil || status.State != threadspub.StatePublished {
		return false, nil
	}
	if err := b.store.ConfirmThreadDraftPublished(ctx, draft.ID, draft.TelegramID, containerID); err != nil {
		return false, fmt.Errorf("confirm reconciled Threads publication: %w", err)
	}
	return true, nil
}

func (b *Service) reconcileUnknownThreadDraft(ctx context.Context, chatID int64, user domain.User, draft domain.ThreadDraft) error {
	if strings.TrimSpace(draft.ContainerID) != "" {
		reconciled, err := b.reconcilePublishedContainer(ctx, draft, draft.ContainerID)
		if err != nil {
			return err
		}
		if reconciled {
			b.metrics.Inc("belcanto_posts_published")
			return b.sendText(ctx, chatID, threadPublishedText(threadspub.Publication{}), nil)
		}
	}
	return b.sendText(ctx, chatID, threadPublishUnknownText(), nil)
}

func (b *Service) cancelThreadDraft(ctx context.Context, chatID int64, user domain.User, payload session.CallbackPayload) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	if err := b.store.CancelThreadDraft(ctx, payload.InteractionID, user.TelegramID, payload.Revision); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadDraftState) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	b.metrics.Inc("belcanto_drafts_cancelled")
	return b.sendText(ctx, chatID, threadDraftCancelledText(), nil)
}
