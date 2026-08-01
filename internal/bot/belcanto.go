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
	"unicode"
	"unicode/utf8"

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
	fallbackLicensedPhotoQuery  = "vintage microphone close up"
)

var (
	errThreadPublishBusy              = errors.New("threads draft publish is already in progress")
	errThreadMediaDeliveryUnavailable = errors.New("threads media delivery is unavailable")
)

func (b *Service) handleBelcantoCommand(ctx context.Context, updateID, chatID int64, user domain.User, lang language) error {
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
	if b.threadGenerator == nil {
		return b.sendText(ctx, chatID, belcantoGeneratorUnavailableText(lang), nil)
	}
	brief, _, err := b.store.StartThreadBrief(ctx, user.TelegramID, updateID, domain.ThreadVoiceBelcanto)
	if err != nil {
		return fmt.Errorf("start Threads brief: %w", err)
	}
	keyboard, err := threadBriefObjectiveKeyboard(b.callbacks, user.TelegramID, brief)
	if err != nil {
		return err
	}
	b.metrics.Inc("belcanto_briefs_started")
	return b.sendText(ctx, chatID, threadBriefObjectivePromptText(), keyboard)
}

func (b *Service) isBelcantoOperator(telegramID int64) bool {
	_, ok := b.belcantoOperators[telegramID]
	return ok
}

func threadObjectiveForAction(action session.Action) (domain.ThreadObjective, bool) {
	switch action {
	case session.ActionThreadObjectiveReach:
		return domain.ThreadObjectiveReach, true
	case session.ActionThreadObjectiveReplies:
		return domain.ThreadObjectiveReplies, true
	case session.ActionThreadObjectiveTrust:
		return domain.ThreadObjectiveTrust, true
	case session.ActionThreadObjectiveTrial:
		return domain.ThreadObjectiveTrial, true
	case session.ActionThreadObjectiveCommunity:
		return domain.ThreadObjectiveCommunity, true
	default:
		return "", false
	}
}

func (b *Service) selectThreadBriefObjective(
	ctx context.Context,
	chatID int64,
	user domain.User,
	payload session.CallbackPayload,
	objective domain.ThreadObjective,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	brief, err := b.store.SetThreadBriefObjective(
		ctx, payload.InteractionID, user.TelegramID, payload.Revision, objective,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadBriefState) {
			// A durable callback may be retried after the objective transition
			// committed but before Telegram acknowledged the material prompt.
			current, lookupErr := b.store.GetThreadBrief(ctx, payload.InteractionID, user.TelegramID)
			if lookupErr == nil && current.Current && current.State == domain.ThreadBriefAwaitingMaterial && current.Objective == objective {
				keyboard, keyboardErr := threadBriefMaterialKeyboard(b.callbacks, user.TelegramID, current)
				if keyboardErr != nil {
					return keyboardErr
				}
				return b.sendText(ctx, chatID, threadBriefMaterialPromptText(current.Objective), keyboard)
			}
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	keyboard, err := threadBriefMaterialKeyboard(b.callbacks, user.TelegramID, brief)
	if err != nil {
		return err
	}
	b.metrics.Inc("belcanto_brief_objective_selected")
	return b.sendText(ctx, chatID, threadBriefMaterialPromptText(brief.Objective), keyboard)
}

func (b *Service) showThreadBriefObjectives(
	ctx context.Context,
	chatID int64,
	user domain.User,
	payload session.CallbackPayload,
) error {
	brief, err := b.store.GetThreadBrief(ctx, payload.InteractionID, user.TelegramID)
	if err != nil || !brief.Current || brief.Revision != payload.Revision || brief.State != domain.ThreadBriefAwaitingMaterial {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	keyboard, err := threadBriefObjectiveKeyboard(b.callbacks, user.TelegramID, brief)
	if err != nil {
		return err
	}
	return b.sendText(ctx, chatID, threadBriefObjectivePromptText(), keyboard)
}

func (b *Service) cancelThreadBrief(
	ctx context.Context,
	chatID int64,
	user domain.User,
	payload session.CallbackPayload,
) error {
	err := b.store.CancelThreadBrief(ctx, payload.InteractionID, user.TelegramID, payload.Revision)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadBriefState) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	b.metrics.Inc("belcanto_briefs_cancelled")
	return b.sendText(ctx, chatID, threadBriefCancelledText(), nil)
}

func (b *Service) cancelCurrentThreadBriefIfPending(
	ctx context.Context,
	chatID int64,
	user domain.User,
) (bool, error) {
	if !b.isBelcantoOperator(user.TelegramID) {
		return false, nil
	}
	brief, err := b.store.GetCurrentThreadBrief(ctx, user.TelegramID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	switch brief.State {
	case domain.ThreadBriefAwaitingGoal, domain.ThreadBriefAwaitingMaterial, domain.ThreadBriefMaterialReady:
	default:
		return false, nil
	}
	if err := b.store.CancelThreadBrief(ctx, brief.ID, user.TelegramID, brief.Revision); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadBriefState) {
			return true, b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return true, err
	}
	b.metrics.Inc("belcanto_briefs_cancelled")
	return true, b.sendText(ctx, chatID, threadBriefCancelledText(), nil)
}

func validThreadBriefMaterial(value string) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > domain.MaxThreadMaterialRunes {
		return false
	}
	for _, character := range value {
		if (unicode.IsControl(character) && character != '\n' && character != '\t') || unicode.In(character, unicode.Cf) {
			return false
		}
	}
	return true
}

func threadBriefMaterialText(message telegram.Message) string {
	if text := strings.TrimSpace(message.Text); text != "" {
		return text
	}
	return strings.TrimSpace(message.Caption)
}

func (b *Service) tryHandleThreadBriefMaterial(
	ctx context.Context,
	updateID int64,
	message telegram.Message,
	user domain.User,
) (bool, error) {
	if !b.isBelcantoOperator(user.TelegramID) {
		return false, nil
	}
	// Recover the exact preview when this message already completed a brief but
	// the worker crashed before Telegram acknowledged the outbound send.
	if existing, lookupErr := b.store.GetThreadDraftByGenerationUpdate(ctx, user.TelegramID, updateID); lookupErr == nil {
		keyboard, keyboardErr := b.threadDraftKeyboard(ctx, user.TelegramID, existing)
		if keyboardErr != nil {
			return true, keyboardErr
		}
		return true, b.sendThreadDraftPreview(ctx, message.Chat.ID, existing, keyboard)
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return true, lookupErr
	}
	brief, err := b.store.GetCurrentThreadBrief(ctx, user.TelegramID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if brief.State == domain.ThreadBriefMaterialReady && brief.MaterialUpdateID == updateID {
		return true, b.generateThreadBrief(ctx, updateID, message.Chat.ID, user, brief)
	}
	if brief.State != domain.ThreadBriefAwaitingMaterial {
		return false, nil
	}
	material := threadBriefMaterialText(message)
	if !validThreadBriefMaterial(material) {
		keyboard, keyboardErr := threadBriefMaterialKeyboard(b.callbacks, user.TelegramID, brief)
		if keyboardErr != nil {
			return true, keyboardErr
		}
		return true, b.sendText(ctx, message.Chat.ID, threadBriefMaterialInvalidText(), keyboard)
	}
	brief, err = b.store.SetThreadBriefMaterial(
		ctx, brief.ID, user.TelegramID, brief.Revision, updateID, domain.ThreadMaterialText, material,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadBriefState) {
			return true, b.sendText(ctx, message.Chat.ID, threadDraftStaleText(), nil)
		}
		return true, err
	}
	return true, b.generateThreadBrief(ctx, updateID, message.Chat.ID, user, brief)
}

func (b *Service) completeThreadBriefWithoutMaterial(
	ctx context.Context,
	updateID, chatID int64,
	user domain.User,
	payload session.CallbackPayload,
) error {
	current, currentErr := b.store.GetThreadBrief(ctx, payload.InteractionID, user.TelegramID)
	if currentErr != nil {
		if errors.Is(currentErr, store.ErrNotFound) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return currentErr
	}
	if current.Objective == domain.ThreadObjectiveTrial && current.Current && current.State == domain.ThreadBriefAwaitingMaterial && current.Revision == payload.Revision {
		keyboard, keyboardErr := threadBriefMaterialKeyboard(b.callbacks, user.TelegramID, current)
		if keyboardErr != nil {
			return keyboardErr
		}
		return b.sendText(ctx, chatID, threadBriefTrialMaterialRequiredText(), keyboard)
	}
	brief, err := b.store.SetThreadBriefMaterial(
		ctx, payload.InteractionID, user.TelegramID, payload.Revision, updateID, domain.ThreadMaterialNone, "",
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadBriefState) {
			if draft, lookupErr := b.store.GetThreadDraftByGenerationUpdate(ctx, user.TelegramID, updateID); lookupErr == nil {
				keyboard, keyboardErr := b.threadDraftKeyboard(ctx, user.TelegramID, draft)
				if keyboardErr != nil {
					return keyboardErr
				}
				return b.sendThreadDraftPreview(ctx, chatID, draft, keyboard)
			}
			if current.State == domain.ThreadBriefMaterialReady && current.MaterialUpdateID == updateID {
				return b.generateThreadBrief(ctx, updateID, chatID, user, current)
			}
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	return b.generateThreadBrief(ctx, updateID, chatID, user, brief)
}

func (b *Service) retryThreadBrief(
	ctx context.Context,
	updateID, chatID int64,
	user domain.User,
	payload session.CallbackPayload,
) error {
	if existing, lookupErr := b.store.GetThreadDraftByGenerationUpdate(ctx, user.TelegramID, updateID); lookupErr == nil {
		keyboard, keyboardErr := b.threadDraftKeyboard(ctx, user.TelegramID, existing)
		if keyboardErr != nil {
			return keyboardErr
		}
		return b.sendThreadDraftPreview(ctx, chatID, existing, keyboard)
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return lookupErr
	}
	brief, err := b.store.GetThreadBrief(ctx, payload.InteractionID, user.TelegramID)
	if err != nil || !brief.Current || brief.Revision != payload.Revision || brief.State != domain.ThreadBriefMaterialReady {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	return b.generateThreadBrief(ctx, updateID, chatID, user, brief)
}

func (b *Service) generateThreadBrief(
	ctx context.Context,
	updateID, chatID int64,
	user domain.User,
	brief domain.ThreadBrief,
) error {
	return b.prepareThreadDraft(ctx, updateID, chatID, user, brief.Voice, "", nil, &brief)
}

func (b *Service) prepareThreadDraft(
	ctx context.Context,
	generationUpdateID, chatID int64,
	user domain.User,
	voice domain.ThreadVoice,
	transform string,
	previous *domain.ThreadDraft,
	brief *domain.ThreadBrief,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	if b.threadGenerator == nil {
		return b.sendText(ctx, chatID, belcantoGeneratorUnavailableText(userLanguage(user.Language)), nil)
	}
	if generationUpdateID > 0 {
		if existing, lookupErr := b.store.GetThreadDraftByGenerationUpdate(ctx, user.TelegramID, generationUpdateID); lookupErr == nil {
			keyboard, keyboardErr := b.threadDraftKeyboard(ctx, user.TelegramID, existing)
			if keyboardErr != nil {
				return keyboardErr
			}
			return b.sendThreadDraftPreview(ctx, chatID, existing, keyboard)
		} else if !errors.Is(lookupErr, store.ErrNotFound) {
			return lookupErr
		}
	}

	objective := domain.ThreadObjectiveReplies
	materialKind := domain.ThreadMaterialNone
	material := ""
	previousScenarioID := ""
	if brief != nil {
		objective = brief.Objective
		materialKind = brief.MaterialKind
		material = brief.MaterialText
	}
	if previous != nil {
		previousScenarioID = previous.ScenarioID
		if previous.Objective.Valid() && previous.Objective != domain.ThreadObjectiveLegacy {
			objective = previous.Objective
		}
		if previous.BriefID > 0 {
			storedBrief, briefErr := b.store.GetThreadBrief(ctx, previous.BriefID, user.TelegramID)
			if briefErr != nil {
				return fmt.Errorf("load Threads brief for refinement: %w", briefErr)
			}
			brief = &storedBrief
			materialKind = storedBrief.MaterialKind
			material = storedBrief.MaterialText
		}
	}

	stopAction := b.startChatAction(ctx, chatID, telegram.ChatActionTyping)
	defer stopAction()
	recent, err := b.store.ListRecentThreadTexts(ctx, user.TelegramID, 20)
	if err != nil {
		return fmt.Errorf("list recent Threads drafts: %w", err)
	}
	request := ai.ThreadPostRequest{
		Voice: voice, Objective: objective, MaterialKind: materialKind, Material: material,
		Transform: transform, PreviousScenarioID: previousScenarioID, RecentTexts: recent,
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
		return b.sendThreadGenerationFailure(ctx, chatID, user, previous, brief)
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
		return b.sendThreadGenerationFailure(ctx, chatID, user, previous, brief)
	}
	// The application, not a model/provider implementation, owns the external
	// stock-search boundary. Derive the only allowed query from bounded scenario
	// metadata before persistence or any automatic Pexels call.
	result.Visual.Query = ai.SafeThreadPhotoQuery(result.ScenarioID)
	draft := domain.ThreadDraft{
		TelegramID: user.TelegramID, Voice: voice, Goal: result.Goal,
		Objective: result.Objective, ScenarioID: result.ScenarioID,
		GenerationID: generationID, GenerationUpdateID: generationUpdateID,
		PhotoQuery: result.Visual.Query, Text: result.Text,
		Provider: result.Provider, Model: result.Model, Revision: revision,
		MediaMode: domain.ThreadMediaText, State: domain.ThreadDraftReady, Current: true,
	}
	if brief != nil {
		draft.BriefID = brief.ID
	}
	if strings.TrimSpace(draft.PhotoQuery) == "" {
		draft.PhotoQuery = fallbackLicensedPhotoQuery
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
			// An explicit operator choice survives small text refinements. An
			// automatic Pexels suggestion is reconsidered on every revision so it
			// cannot silently drift from the new text.
			preserve = previousMedia.EffectiveSourceKind() == domain.ThreadMediaSourceTelegram ||
				(previousMedia.EffectiveSourceKind() == domain.ThreadMediaSourcePexels && previousMedia.AttachUpdateID > 0)
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
	if brief != nil && previous == nil {
		draft, _, err = b.store.CreateThreadDraftForBrief(ctx, brief.ID, brief.Revision, draft, sourcedMedia)
		if err != nil {
			return fmt.Errorf("save Threads draft for brief: %w", err)
		}
	} else {
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
	}
	if draft.MediaID > 0 {
		storedMedia, mediaErr := b.store.GetThreadMedia(ctx, draft.MediaID, user.TelegramID)
		if mediaErr != nil {
			return fmt.Errorf("load stored Threads preview media: %w", mediaErr)
		}
		previewMedia = &storedMedia
	}
	keyboard, err := b.threadDraftKeyboard(ctx, user.TelegramID, draft)
	if err != nil {
		return err
	}
	b.recordBelcantoDraftReady(draft)
	b.metrics.Add("input_tokens", int64(result.Usage.InputTokens))
	b.metrics.Add("output_tokens", int64(result.Usage.OutputTokens))
	previewErr := b.sendThreadDraftPreview(ctx, chatID, draft, keyboard)
	previewStatus := "sent_acknowledged"
	if previewErr != nil {
		previewStatus = "send_error_unknown"
	}
	b.logBelcantoEditorialAudit(user.TelegramID, draft, transform, result, previewMedia, previewStatus, brief)
	return previewErr
}

func (b *Service) sendThreadGenerationFailure(
	ctx context.Context,
	chatID int64,
	user domain.User,
	previous *domain.ThreadDraft,
	brief *domain.ThreadBrief,
) error {
	if previous != nil {
		keyboard, err := b.threadDraftKeyboard(ctx, user.TelegramID, *previous)
		if err != nil {
			return err
		}
		return b.sendText(ctx, chatID, belcantoRefinementErrorText(userLanguage(user.Language)), keyboard)
	}
	if brief != nil {
		keyboard, err := threadBriefRetryKeyboard(b.callbacks, user.TelegramID, *brief)
		if err != nil {
			return err
		}
		return b.sendText(ctx, chatID, belcantoGenerationErrorText(userLanguage(user.Language)), keyboard)
	}
	return b.sendText(ctx, chatID, belcantoGenerationErrorText(userLanguage(user.Language)), nil)
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
		result.Objective = candidate.Objective
		result.ScenarioID = candidate.ScenarioID
		result.Mechanism = candidate.Mechanism
		result.MaterialBasis = candidate.MaterialBasis
		result.Evidence = candidate.Evidence
		result.Text = candidate.Text
		result.Visual = ai.ThreadPostVisualRecommendation{Mode: "text_only", Query: fallbackLicensedPhotoQuery}
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
	selectedObjective := domain.ThreadObjective("")
	selectedScenarioID := ""
	selectedMechanism := ""
	selectedText := ""
	ids := make(map[string]struct{}, requiredFinalists)
	scenarios := make(map[string]struct{}, requiredFinalists)
	mechanisms := make(map[string]struct{}, requiredFinalists)
	for _, candidate := range result.Audit.Candidates {
		if !candidate.Eligible || !candidate.Considered {
			if candidate.Selected {
				return false
			}
			continue
		}
		finalists++
		if strings.TrimSpace(candidate.ReviewerID) == "" || strings.TrimSpace(candidate.Goal) == "" ||
			!candidate.Objective.Valid() || strings.TrimSpace(candidate.ScenarioID) == "" ||
			!knownThreadScenario(candidate.ScenarioID) || candidate.ScenarioID == "legacy_unspecified" ||
			strings.TrimSpace(candidate.Mechanism) == "" || strings.TrimSpace(candidate.Text) == "" {
			return false
		}
		if _, duplicate := ids[candidate.ReviewerID]; duplicate {
			return false
		}
		ids[candidate.ReviewerID] = struct{}{}
		if _, duplicate := scenarios[candidate.ScenarioID]; duplicate {
			return false
		}
		scenarios[candidate.ScenarioID] = struct{}{}
		mechanisms[candidate.Mechanism] = struct{}{}
		if candidate.Selected {
			selected++
			selectedID = candidate.ReviewerID
			selectedGoal = candidate.Goal
			selectedObjective = candidate.Objective
			selectedScenarioID = candidate.ScenarioID
			selectedMechanism = candidate.Mechanism
			selectedText = candidate.Text
		}
	}
	return finalists == requiredFinalists && len(scenarios) == requiredFinalists && len(mechanisms) >= 4 && selected == 1 &&
		result.Audit.DeliveredWinnerID == selectedID && result.Goal == selectedGoal &&
		result.Objective == selectedObjective && result.ScenarioID == selectedScenarioID &&
		result.Mechanism == selectedMechanism && result.Text == selectedText
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
	excludedAssetIDs ...string,
) (domain.ThreadMedia, error) {
	var (
		asset photos.Asset
		err   error
	)
	if len(excludedAssetIDs) > 0 {
		if alternative, ok := b.threadPhotoSource.(photos.AlternativeSource); ok {
			asset, err = alternative.FindAlternative(ctx, recommendation.Query, excludedAssetIDs...)
		} else {
			asset, err = b.threadPhotoSource.Find(ctx, recommendation.Query)
		}
	} else {
		asset, err = b.threadPhotoSource.Find(ctx, recommendation.Query)
	}
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
		SourceAuthor: asset.Author, SourceAuthorURL: asset.AuthorURL, SourceQuery: recommendation.Query,
		Data: normalized.Data, MediaType: normalized.MediaType, Width: normalized.Width, Height: normalized.Height,
		Digest: hex.EncodeToString(digest[:]), DeliveryKey: deliveryKey,
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return domain.ThreadMedia{}, err
	}
	return mediaValue, nil
}

func (b *Service) selectThreadDraftLicensedPhoto(
	ctx context.Context,
	updateID, chatID int64,
	user domain.User,
	payload session.CallbackPayload,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	// A durable callback can be retried after the licensed-media transaction
	// committed but before Telegram acknowledged the exact photo preview.
	committed, err := b.store.GetThreadDraftByMediaAttachUpdate(ctx, user.TelegramID, updateID)
	if err == nil {
		if committed.ID != payload.InteractionID || !committed.Current {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		keyboard, keyboardErr := b.threadDraftKeyboard(ctx, user.TelegramID, committed)
		if keyboardErr != nil {
			return keyboardErr
		}
		return b.sendThreadDraftPreview(ctx, chatID, committed, keyboard)
	}
	if errors.Is(err, store.ErrThreadDraftState) {
		return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if b.threadPhotoSource == nil {
		return b.sendText(ctx, chatID, threadLicensedPhotoDisabledText(), nil)
	}
	draft, err := b.store.GetThreadDraft(ctx, payload.InteractionID, user.TelegramID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return err
	}
	if !draft.Current || draft.Revision != payload.Revision ||
		(draft.State != domain.ThreadDraftReady && draft.State != domain.ThreadDraftFailed) {
		return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
	}
	// Never forward a persisted model-authored query to an external photo
	// provider. Older drafts may predate the fixed-query boundary, so derive the
	// search phrase again from the bounded scenario ID on every manual search.
	query := ai.SafeThreadPhotoQuery(draft.ScenarioID)
	var excludedAssetIDs []string
	var currentAssetID, currentDigest string
	if draft.MediaID > 0 {
		currentMedia, mediaErr := b.store.GetThreadMedia(ctx, draft.MediaID, user.TelegramID)
		if mediaErr != nil {
			return fmt.Errorf("load current Threads media for Pexels replacement: %w", mediaErr)
		}
		currentDigest = currentMedia.Digest
		if currentMedia.EffectiveSourceKind() == domain.ThreadMediaSourcePexels {
			currentAssetID = currentMedia.SourceAssetID
			excludedAssetIDs = append(excludedAssetIDs, currentMedia.SourceAssetID)
		}
	}
	stopAction := b.startChatAction(ctx, chatID, telegram.ChatActionUploadPhoto)
	defer stopAction()
	mediaValue, err := b.prepareLicensedThreadMedia(
		ctx, user.TelegramID,
		ai.ThreadPostVisualRecommendation{Mode: "licensed_photo", Query: query},
		excludedAssetIDs...,
	)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.metrics.Inc("belcanto_photo_source_fallbacks")
		b.logger.Warn(
			"Belcanto manual licensed photo unavailable; draft unchanged",
			"user", observability.UserHash(b.config.CallbackSecret, user.TelegramID),
			"error", safeErrorCode(err),
		)
		return b.sendText(ctx, chatID, threadLicensedPhotoErrorText(err, draft.MediaID > 0), nil)
	}
	mediaValue.AttachUpdateID = updateID
	if err := mediaValue.ValidateForStore(); err != nil {
		return fmt.Errorf("validate manual licensed Threads media: %w", err)
	}
	if (currentAssetID != "" && mediaValue.SourceAssetID == currentAssetID) ||
		(currentDigest != "" && mediaValue.Digest == currentDigest) {
		b.metrics.Inc("belcanto_photo_source_fallbacks")
		return b.sendText(ctx, chatID, threadLicensedPhotoUnavailableText(true), nil)
	}
	if err := validateUpdateLease(ctx); err != nil {
		return err
	}
	updated, err := b.store.AttachLicensedThreadDraftMedia(
		ctx, draft.ID, user.TelegramID, draft.Revision, mediaValue,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrThreadDraftState) {
			return b.sendText(ctx, chatID, threadDraftStaleText(), nil)
		}
		return fmt.Errorf("attach licensed Threads media: %w", err)
	}
	keyboard, err := b.threadDraftKeyboard(ctx, user.TelegramID, updated)
	if err != nil {
		return err
	}
	attachedMedia, err := b.store.GetThreadMedia(ctx, updated.MediaID, user.TelegramID)
	if err != nil {
		return fmt.Errorf("load attached licensed Threads media: %w", err)
	}
	b.metrics.Inc("belcanto_licensed_media_selected")
	b.logBelcantoMediaChanged(user.TelegramID, updated, &attachedMedia, "pexels_selected")
	return b.sendThreadDraftPreview(ctx, chatID, updated, keyboard)
}

func (b *Service) sendThreadDraftPreview(
	ctx context.Context,
	chatID int64,
	draft domain.ThreadDraft,
	keyboard *telegram.InlineKeyboardMarkup,
) error {
	materialKind := domain.ThreadMaterialNone
	if draft.BriefID > 0 {
		brief, err := b.store.GetThreadBrief(ctx, draft.BriefID, draft.TelegramID)
		if err != nil {
			return fmt.Errorf("load Threads brief for preview: %w", err)
		}
		materialKind = brief.MaterialKind
	}
	if draft.MediaMode != domain.ThreadMediaImage {
		return b.sendText(ctx, chatID, threadDraftTextWithBrief(draft, materialKind), keyboard)
	}
	mediaValue, err := b.store.GetThreadMedia(ctx, draft.MediaID, draft.TelegramID)
	if err != nil {
		return fmt.Errorf("load Threads preview media: %w", err)
	}
	keyboard = threadDraftKeyboardWithMediaSource(keyboard, mediaValue)
	_, err = b.telegram.SendPhoto(ctx, telegram.SendPhotoParams{
		ChatID: chatID, Photo: telegram.FileUpload("belcanto-threads.jpg", mediaValue.Data),
		Caption: threadDraftTextWithMediaAndBrief(draft, mediaValue, materialKind), ReplyMarkup: keyboard, ProtectContent: true,
	})
	return err
}

func (b *Service) threadDraftKeyboard(
	ctx context.Context,
	telegramID int64,
	draft domain.ThreadDraft,
) (*telegram.InlineKeyboardMarkup, error) {
	pexelsSelected := false
	if draft.MediaID > 0 {
		mediaValue, err := b.store.GetThreadMedia(ctx, draft.MediaID, telegramID)
		if err != nil {
			return nil, fmt.Errorf("load Threads media for keyboard: %w", err)
		}
		pexelsSelected = mediaValue.EffectiveSourceKind() == domain.ThreadMediaSourcePexels
	}
	return threadDraftKeyboard(
		b.callbacks, telegramID, draft, b.threadPhotoSource != nil, pexelsSelected,
	)
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
	updateID, chatID int64,
	user domain.User,
	payload session.CallbackPayload,
	voice domain.ThreadVoice,
	transform string,
) error {
	if !b.isBelcantoOperator(user.TelegramID) {
		return b.sendText(ctx, chatID, belcantoOwnerOnlyText(userLanguage(user.Language)), nil)
	}
	if existing, lookupErr := b.store.GetThreadDraftByGenerationUpdate(ctx, user.TelegramID, updateID); lookupErr == nil {
		keyboard, keyboardErr := b.threadDraftKeyboard(ctx, user.TelegramID, existing)
		if keyboardErr != nil {
			return keyboardErr
		}
		return b.sendThreadDraftPreview(ctx, chatID, existing, keyboard)
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return lookupErr
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
	return b.prepareThreadDraft(ctx, updateID, chatID, user, voice, transform, &draft, nil)
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
	keyboard, err := b.threadDraftKeyboard(ctx, telegramID, draft)
	if err != nil {
		return err
	}
	if mode == domain.ThreadMediaImagePending {
		b.metrics.Inc("belcanto_media_requested")
		b.logBelcantoMediaChanged(telegramID, draft, b.threadDraftMediaForAudit(ctx, telegramID, draft), "own_photo_requested")
		return b.sendText(ctx, chatID, threadImagePromptText(draft.MediaID > 0), keyboard)
	}
	if mode == domain.ThreadMediaImage {
		b.metrics.Inc("belcanto_media_replacement_cancelled")
		b.logBelcantoMediaChanged(telegramID, draft, b.threadDraftMediaForAudit(ctx, telegramID, draft), "previous_photo_kept")
	} else {
		b.metrics.Inc("belcanto_text_format_selected")
		b.logBelcantoMediaChanged(telegramID, draft, nil, "text_selected")
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
		keyboard, keyboardErr := b.threadDraftKeyboard(ctx, user.TelegramID, existing)
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
	keyboard, err := b.threadDraftKeyboard(ctx, user.TelegramID, draft)
	if err != nil {
		return true, err
	}
	b.metrics.Inc("belcanto_media_attached")
	b.logBelcantoMediaChanged(user.TelegramID, draft, &mediaValue, "own_photo_uploaded")
	return true, b.sendThreadDraftPreview(ctx, message.Chat.ID, draft, keyboard)
}

func (b *Service) threadDraftMediaForAudit(
	ctx context.Context,
	telegramID int64,
	draft domain.ThreadDraft,
) *domain.ThreadMedia {
	if draft.MediaID <= 0 {
		return nil
	}
	mediaValue, err := b.store.GetThreadMedia(ctx, draft.MediaID, telegramID)
	if err != nil {
		return nil
	}
	return &mediaValue
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
		b.recordBelcantoPublished(draft)
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
				b.recordBelcantoPublished(draft)
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
	b.recordBelcantoPublished(draft)
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
			b.recordBelcantoPublished(draft)
			return b.sendText(ctx, chatID, threadPublishedText(threadspub.Publication{}), nil)
		}
	}
	return b.sendText(ctx, chatID, threadPublishUnknownText(), nil)
}

func (b *Service) recordBelcantoDraftReady(draft domain.ThreadDraft) {
	b.metrics.Inc("belcanto_drafts_ready")
	if draft.Objective.Selectable() {
		b.metrics.Inc("belcanto_drafts_ready_objective_" + string(draft.Objective))
	}
	if knownThreadScenario(draft.ScenarioID) && draft.ScenarioID != "legacy_unspecified" {
		b.metrics.Inc("belcanto_drafts_ready_scenario_" + draft.ScenarioID)
	}
}

func (b *Service) recordBelcantoPublished(draft domain.ThreadDraft) {
	b.metrics.Inc("belcanto_posts_published")
	if draft.Objective.Selectable() {
		b.metrics.Inc("belcanto_posts_published_objective_" + string(draft.Objective))
	}
	if knownThreadScenario(draft.ScenarioID) && draft.ScenarioID != "legacy_unspecified" {
		b.metrics.Inc("belcanto_posts_published_scenario_" + draft.ScenarioID)
	}
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
