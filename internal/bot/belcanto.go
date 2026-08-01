package bot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/media"
	"github.com/aleka7sk/witty-reply/internal/observability"
	"github.com/aleka7sk/witty-reply/internal/photos"
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

var (
	errThreadPublishBusy              = errors.New("threads draft publish is already in progress")
	errThreadMediaDeliveryUnavailable = errors.New("threads media delivery is unavailable")
)

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
		DeliveryCheck: b.threadPostDeliverySafe,
	}
	generationID, err := newThreadClaimToken()
	if err != nil {
		return fmt.Errorf("create Threads generation ID: %w", err)
	}
	request.GenerationID = generationID
	revision := uint32(1)
	if previous != nil {
		request.PreviousText = previous.Text
		revision = previous.Revision + 1
	}
	providerCtx, cancel := context.WithTimeout(ctx, b.config.ThreadProviderTimeout)
	result, err := b.threadGenerator.GenerateThreadPost(providerCtx, request)
	cancel()
	if err != nil {
		b.metrics.Inc("belcanto_generation_errors")
		b.logError("Belcanto post generation failed", user.TelegramID, "error", safeErrorCode(err))
		return b.sendText(ctx, chatID, belcantoGenerationErrorText(userLanguage(user.Language)), nil)
	}
	if result.FallbackReason != "" {
		b.metrics.Inc("belcanto_curated_fallbacks")
		b.logger.Warn(
			"Belcanto post used curated fallback",
			"user", observability.UserHash(b.config.CallbackSecret, user.TelegramID),
			"reason", result.FallbackReason,
		)
	}
	result, ok := b.selectLastMileThreadPost(result)
	if !ok {
		b.metrics.Inc("belcanto_safety_rejections")
		return b.sendText(ctx, chatID, belcantoGenerationErrorText(userLanguage(user.Language)), nil)
	}
	draft := domain.ThreadDraft{
		TelegramID: user.TelegramID, Voice: voice, Goal: result.Goal, Text: result.Text,
		Provider: result.Provider, Model: result.Model, Revision: revision,
		MediaMode: domain.ThreadMediaText, State: domain.ThreadDraftReady, Current: true,
	}
	var previewMedia *domain.ThreadMedia
	if previous != nil && transform != "different_angle" && voice == previous.Voice &&
		(previous.MediaMode == domain.ThreadMediaImage || previous.MediaMode == domain.ThreadMediaImagePending) {
		preserve := previous.MediaID == 0 && previous.MediaMode == domain.ThreadMediaImagePending
		if previous.MediaID > 0 {
			previousMedia, mediaErr := b.store.GetThreadMedia(ctx, previous.MediaID, user.TelegramID)
			if mediaErr != nil {
				return fmt.Errorf("load previous Threads media: %w", mediaErr)
			}
			// A photo explicitly uploaded by the operator is a human override and
			// survives small text refinements. A topic-specific Pexels suggestion
			// is reconsidered on every revision so it cannot drift from the text.
			preserve = previousMedia.EffectiveSourceKind() == domain.ThreadMediaSourceTelegram
			if preserve {
				previewMedia = &previousMedia
			}
		}
		if preserve {
			draft.MediaMode = previous.MediaMode
			draft.MediaID = previous.MediaID
		}
	}
	var sourcedMedia *domain.ThreadMedia
	if draft.MediaMode == domain.ThreadMediaText && result.Visual.Mode == "belcanto_photo" {
		draft.MediaMode = domain.ThreadMediaImagePending
	}
	if draft.MediaMode == domain.ThreadMediaText && result.Visual.Mode == "licensed_photo" && b.threadPhotoSource != nil {
		mediaValue, mediaErr := b.prepareLicensedThreadMedia(ctx, user.TelegramID, result.Visual)
		if mediaErr != nil {
			b.metrics.Inc("belcanto_photo_source_fallbacks")
			b.logger.Warn(
				"Belcanto licensed photo unavailable; using text-only post",
				"generation_id", generationID,
				"user", observability.UserHash(b.config.CallbackSecret, user.TelegramID),
				"error", safeErrorCode(mediaErr),
			)
		} else {
			draft.MediaMode = domain.ThreadMediaImage
			sourcedMedia = &mediaValue
			previewMedia = sourcedMedia
		}
	}
	var draftID int64
	if sourcedMedia != nil {
		draftID, err = b.store.CreateThreadDraftWithMedia(ctx, draft, *sourcedMedia)
	} else {
		draftID, err = b.store.CreateThreadDraft(ctx, draft)
	}
	if err != nil {
		return fmt.Errorf("save Threads draft: %w", err)
	}
	if sourcedMedia != nil {
		draft, err = b.store.GetThreadDraft(ctx, draftID, user.TelegramID)
		if err != nil {
			return fmt.Errorf("load sourced Threads draft: %w", err)
		}
	} else {
		draft.ID = draftID
	}
	keyboard, err := threadDraftKeyboard(b.callbacks, user.TelegramID, draft)
	if err != nil {
		return err
	}
	b.metrics.Inc("belcanto_drafts_ready")
	b.metrics.Add("input_tokens", int64(result.Usage.InputTokens))
	b.metrics.Add("output_tokens", int64(result.Usage.OutputTokens))
	previewErr := b.sendThreadDraftPreview(ctx, chatID, draft, keyboard)
	previewStatus := "sent_acknowledged"
	if previewErr != nil {
		previewStatus = "send_error_unknown"
	}
	b.logBelcantoEditorialAudit(user.TelegramID, draft, transform, result, previewMedia, previewStatus)
	return previewErr
}

func (b *Service) selectLastMileThreadPost(result ai.ThreadPostResult) (ai.ThreadPostResult, bool) {
	if !validThreadPostEditorialResult(result) {
		return ai.ThreadPostResult{}, false
	}
	if b.threadPostDeliverySafe(result.Text) {
		return result, true
	}

	indexes := make([]int, 0, len(result.Audit.Candidates))
	for index, candidate := range result.Audit.Candidates {
		if candidate.Eligible && candidate.Considered && !candidate.Selected {
			indexes = append(indexes, index)
		}
	}
	sort.SliceStable(indexes, func(left, right int) bool {
		first := result.Audit.Candidates[indexes[left]]
		second := result.Audit.Candidates[indexes[right]]
		if first.Review.Total > 0 || second.Review.Total > 0 {
			return belcantoAuditReviewBetter(first, second)
		}
		if first.Local.Score != second.Local.Score {
			return first.Local.Score > second.Local.Score
		}
		if belcantoAuditLengthDistance(first.Local.RuneCount) != belcantoAuditLengthDistance(second.Local.RuneCount) {
			return belcantoAuditLengthDistance(first.Local.RuneCount) < belcantoAuditLengthDistance(second.Local.RuneCount)
		}
		return first.ReviewerID < second.ReviewerID
	})
	for _, index := range indexes {
		candidate := &result.Audit.Candidates[index]
		if !b.threadPostDeliverySafe(candidate.Text) {
			continue
		}
		for other := range result.Audit.Candidates {
			result.Audit.Candidates[other].Selected = other == index
		}
		result.Goal = candidate.Goal
		result.Text = candidate.Text
		result.Visual = ai.ThreadPostVisualRecommendation{Mode: "text_only"}
		result.Audit.DeliveredWinnerID = candidate.ReviewerID
		result.Audit.SelectionMode = "last_mile_override"
		result.Audit.DecisionReason = "The reviewed winner failed final delivery moderation; selected the next highest safe finalist."
		return result, true
	}
	return ai.ThreadPostResult{}, false
}

func validThreadPostEditorialResult(result ai.ThreadPostResult) bool {
	const requiredFinalists = 5
	finalists := 0
	selected := 0
	selectedID := ""
	selectedGoal := ""
	selectedText := ""
	ids := make(map[string]struct{}, requiredFinalists)
	for _, candidate := range result.Audit.Candidates {
		if !candidate.Eligible || !candidate.Considered {
			if candidate.Selected {
				return false
			}
			continue
		}
		finalists++
		if strings.TrimSpace(candidate.ReviewerID) == "" || strings.TrimSpace(candidate.Goal) == "" || strings.TrimSpace(candidate.Text) == "" {
			return false
		}
		if _, duplicate := ids[candidate.ReviewerID]; duplicate {
			return false
		}
		ids[candidate.ReviewerID] = struct{}{}
		if candidate.Selected {
			selected++
			selectedID = candidate.ReviewerID
			selectedGoal = candidate.Goal
			selectedText = candidate.Text
		}
	}
	return finalists == requiredFinalists && selected == 1 &&
		result.Audit.DeliveredWinnerID == selectedID && result.Goal == selectedGoal && result.Text == selectedText
}

func (b *Service) threadPostDeliverySafe(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	decision := b.threadSafety.FilterText(text)
	// Formatting-only normalization (for example paragraph breaks) has no
	// reason code and is intentionally not applied. PII redaction, profanity
	// masking, truncation, controls, and hard moderation would change what the
	// reviewer scored, so those candidates are rejected instead.
	return decision.Allowed && len(decision.Reasons) == 0
}

func (b *Service) prepareLicensedThreadMedia(
	ctx context.Context,
	telegramID int64,
	recommendation ai.ThreadPostVisualRecommendation,
) (domain.ThreadMedia, error) {
	asset, err := b.threadPhotoSource.Find(ctx, recommendation.Query)
	if err != nil {
		return domain.ThreadMedia{}, err
	}
	if asset.Provider != "pexels" {
		return domain.ThreadMedia{}, photos.ErrInvalidResult
	}
	maxOutputBytes := min(b.config.Limits.MaxImageBytes, domain.MaxThreadMediaBytes)
	normalized, err := media.NormalizeImage(asset.Data, media.ImageConfig{
		MaxInputBytes: b.config.Limits.MaxImageBytes, MaxOutputBytes: maxOutputBytes, LongSide: 1_440,
	})
	if err != nil {
		return domain.ThreadMedia{}, err
	}
	digest := sha256.Sum256(normalized.Data)
	deliveryKey, err := newThreadClaimToken()
	if err != nil {
		return domain.ThreadMedia{}, err
	}
	mediaValue := domain.ThreadMedia{
		TelegramID: telegramID, SourceKind: domain.ThreadMediaSourcePexels,
		SourceAssetID: asset.AssetID, SourcePageURL: asset.PageURL,
		SourceAuthor: asset.Author, SourceAuthorURL: asset.AuthorURL, SourceQuery: asset.Query,
		Data: normalized.Data, MediaType: normalized.MediaType, Width: normalized.Width, Height: normalized.Height,
		Digest: hex.EncodeToString(digest[:]), DeliveryKey: deliveryKey,
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return domain.ThreadMedia{}, err
	}
	return mediaValue, nil
}

func (b *Service) sendThreadDraftPreview(
	ctx context.Context,
	chatID int64,
	draft domain.ThreadDraft,
	keyboard *telegram.InlineKeyboardMarkup,
) error {
	if draft.MediaMode != domain.ThreadMediaImage {
		return b.sendText(ctx, chatID, threadDraftText(draft), keyboard)
	}
	mediaValue, err := b.store.GetThreadMedia(ctx, draft.MediaID, draft.TelegramID)
	if err != nil {
		return fmt.Errorf("load Threads preview media: %w", err)
	}
	keyboard = threadDraftKeyboardWithMediaSource(keyboard, mediaValue)
	_, err = b.telegram.SendPhoto(ctx, telegram.SendPhotoParams{
		ChatID: chatID, Photo: telegram.FileUpload("belcanto-threads.jpg", mediaValue.Data),
		Caption: threadDraftTextWithMedia(draft, mediaValue), ReplyMarkup: keyboard, ProtectContent: true,
	})
	return err
}

func threadDraftKeyboardWithMediaSource(
	keyboard *telegram.InlineKeyboardMarkup,
	mediaValue domain.ThreadMedia,
) *telegram.InlineKeyboardMarkup {
	if keyboard == nil || mediaValue.EffectiveSourceKind() != domain.ThreadMediaSourcePexels || mediaValue.SourcePageURL == "" {
		return keyboard
	}
	rows := append([][]telegram.InlineKeyboardButton(nil), keyboard.InlineKeyboard...)
	rows = append(rows, []telegram.InlineKeyboardButton{{Text: "📷 Источник фото · Pexels", URL: mediaValue.SourcePageURL}})
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
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

func (b *Service) setThreadDraftMediaMode(
	ctx context.Context,
	chatID int64,
	user domain.User,
	payload session.CallbackPayload,
	mode domain.ThreadMediaMode,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	draft, err := b.store.SetThreadDraftMediaMode(
		ctx, payload.InteractionID, user.TelegramID, payload.Revision, mode,
	)
	if err != nil {
		if errors.Is(err, store.ErrThreadDraftState) && payload.Revision < ^uint32(0) {
			// The durable callback job may be retried after the database transition
			// committed but before Telegram accepted the new prompt/preview. Recover
			// that exact one-step transition instead of stranding the operator on a
			// stale keyboard that can no longer advance the workflow.
			current, lookupErr := b.store.GetThreadDraft(ctx, payload.InteractionID, user.TelegramID)
			if lookupErr == nil && current.Current && current.Revision == payload.Revision+1 &&
				current.MediaMode == mode &&
				(current.State == domain.ThreadDraftReady || current.State == domain.ThreadDraftFailed) {
				draft = current
				err = nil
			}
		}
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadDraftState) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		if err != nil {
			return err
		}
	}
	return b.deliverThreadDraftMediaMode(ctx, chatID, user.TelegramID, draft, mode)
}

func (b *Service) deliverThreadDraftMediaMode(
	ctx context.Context,
	chatID, telegramID int64,
	draft domain.ThreadDraft,
	mode domain.ThreadMediaMode,
) error {
	keyboard, err := threadDraftKeyboard(b.callbacks, telegramID, draft)
	if err != nil {
		return err
	}
	if mode == domain.ThreadMediaImagePending {
		b.metrics.Inc("belcanto_media_requested")
		return b.sendText(ctx, chatID, threadImagePromptText(draft.MediaID > 0), keyboard)
	}
	if mode == domain.ThreadMediaImage {
		b.metrics.Inc("belcanto_media_replacement_cancelled")
	} else {
		b.metrics.Inc("belcanto_text_format_selected")
	}
	return b.sendThreadDraftPreview(ctx, chatID, draft, keyboard)
}

func (b *Service) tryHandleThreadMediaUpload(
	ctx context.Context,
	updateID int64,
	message telegram.Message,
	user domain.User,
) (bool, error) {
	if !b.isBelcantoOperator(user.TelegramID) {
		return false, nil
	}
	raw, err := b.classifyInput(message)
	if err != nil || raw.kind != domain.InputImage {
		return false, nil
	}
	// A durable Telegram job may be retried after the media transaction committed
	// but before the preview was acknowledged. Replay that same attachment rather
	// than accidentally routing the photo into ordinary Witty Reply.
	existing, err := b.store.GetThreadDraftByMediaUpdate(ctx, user.TelegramID, updateID)
	if err == nil {
		if !existing.Current {
			return true, b.sendText(ctx, message.Chat.ID, threadDraftStaleText(), nil)
		}
		keyboard, keyboardErr := threadDraftKeyboard(b.callbacks, user.TelegramID, existing)
		if keyboardErr != nil {
			return true, keyboardErr
		}
		return true, b.sendThreadDraftPreview(ctx, message.Chat.ID, existing, keyboard)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return true, err
	}
	draft, err := b.store.GetCurrentThreadDraft(ctx, user.TelegramID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && draft.MediaMode != domain.ThreadMediaImagePending) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if strings.TrimSpace(message.MediaGroupID) != "" {
		return true, b.sendText(ctx, message.Chat.ID, threadImageAlbumText(), nil)
	}
	if raw.fileSize > int64(b.config.Limits.MaxImageBytes) {
		return true, b.sendText(ctx, message.Chat.ID, threadImageInvalidText(), nil)
	}
	download, err := b.telegram.DownloadFileLimit(ctx, raw.fileID, int64(b.config.Limits.MaxImageBytes))
	if err != nil {
		var tooLarge *telegram.FileTooLargeError
		if errors.As(err, &tooLarge) {
			b.metrics.Inc("belcanto_media_rejections")
			return true, b.sendText(ctx, message.Chat.ID, threadImageInvalidText(), nil)
		}
		// Transport, Bot API, and context failures are not evidence that the
		// operator's photo is invalid. Keep the durable update retryable.
		return true, fmt.Errorf("download Threads media: %w", err)
	}
	maxOutputBytes := min(b.config.Limits.MaxImageBytes, domain.MaxThreadMediaBytes)
	normalized, err := media.NormalizeImage(download.Data, media.ImageConfig{
		MaxInputBytes:  b.config.Limits.MaxImageBytes,
		MaxOutputBytes: maxOutputBytes,
		LongSide:       1_440,
	})
	if err != nil {
		b.metrics.Inc("belcanto_media_rejections")
		return true, b.sendText(ctx, message.Chat.ID, threadImageInvalidText(), nil)
	}
	digest := sha256.Sum256(normalized.Data)
	deliveryKey, err := newThreadClaimToken()
	if err != nil {
		return true, fmt.Errorf("create Threads media delivery key: %w", err)
	}
	mediaValue := domain.ThreadMedia{
		TelegramID: user.TelegramID, SourceKind: domain.ThreadMediaSourceTelegram, SourceUpdateID: updateID,
		Data: normalized.Data, MediaType: normalized.MediaType,
		Width: normalized.Width, Height: normalized.Height,
		Digest: hex.EncodeToString(digest[:]), DeliveryKey: deliveryKey,
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		b.metrics.Inc("belcanto_media_rejections")
		return true, b.sendText(ctx, message.Chat.ID, threadImageInvalidText(), nil)
	}
	draft, err = b.store.AttachThreadDraftMedia(
		ctx, draft.ID, user.TelegramID, draft.Revision, mediaValue,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadDraftState) {
			return true, b.sendText(ctx, message.Chat.ID, threadDraftStaleText(), nil)
		}
		// Database availability, transaction, and collision errors are retryable
		// infrastructure failures. Do not acknowledge a valid photo as rejected.
		return true, fmt.Errorf("attach Threads media: %w", err)
	}
	keyboard, err := threadDraftKeyboard(b.callbacks, user.TelegramID, draft)
	if err != nil {
		return true, err
	}
	b.metrics.Inc("belcanto_media_attached")
	return true, b.sendThreadDraftPreview(ctx, message.Chat.ID, draft, keyboard)
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
		containerID, err = b.createThreadContainer(callCtx, draft)
		cancel()
		if err != nil {
			code := threadspub.SafeCode(err)
			if errors.Is(err, errThreadMediaDeliveryUnavailable) {
				code = "media_delivery_unavailable"
			}
			if finalizeErr := b.failThreadDraft(draft.ID, user.TelegramID, claimToken, code, false); finalizeErr != nil {
				return fmt.Errorf("finalize Threads container failure: %w", finalizeErr)
			}
			b.logError("Threads container creation failed", user.TelegramID, "error", code)
			if errors.Is(err, errThreadMediaDeliveryUnavailable) {
				return b.sendText(ctx, chatID, threadImageDeliveryUnavailableText(), nil)
			}
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

func (b *Service) createThreadContainer(ctx context.Context, draft domain.ThreadDraft) (string, error) {
	if draft.MediaMode != domain.ThreadMediaImage {
		return b.threadPublisher.CreateText(ctx, draft.Text, "")
	}
	imagePublisher, ok := b.threadPublisher.(threadspub.ImagePublisher)
	if !ok || b.threadMediaURL == nil {
		return "", errThreadMediaDeliveryUnavailable
	}
	mediaValue, err := b.store.GetThreadMedia(ctx, draft.MediaID, draft.TelegramID)
	if err != nil {
		return "", fmt.Errorf("load Threads publication media: %w", err)
	}
	imageURL, err := b.threadMediaURL(mediaValue.DeliveryKey)
	if err != nil {
		return "", errThreadMediaDeliveryUnavailable
	}
	return imagePublisher.CreateImage(ctx, draft.Text, imageURL, "")
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
