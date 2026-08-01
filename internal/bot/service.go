package bot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/media"
	"github.com/aleka7sk/witty-reply/internal/meme"
	"github.com/aleka7sk/witty-reply/internal/observability"
	"github.com/aleka7sk/witty-reply/internal/photos"
	"github.com/aleka7sk/witty-reply/internal/safety"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
	"github.com/aleka7sk/witty-reply/internal/transcribe"
)

var errLowTranscriptionConfidence = errors.New("transcription confidence is too low")

type TelegramClient interface {
	SendMessage(context.Context, telegram.SendMessageParams) (telegram.Message, error)
	SendPhoto(context.Context, telegram.SendPhotoParams) (telegram.Message, error)
	SendChatAction(context.Context, telegram.SendChatActionParams) error
	AnswerCallbackQuery(context.Context, telegram.AnswerCallbackQueryParams) error
	DownloadFileLimit(context.Context, string, int64) (telegram.DownloadedFile, error)
}

type Limits struct {
	TextDaily       int
	MediaDaily      int
	MemeDaily       int
	RefinementDaily int
	StyleExamples   int
	MaxTextRunes    int
	MaxImageBytes   int
	MaxVoiceBytes   int
}

type Config struct {
	ProviderTimeout       time.Duration
	ThreadProviderTimeout time.Duration
	UsageLocation         *time.Location
	CallbackSecret        string
	PrivacyURL            string
	SpeechProvider        string
	BelcantoOperatorIDs   []int64
	ThreadsPublisher      threadspub.Publisher
	ThreadMediaURL        func(string) (string, error)
	ThreadPhotoSource     photos.Source
	BelcantoReviewLogMode string
	Limits                Limits
}

type Interaction struct {
	SourceID           int64
	Input              domain.Input
	Tone               domain.Tone
	Mode               domain.ScenarioMode
	SourceHint         string
	Language           string
	PreviousReplies    []string
	GenerationID       int64
	Revision           uint32
	ChatID             int64
	AwaitingMode       bool
	AutoDetected       bool
	FreeModeSwitch     bool
	QuotaReservationID int64
	QuotaCategory      domain.QuotaCategory
}

// interaction keeps existing package-local tests concise while exposing the
// cache value type only for application composition.
type interaction = Interaction

// UpdateQueuePolicy is persisted with the encrypted inbox row. Supersedable
// work may be terminally replaced, while Superseding work replaces only older
// generative work for the same actor.
type UpdateQueuePolicy struct {
	Supersedable bool
	Superseding  bool
}

type updateLeaseGuard func(context.Context) error

type updateLeaseGuardKey struct{}

type sessionPersistenceContextKey struct{}

func withUpdateLeaseGuard(ctx context.Context, guard updateLeaseGuard) context.Context {
	return context.WithValue(ctx, updateLeaseGuardKey{}, guard)
}

func withSessionPersistenceContext(ctx, persistent context.Context) context.Context {
	return context.WithValue(ctx, sessionPersistenceContextKey{}, persistent)
}

func sessionPersistenceContext(ctx context.Context) (context.Context, bool) {
	persistent, _ := ctx.Value(sessionPersistenceContextKey{}).(context.Context)
	if persistent == nil {
		return nil, false
	}
	return persistent, true
}

func validateUpdateLease(ctx context.Context) error {
	guard, _ := ctx.Value(updateLeaseGuardKey{}).(updateLeaseGuard)
	if guard == nil {
		return nil
	}
	return guard(ctx)
}

type Service struct {
	telegram          TelegramClient
	provider          ai.Provider
	threadGenerator   ai.ThreadPostGenerator
	threadPublisher   threadspub.Publisher
	threadMediaURL    func(string) (string, error)
	threadPhotoSource photos.Source
	transcriber       transcribe.Transcriber
	store             store.Store
	sessions          *session.Cache[interaction]
	callbacks         *session.CallbackCodec
	safety            *safety.Filter
	threadSafety      *safety.Filter
	renderer          *meme.Renderer
	metrics           *observability.Metrics
	logger            *slog.Logger
	config            Config
	belcantoOperators map[int64]struct{}

	chatActions sync.WaitGroup
}

func NewService(
	telegramClient TelegramClient,
	provider ai.Provider,
	transcriber transcribe.Transcriber,
	dataStore store.Store,
	sessions *session.Cache[interaction],
	callbacks *session.CallbackCodec,
	safetyFilter *safety.Filter,
	renderer *meme.Renderer,
	metrics *observability.Metrics,
	logger *slog.Logger,
	config Config,
) (*Service, error) {
	if telegramClient == nil || provider == nil || transcriber == nil || dataStore == nil || sessions == nil || callbacks == nil || safetyFilter == nil {
		return nil, errors.New("bot dependencies must not be nil")
	}
	if metrics == nil {
		metrics = observability.NewMetrics()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if config.ProviderTimeout <= 0 {
		config.ProviderTimeout = 45 * time.Second
	}
	if config.ThreadProviderTimeout <= 0 {
		config.ThreadProviderTimeout = 120 * time.Second
	}
	if config.BelcantoReviewLogMode == "" {
		config.BelcantoReviewLogMode = "full"
	}
	if config.BelcantoReviewLogMode != "off" && config.BelcantoReviewLogMode != "metadata" && config.BelcantoReviewLogMode != "full" {
		return nil, errors.New("belcanto review log mode must be off, metadata, or full")
	}
	if config.UsageLocation == nil {
		config.UsageLocation = time.UTC
	}
	if config.Limits.TextDaily < 1 || config.Limits.MediaDaily < 1 || config.Limits.MemeDaily < 1 || config.Limits.RefinementDaily < 1 || config.Limits.StyleExamples < 1 {
		return nil, errors.New("bot quota limits must be positive")
	}
	threadGenerator, _ := provider.(ai.ThreadPostGenerator)
	operators := make(map[int64]struct{}, len(config.BelcantoOperatorIDs))
	for _, id := range config.BelcantoOperatorIDs {
		if id > 0 {
			operators[id] = struct{}{}
		}
	}
	return &Service{
		telegram: telegramClient, provider: provider, threadGenerator: threadGenerator, threadPublisher: config.ThreadsPublisher,
		threadMediaURL: config.ThreadMediaURL, threadPhotoSource: config.ThreadPhotoSource,
		transcriber: transcriber, store: dataStore, sessions: sessions,
		callbacks: callbacks, safety: safetyFilter, threadSafety: safety.New(safety.Config{MaxRunes: 500, CandidateCount: 1}),
		renderer: renderer, metrics: metrics, logger: logger, config: config, belcantoOperators: operators,
	}, nil
}

func (b *Service) HandleUpdate(ctx context.Context, update telegram.Update) error {
	if update.UpdateID <= 0 {
		return nil
	}
	if err := validateUpdateLease(ctx); err != nil {
		return err
	}
	b.metrics.Inc("updates_accepted")

	switch {
	case update.CallbackQuery != nil:
		return b.handleCallback(ctx, update.UpdateID, *update.CallbackQuery)
	case update.Message != nil:
		return b.handleMessage(ctx, update.UpdateID, *update.Message)
	default:
		b.metrics.Inc("updates_ignored")
		return nil
	}
}

// Wait blocks until best-effort chat-action goroutines have observed their
// cancellation. Call it after the update processor has stopped its workers.
func (b *Service) Wait() { b.chatActions.Wait() }

// QueuePolicy classifies only user actions whose meaning is available without
// reading durable user data. Unrelated commands and callbacks remain strictly
// ordered and are never skipped.
func (b *Service) QueuePolicy(update telegram.Update) UpdateQueuePolicy {
	if update.Message != nil && update.Message.From != nil && !update.Message.From.IsBot && update.Message.Chat.Type == "private" {
		command := parseCommand(update.Message.Text)
		switch command {
		case "new", "cancel", "delete_me", "delete_data":
			return UpdateQueuePolicy{Superseding: true}
		case "":
			if raw, err := b.classifyInput(*update.Message); err == nil {
				if raw.kind == domain.InputImage && b.isBelcantoOperator(update.Message.From.ID) {
					// An operator image may be the durable second step of a
					// Threads draft. Keep it ordered and retryable until storage
					// can decide whether it belongs to that workflow.
					return UpdateQueuePolicy{}
				}
				return UpdateQueuePolicy{Supersedable: true, Superseding: true}
			}
		}
		return UpdateQueuePolicy{}
	}
	if update.CallbackQuery == nil {
		return UpdateQueuePolicy{}
	}
	callback := update.CallbackQuery
	payload, err := b.callbacks.DecodeForUser(callback.Data, callback.From.ID)
	if err != nil {
		return UpdateQueuePolicy{}
	}
	switch payload.Action {
	case session.ActionConfirmDelete, session.ActionCancel:
		return UpdateQueuePolicy{Superseding: true}
	case session.ActionMeme, session.ActionFunnier, session.ActionSharper, session.ActionSofter,
		session.ActionShorter, session.ActionMore, session.ActionRetry,
		session.ActionModeReply, session.ActionModeComment,
		session.ActionCommentSubtler, session.ActionCommentBolder,
		session.ActionCommentAbsurd, session.ActionCommentDifferentAngle:
		// Generative callbacks remain replaceable by a newer source message, but
		// do not preempt one another. A delayed double-tap carries the previous
		// revision and must not cancel the valid generation already in flight.
		return UpdateQueuePolicy{Supersedable: true}
	case session.ActionToneSmart, session.ActionTonePlayful, session.ActionToneSharp, session.ActionToneBoundary:
		if payload.InteractionID != callback.From.ID {
			return UpdateQueuePolicy{Supersedable: true}
		}
	}
	return UpdateQueuePolicy{}
}

// PreemptUpdate cancels only work that a newly persisted update explicitly
// supersedes. It is called after durable enqueue, so a database failure cannot
// cancel the user's current request without preserving the replacement.
func (b *Service) PreemptUpdate(update telegram.Update) {
	policy := b.QueuePolicy(update)
	if !policy.Superseding {
		return
	}
	if policy.Supersedable && update.CallbackQuery != nil {
		// A refinement needs the current input snapshot. Install an equivalent
		// fresh session before cancelling the older lease so its AfterFunc cannot
		// delete the state needed by the queued refinement.
		userID := update.CallbackQuery.From.ID
		if snapshot, ok := b.sessions.Peek(userID); ok {
			_, _ = b.sessions.Begin(context.Background(), userID, snapshot.Value)
		}
		return
	}
	if update.Message != nil && update.Message.From != nil {
		b.sessions.Cancel(update.Message.From.ID)
		return
	}
	if update.CallbackQuery != nil {
		b.sessions.Cancel(update.CallbackQuery.From.ID)
	}
}

func (b *Service) handleMessage(ctx context.Context, updateID int64, message telegram.Message) error {
	if message.Chat.Type != "private" || message.From == nil || message.From.IsBot {
		b.metrics.Inc("non_private_ignored")
		return nil
	}
	user, err := b.upsertUser(ctx, *message.From)
	if err != nil {
		return err
	}
	lang := userLanguage(user.Language)
	if command := parseCommand(message.Text); command != "" {
		return b.handleCommand(ctx, message.Chat.ID, user, lang, command)
	}
	if !user.HasConsent() {
		keyboard, keyboardErr := consentKeyboard(b.callbacks, user.TelegramID, lang)
		if keyboardErr != nil {
			return keyboardErr
		}
		return b.sendText(ctx, message.Chat.ID, consentRequiredText(lang), keyboard)
	}
	if handled, mediaErr := b.tryHandleThreadMediaUpload(ctx, updateID, message, user); handled {
		return mediaErr
	}

	raw, err := b.classifyInput(message)
	if err != nil {
		b.metrics.Inc("unsupported_inputs")
		return b.sendText(ctx, message.Chat.ID, unsupportedText(lang), nil)
	}
	category, limit := b.inputQuota(raw.kind)
	reservation := quotaReservation{id: updateID, category: category}
	decision, err := b.store.ConsumeQuota(ctx, user.TelegramID, reservation.id, category, limit, b.now())
	if err != nil {
		return fmt.Errorf("consume quota: %w", err)
	}
	if !decision.Allowed {
		b.metrics.Inc("quota_denials")
		return b.sendText(ctx, message.Chat.ID, quotaText(lang, decision), nil)
	}

	tone := user.DefaultTone
	if raw.mode == domain.ScenarioComment {
		tone = domain.ToneMix
	}
	value := interaction{
		SourceID: updateID, Tone: tone, Mode: raw.mode, SourceHint: raw.sourceHint,
		Language: user.Language, Revision: 1, ChatID: message.Chat.ID,
		QuotaReservationID: reservation.id, QuotaCategory: reservation.category,
	}
	if value.Mode == "" {
		value.Mode = domain.ScenarioAuto
	}
	lease, err := b.sessions.Begin(ctx, user.TelegramID, value)
	if err != nil {
		b.refundQuota(user.TelegramID, reservation)
		return err
	}
	input, sourceLanguage, err := b.materializeInput(lease.Context, raw, user.Language)
	if err != nil {
		if ctx.Err() != nil {
			b.refundQuota(user.TelegramID, reservation)
			return ctx.Err()
		}
		if !b.sessions.IsCurrent(lease) {
			b.metrics.Inc("obsolete_jobs")
			b.refundQuota(user.TelegramID, reservation)
			return nil
		}
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		b.metrics.Inc("input_processing_errors")
		if errors.Is(err, transcribe.ErrDisabled) {
			return b.sendText(ctx, message.Chat.ID, voiceDisabledText(lang), nil)
		}
		if errors.Is(err, errLowTranscriptionConfidence) {
			return b.sendText(ctx, message.Chat.ID, voiceLowConfidenceText(lang), nil)
		}
		b.logError("input processing failed", user.TelegramID, "error", safeErrorCode(err))
		return b.sendText(ctx, message.Chat.ID, invalidInputText(lang), nil)
	}
	if err := input.Validate(b.config.Limits.MaxTextRunes, b.config.Limits.MaxImageBytes); err != nil {
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		return b.sendText(ctx, message.Chat.ID, invalidInputText(lang), nil)
	}
	value.Input = input
	value.Language = sourceLanguage
	if err := b.sessions.Commit(lease, value); err != nil {
		if ctx.Err() != nil {
			b.refundQuota(user.TelegramID, reservation)
			return ctx.Err()
		}
		b.metrics.Inc("obsolete_jobs")
		b.refundQuota(user.TelegramID, reservation)
		return nil
	}
	return b.generateWithLease(ctx, lease, message.Chat.ID, user, value, "", reservation)
}

func (b *Service) handleCommand(ctx context.Context, chatID int64, user domain.User, lang language, command string) error {
	switch command {
	case "start":
		if user.HasConsent() {
			return b.sendText(ctx, chatID, consentAcceptedText(lang), nil)
		}
		keyboard, err := consentKeyboard(b.callbacks, user.TelegramID, lang)
		if err != nil {
			return err
		}
		return b.sendText(ctx, chatID, startText(lang, b.config.PrivacyURL, b.config.SpeechProvider != "disabled"), keyboard)
	case "help":
		return b.sendText(ctx, chatID, helpText(lang), nil)
	case "privacy":
		return b.sendText(ctx, chatID, privacyText(lang, b.config.PrivacyURL, b.config.SpeechProvider != "disabled"), nil)
	case "new":
		b.sessions.Cancel(user.TelegramID)
		return b.sendText(ctx, chatID, contextClearedText(lang), nil)
	case "cancel":
		b.sessions.Cancel(user.TelegramID)
		return b.sendText(ctx, chatID, cancelledText(lang), nil)
	case "style":
		keyboard, err := styleKeyboard(b.callbacks, user.TelegramID, lang)
		if err != nil {
			return err
		}
		examples, err := b.store.ListStyleExamples(ctx, user.TelegramID, b.config.Limits.StyleExamples)
		if err != nil {
			return err
		}
		return b.sendText(ctx, chatID, styleText(lang, user.DefaultTone, len(examples), b.config.Limits.StyleExamples), keyboard)
	case "plan":
		stats, err := b.store.Stats(ctx, user.TelegramID, b.now(), b.config.Limits.TextDaily)
		if err != nil {
			return err
		}
		return b.sendText(ctx, chatID, planText(lang, stats, nextDay(b.now())), nil)
	case "belcanto", "threads":
		return b.handleBelcantoCommand(ctx, chatID, user, lang)
	case "delete_me", "delete_data":
		keyboard, err := deleteKeyboard(b.callbacks, user.TelegramID, lang)
		if err != nil {
			return err
		}
		return b.sendText(ctx, chatID, deleteConfirmText(lang), keyboard)
	default:
		return b.sendText(ctx, chatID, helpText(lang), nil)
	}
}

func (b *Service) handleCallback(ctx context.Context, updateID int64, callback telegram.CallbackQuery) error {
	lang := userLanguage(callback.From.LanguageCode)
	payload, err := b.callbacks.DecodeForUser(callback.Data, callback.From.ID)
	if err != nil {
		_ = b.telegram.AnswerCallbackQuery(ctx, telegram.AnswerCallbackQueryParams{CallbackQueryID: callback.ID, Text: expiredText(lang), ShowAlert: true})
		b.metrics.Inc("invalid_callbacks")
		return nil
	}
	if err := b.telegram.AnswerCallbackQuery(ctx, telegram.AnswerCallbackQueryParams{CallbackQueryID: callback.ID}); err != nil {
		b.metrics.Inc("callback_answer_errors")
	}
	chatID := callback.From.ID
	if callback.Message != nil {
		chatID = callback.Message.Chat.ID
	}
	user, err := b.upsertUser(ctx, callback.From)
	if err != nil {
		return err
	}
	if threadActionRequiresConsent(payload.Action) && !user.HasConsent() {
		consentLang := userLanguage(user.Language)
		keyboard, keyboardErr := consentKeyboard(b.callbacks, user.TelegramID, consentLang)
		if keyboardErr != nil {
			return keyboardErr
		}
		return b.sendText(ctx, chatID, consentRequiredText(consentLang), keyboard)
	}

	switch payload.Action {
	case session.ActionConsent:
		if payload.InteractionID != user.TelegramID {
			return nil
		}
		if err := b.store.SetConsent(ctx, user.TelegramID, true); err != nil {
			return err
		}
		b.metrics.Inc("consents")
		return b.sendText(ctx, chatID, consentAcceptedText(lang), nil)
	case session.ActionConfirmDelete:
		b.sessions.Delete(user.TelegramID)
		if err := b.store.DeleteUser(ctx, user.TelegramID); err != nil {
			return err
		}
		b.metrics.Inc("users_deleted")
		return b.sendText(ctx, chatID, deletedText(lang), nil)
	case session.ActionCancelDelete:
		return b.sendText(ctx, chatID, deleteCancelledText(lang), nil)
	case session.ActionResetStyle:
		if err := b.store.ResetStyle(ctx, user.TelegramID); err != nil {
			return err
		}
		return b.sendText(ctx, chatID, styleResetText(lang), nil)
	case session.ActionFeedbackUp, session.ActionFeedbackDown:
		rating := -1
		if payload.Action == session.ActionFeedbackUp {
			rating = 1
		}
		err := b.store.RecordFeedback(ctx, domain.Feedback{
			GenerationID: payload.InteractionID, TelegramID: user.TelegramID, Candidate: int(payload.Candidate), Rating: rating, Action: "rating",
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return b.sendText(ctx, chatID, expiredText(lang), nil)
			}
			return err
		}
		b.metrics.Inc("feedback_recorded")
		return b.sendText(ctx, chatID, feedbackThanksText(lang), nil)
	case session.ActionSaveStyle:
		return b.saveStyle(ctx, chatID, user, lang, payload)
	case session.ActionToneSmart, session.ActionTonePlayful, session.ActionToneSharp, session.ActionToneBoundary:
		tone := toneForAction(payload.Action)
		if payload.InteractionID == user.TelegramID {
			if err := b.store.SetDefaultTone(ctx, user.TelegramID, tone); err != nil {
				return err
			}
			return b.sendText(ctx, chatID, styleChangedText(lang, tone), nil)
		}
		return b.refine(ctx, updateID, chatID, user, lang, payload, tone, "tone")
	case session.ActionMeme:
		return b.refine(ctx, updateID, chatID, user, lang, payload, domain.ToneMeme, "meme")
	case session.ActionFunnier:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "funnier")
	case session.ActionSharper:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "sharper")
	case session.ActionSofter:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "softer")
	case session.ActionShorter:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "shorter")
	case session.ActionMore, session.ActionRetry:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "more")
	case session.ActionCommentSubtler:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "subtler")
	case session.ActionCommentBolder:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "bolder")
	case session.ActionCommentAbsurd:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "absurder")
	case session.ActionCommentDifferentAngle:
		return b.refine(ctx, updateID, chatID, user, lang, payload, "", "new_angle")
	case session.ActionModeReply:
		return b.changeMode(ctx, updateID, chatID, user, lang, payload, domain.ScenarioReply)
	case session.ActionModeComment:
		return b.changeMode(ctx, updateID, chatID, user, lang, payload, domain.ScenarioComment)
	case session.ActionThreadNewBelcanto:
		return b.refineThreadDraft(ctx, chatID, user, payload, domain.ThreadVoiceBelcanto, "different_angle")
	case session.ActionThreadNewAlisher:
		return b.refineThreadDraft(ctx, chatID, user, payload, domain.ThreadVoiceAlisher, "different_angle")
	case session.ActionThreadWittier:
		return b.refineThreadDraft(ctx, chatID, user, payload, "", "wittier")
	case session.ActionThreadWarmer:
		return b.refineThreadDraft(ctx, chatID, user, payload, "", "warmer")
	case session.ActionThreadShorter:
		return b.refineThreadDraft(ctx, chatID, user, payload, "", "shorter")
	case session.ActionThreadDifferentAngle:
		return b.refineThreadDraft(ctx, chatID, user, payload, "", "different_angle")
	case session.ActionThreadNoSell:
		return b.refineThreadDraft(ctx, chatID, user, payload, "", "no_sell")
	case session.ActionThreadUseImage:
		return b.setThreadDraftMediaMode(ctx, chatID, user, payload, domain.ThreadMediaImagePending)
	case session.ActionThreadUseText:
		return b.setThreadDraftMediaMode(ctx, chatID, user, payload, domain.ThreadMediaText)
	case session.ActionThreadKeepImage:
		return b.setThreadDraftMediaMode(ctx, chatID, user, payload, domain.ThreadMediaImage)
	case session.ActionThreadPublish:
		return b.publishThreadDraft(ctx, chatID, user, payload)
	case session.ActionThreadCancel:
		return b.cancelThreadDraft(ctx, chatID, user, payload)
	case session.ActionCancel:
		b.sessions.Cancel(user.TelegramID)
		return b.sendText(ctx, chatID, cancelledText(lang), nil)
	default:
		return nil
	}
}

func threadActionRequiresConsent(action session.Action) bool {
	switch action {
	case session.ActionThreadNewBelcanto,
		session.ActionThreadNewAlisher,
		session.ActionThreadWittier,
		session.ActionThreadWarmer,
		session.ActionThreadShorter,
		session.ActionThreadDifferentAngle,
		session.ActionThreadNoSell,
		session.ActionThreadPublish,
		session.ActionThreadCancel,
		session.ActionThreadUseImage,
		session.ActionThreadUseText,
		session.ActionThreadKeepImage:
		return true
	default:
		return false
	}
}

func (b *Service) refine(ctx context.Context, updateID, chatID int64, user domain.User, lang language, payload session.CallbackPayload, tone domain.Tone, transform string) error {
	snapshot, ok := b.sessions.Peek(user.TelegramID)
	if !ok || snapshot.Value.AwaitingMode || snapshot.Value.GenerationID != payload.InteractionID || snapshot.Value.Revision != payload.Revision {
		return b.sendText(ctx, chatID, expiredText(lang), nil)
	}
	if strings.HasPrefix(transform, "subtl") || transform == "bolder" || transform == "absurder" || transform == "new_angle" {
		if snapshot.Value.Mode != domain.ScenarioComment {
			return b.sendText(ctx, chatID, expiredText(lang), nil)
		}
	}
	category, limit := domain.QuotaRefinement, b.config.Limits.RefinementDaily
	if tone == domain.ToneMeme {
		category, limit = domain.QuotaMeme, b.config.Limits.MemeDaily
	}
	reservation := quotaReservation{id: updateID, category: category}
	decision, err := b.store.ConsumeQuota(ctx, user.TelegramID, reservation.id, category, limit, b.now())
	if err != nil {
		return err
	}
	if !decision.Allowed {
		b.metrics.Inc("quota_denials")
		return b.sendText(ctx, chatID, quotaText(lang, decision), nil)
	}
	if tone == "" {
		tone = snapshot.Value.Tone
	}
	value := snapshot.Value
	value.FreeModeSwitch = false
	return b.generate(ctx, chatID, user, value, tone, value.Mode, transform, reservation, value.Revision+1)
}

func (b *Service) changeMode(ctx context.Context, updateID, chatID int64, user domain.User, lang language, payload session.CallbackPayload, mode domain.ScenarioMode) error {
	snapshot, ok := b.sessions.Peek(user.TelegramID)
	if !ok || snapshot.Value.Revision != payload.Revision {
		return b.sendText(ctx, chatID, expiredText(lang), nil)
	}
	value := snapshot.Value
	tone := user.DefaultTone
	if mode == domain.ScenarioComment {
		tone = domain.ToneMix
	}
	if value.AwaitingMode {
		if payload.InteractionID != value.SourceID || value.SourceID <= 0 {
			return b.sendText(ctx, chatID, expiredText(lang), nil)
		}
		reservation := quotaReservation{id: value.QuotaReservationID, category: value.QuotaCategory}
		value.AwaitingMode = false
		value.AutoDetected = false
		value.FreeModeSwitch = false
		b.metrics.Inc("mode_choices")
		return b.generate(ctx, chatID, user, value, tone, mode, "mode_selected", reservation, value.Revision+1)
	}
	if value.GenerationID != payload.InteractionID || value.GenerationID <= 0 {
		return b.sendText(ctx, chatID, expiredText(lang), nil)
	}
	if value.Mode == mode {
		return nil
	}
	reservation := quotaReservation{}
	if value.FreeModeSwitch {
		b.metrics.Inc("mode_corrections_free")
	} else {
		reservation = quotaReservation{id: updateID, category: domain.QuotaRefinement}
		decision, err := b.store.ConsumeQuota(ctx, user.TelegramID, reservation.id, reservation.category, b.config.Limits.RefinementDaily, b.now())
		if err != nil {
			return err
		}
		if !decision.Allowed {
			b.metrics.Inc("quota_denials")
			return b.sendText(ctx, chatID, quotaText(lang, decision), nil)
		}
	}
	value.FreeModeSwitch = false
	value.AutoDetected = false
	b.metrics.Inc("mode_switches")
	return b.generate(ctx, chatID, user, value, tone, mode, "mode_switch", reservation, value.Revision+1)
}

func (b *Service) saveStyle(ctx context.Context, chatID int64, user domain.User, lang language, payload session.CallbackPayload) error {
	record, err := b.store.GetGeneration(ctx, payload.InteractionID, user.TelegramID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return b.sendText(ctx, chatID, expiredText(lang), nil)
		}
		return err
	}
	index := int(payload.Candidate)
	if index < 0 || index >= len(record.Result.Replies) {
		return nil
	}
	saved, err := b.store.SaveStyleExample(ctx, user.TelegramID, record.ID, record.Result.Replies[index].Text, b.config.Limits.StyleExamples)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return b.sendText(ctx, chatID, expiredText(lang), nil)
		}
		return err
	}
	if !saved {
		return b.sendText(ctx, chatID, styleLimitText(lang, b.config.Limits.StyleExamples), nil)
	}
	return b.sendText(ctx, chatID, styleSavedText(lang), nil)
}

func (b *Service) generate(ctx context.Context, chatID int64, user domain.User, value interaction, tone domain.Tone, mode domain.ScenarioMode, transform string, reservation quotaReservation, revision uint32) error {
	value.Tone = tone
	value.Mode = mode
	value.Revision = revision
	value.ChatID = chatID
	value.GenerationID = 0
	value.AwaitingMode = false
	lease, err := b.sessions.Begin(ctx, user.TelegramID, value)
	if err != nil {
		b.refundQuota(user.TelegramID, reservation)
		return err
	}
	return b.generateWithLease(ctx, lease, chatID, user, value, transform, reservation)
}

func (b *Service) generateWithLease(ctx context.Context, lease session.Lease, chatID int64, user domain.User, value interaction, transform string, reservation quotaReservation) error {
	stopAction := b.startChatAction(lease.Context, chatID, telegram.ChatActionTyping)
	defer stopAction()

	var examples []string
	var err error
	if value.Mode != domain.ScenarioComment {
		examples, err = b.store.ListStyleExamples(lease.Context, user.TelegramID, b.config.Limits.StyleExamples)
	}
	if err != nil {
		if ctx.Err() != nil {
			b.refundQuota(user.TelegramID, reservation)
			return ctx.Err()
		}
		if !b.sessions.IsCurrent(lease) {
			b.metrics.Inc("obsolete_jobs")
			b.refundQuota(user.TelegramID, reservation)
			return nil
		}
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		return err
	}
	request := domain.GenerationRequest{
		Input: value.Input, Tone: value.Tone, Mode: value.Mode, Transform: transform,
		Language: value.Language, SourceHint: value.SourceHint, StyleExamples: examples,
		PreviousReplies: append([]string(nil), value.PreviousReplies...), VariantSeed: value.Revision,
	}
	if err := validateUpdateLease(lease.Context); err != nil {
		b.refundQuota(user.TelegramID, reservation)
		return err
	}
	started := time.Now()
	providerCtx, cancel := context.WithTimeout(lease.Context, b.config.ProviderTimeout)
	result, err := b.provider.Generate(providerCtx, request)
	cancel()
	b.metrics.Observe("provider_latency", time.Since(started))
	if err != nil {
		if ctx.Err() != nil {
			b.refundQuota(user.TelegramID, reservation)
			return ctx.Err()
		}
		if !b.sessions.IsCurrent(lease) {
			b.metrics.Inc("obsolete_jobs")
			b.refundQuota(user.TelegramID, reservation)
			return nil
		}
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		b.metrics.Inc("provider_errors")
		b.logError("provider generation failed", user.TelegramID, "error", safeErrorCode(err))
		return b.sendText(ctx, chatID, processingErrorText(userLanguage(user.Language)), nil)
	}
	if ctx.Err() != nil {
		b.refundQuota(user.TelegramID, reservation)
		return ctx.Err()
	}
	if !b.sessions.IsCurrent(lease) {
		b.metrics.Inc("obsolete_jobs")
		b.refundQuota(user.TelegramID, reservation)
		return nil
	}
	b.metrics.Add("input_tokens", int64(result.Usage.InputTokens))
	b.metrics.Add("output_tokens", int64(result.Usage.OutputTokens))

	if request.Mode == domain.ScenarioAuto && result.ModeConfidence == domain.ModeConfidenceLow {
		value.Mode = result.Mode
		value.AwaitingMode = true
		value.AutoDetected = true
		value.FreeModeSwitch = false
		value.GenerationID = 0
		value.PreviousReplies = nil
		if err := b.sessions.Commit(lease, value); err != nil {
			if ctx.Err() != nil {
				b.refundQuota(user.TelegramID, reservation)
				return ctx.Err()
			}
			b.metrics.Inc("obsolete_jobs")
			b.refundQuota(user.TelegramID, reservation)
			return nil
		}
		keyboard, keyboardErr := modeChoiceKeyboard(b.callbacks, user.TelegramID, value.SourceID, value.Revision, userLanguage(user.Language))
		if keyboardErr != nil {
			b.sessions.Cancel(user.TelegramID)
			b.refundQuota(user.TelegramID, reservation)
			return keyboardErr
		}
		if err := b.sendText(lease.Context, chatID, modeChoiceText(userLanguage(user.Language)), keyboard); err != nil {
			if errors.Is(err, store.ErrSuperseded) {
				b.refundQuota(user.TelegramID, reservation)
				return err
			}
			b.sessions.Cancel(user.TelegramID)
			b.refundQuota(user.TelegramID, reservation)
			return err
		}
		if persistent, ok := sessionPersistenceContext(lease.Context); ok {
			if _, err := b.sessions.Reparent(lease, persistent); err != nil {
				b.refundQuota(user.TelegramID, reservation)
				return err
			}
		}
		b.metrics.Inc("mode_ambiguous")
		return nil
	}

	if result.Mode == domain.ScenarioComment && value.Tone != domain.ToneMeme {
		value.Tone = domain.ToneMix
	}
	result, report := b.safety.FilterResultForMode(result, result.Mode, value.Tone, value.Language)
	if report.Dropped > 0 || report.Modified > 0 || report.UsedFallback {
		b.metrics.Inc("safety_interventions")
	}
	if len(result.Replies) != 3 {
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		return b.sendText(ctx, chatID, processingErrorText(userLanguage(user.Language)), nil)
	}
	wasAuto := request.Mode == domain.ScenarioAuto
	value.Mode = result.Mode
	value.AwaitingMode = false
	value.AutoDetected = wasAuto
	value.FreeModeSwitch = wasAuto
	value.PreviousReplies = make([]string, 0, len(result.Replies))
	for _, reply := range result.Replies {
		value.PreviousReplies = append(value.PreviousReplies, reply.Text)
	}
	value.QuotaReservationID = 0
	value.QuotaCategory = ""
	record := domain.GenerationRecord{
		TelegramID: user.TelegramID, InputKind: value.Input.Kind, InputDigest: b.inputDigest(value.Input), Tone: value.Tone,
		Provider: result.Provider, Model: result.Model, Result: result,
	}
	generationID, err := b.store.SaveGeneration(lease.Context, record)
	if err != nil {
		if ctx.Err() != nil {
			b.refundQuota(user.TelegramID, reservation)
			return ctx.Err()
		}
		if !b.sessions.IsCurrent(lease) {
			b.metrics.Inc("obsolete_jobs")
			b.refundQuota(user.TelegramID, reservation)
			return nil
		}
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		return fmt.Errorf("save generation: %w", err)
	}
	value.GenerationID = generationID
	if err := b.sessions.Commit(lease, value); err != nil {
		if ctx.Err() != nil {
			b.refundQuota(user.TelegramID, reservation)
			return ctx.Err()
		}
		b.metrics.Inc("obsolete_jobs")
		b.refundQuota(user.TelegramID, reservation)
		return nil
	}
	keyboard, err := resultKeyboard(b.callbacks, user.TelegramID, generationID, value.Revision, result.Replies, result.Mode, userLanguage(user.Language))
	if err != nil {
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		return err
	}
	if ctx.Err() != nil {
		b.refundQuota(user.TelegramID, reservation)
		return ctx.Err()
	}
	if !b.sessions.IsCurrent(lease) {
		b.metrics.Inc("obsolete_jobs")
		b.refundQuota(user.TelegramID, reservation)
		return nil
	}
	err = b.deliverResult(lease.Context, chatID, userLanguage(user.Language), result, keyboard, value.Tone)
	if err != nil {
		if errors.Is(err, store.ErrSuperseded) {
			b.refundQuota(user.TelegramID, reservation)
			return err
		}
		if ctx.Err() != nil {
			b.refundQuota(user.TelegramID, reservation)
			return ctx.Err()
		}
		if !b.sessions.IsCurrent(lease) {
			b.metrics.Inc("obsolete_jobs")
			b.refundQuota(user.TelegramID, reservation)
			return nil
		}
		b.sessions.Cancel(user.TelegramID)
		b.refundQuota(user.TelegramID, reservation)
		return err
	}
	if persistent, ok := sessionPersistenceContext(lease.Context); ok {
		if _, err := b.sessions.Reparent(lease, persistent); err != nil {
			return err
		}
	}
	b.metrics.Inc("generations_succeeded")
	if result.Mode == domain.ScenarioComment {
		b.metrics.Inc("generations_comment")
	} else {
		b.metrics.Inc("generations_reply")
	}
	return nil
}

func (b *Service) deliverResult(ctx context.Context, chatID int64, lang language, result domain.GenerationResult, keyboard *telegram.InlineKeyboardMarkup, tone domain.Tone) error {
	if err := validateUpdateLease(ctx); err != nil {
		return err
	}
	if tone == domain.ToneMeme && result.Meme != nil && b.renderer != nil {
		pngData, err := b.renderer.Render(*result.Meme)
		if err == nil {
			_, err = b.telegram.SendPhoto(ctx, telegram.SendPhotoParams{
				ChatID: chatID, Photo: telegram.FileUpload("witty-reply.png", pngData), Caption: resultsText(lang, result), ReplyMarkup: keyboard,
			})
			if err == nil {
				b.metrics.Inc("memes_rendered")
				return nil
			}
			b.metrics.Inc("meme_send_errors")
			// Preserve the useful text result when Telegram rejects or cannot
			// receive the rendered upload.
			return b.sendText(ctx, chatID, resultsText(lang, result), keyboard)
		}
		b.metrics.Inc("meme_render_errors")
	}
	return b.sendText(ctx, chatID, resultsText(lang, result), keyboard)
}

type rawInput struct {
	kind       domain.InputKind
	text       string
	mode       domain.ScenarioMode
	sourceHint string
	fileID     string
	mediaType  string
	filename   string
	fileSize   int64
	duration   int
}

type quotaReservation struct {
	id       int64
	category domain.QuotaCategory
}

func (b *Service) classifyInput(message telegram.Message) (rawInput, error) {
	if text := strings.TrimSpace(message.Text); text != "" {
		mode, source := domain.ScenarioAuto, text
		if message.ForwardOrigin == nil {
			mode, source = extractScenarioDirective(text, false)
		}
		if utf8.RuneCountInString(source) > b.config.Limits.MaxTextRunes {
			return rawInput{}, errors.New("text too long")
		}
		return rawInput{kind: domain.InputText, text: source, mode: mode, sourceHint: sourceHint(message, domain.InputText)}, nil
	}
	if photo, ok := message.LargestPhoto(); ok {
		mode, caption := domain.ScenarioAuto, strings.TrimSpace(message.Caption)
		if message.ForwardOrigin == nil {
			mode, caption = extractScenarioDirective(message.Caption, true)
		}
		return rawInput{kind: domain.InputImage, text: caption, mode: mode, sourceHint: sourceHint(message, domain.InputImage), fileID: photo.FileID, mediaType: "image/jpeg", fileSize: photo.FileSize}, nil
	}
	if message.Document != nil && isImageDocument(*message.Document) {
		document := message.Document
		mode, caption := domain.ScenarioAuto, strings.TrimSpace(message.Caption)
		if message.ForwardOrigin == nil {
			mode, caption = extractScenarioDirective(message.Caption, true)
		}
		return rawInput{kind: domain.InputImage, text: caption, mode: mode, sourceHint: sourceHint(message, domain.InputImage), fileID: document.FileID, mediaType: document.MIMEType, filename: document.FileName, fileSize: document.FileSize}, nil
	}
	if message.Voice != nil {
		voice := message.Voice
		if voice.Duration > 5*60 {
			return rawInput{}, errors.New("voice too long")
		}
		return rawInput{kind: domain.InputVoice, mode: domain.ScenarioAuto, sourceHint: sourceHint(message, domain.InputVoice), fileID: voice.FileID, mediaType: voice.MIMEType, filename: "voice.ogg", fileSize: voice.FileSize, duration: voice.Duration}, nil
	}
	return rawInput{}, errors.New("unsupported input")
}

func (b *Service) materializeInput(ctx context.Context, raw rawInput, uiLanguage string) (domain.Input, string, error) {
	switch raw.kind {
	case domain.InputText:
		return domain.Input{Kind: domain.InputText, Text: raw.text}, ai.DetectSourceLanguage(raw.text, uiLanguage), nil
	case domain.InputImage:
		if raw.fileSize > int64(b.config.Limits.MaxImageBytes) {
			return domain.Input{}, "", errors.New("image too large")
		}
		download, err := b.telegram.DownloadFileLimit(ctx, raw.fileID, int64(b.config.Limits.MaxImageBytes))
		if err != nil {
			return domain.Input{}, "", err
		}
		normalized, err := media.NormalizeImage(download.Data, media.ImageConfig{
			MaxInputBytes: b.config.Limits.MaxImageBytes, MaxOutputBytes: b.config.Limits.MaxImageBytes,
		})
		if err != nil {
			return domain.Input{}, "", err
		}
		return domain.Input{Kind: domain.InputImage, Text: raw.text, Image: normalized.Data, MediaType: normalized.MediaType}, uiLanguage, nil
	case domain.InputVoice:
		if raw.fileSize > int64(b.config.Limits.MaxVoiceBytes) {
			return domain.Input{}, "", errors.New("voice too large")
		}
		download, err := b.telegram.DownloadFileLimit(ctx, raw.fileID, int64(b.config.Limits.MaxVoiceBytes))
		if err != nil {
			return domain.Input{}, "", err
		}
		result, err := b.transcriber.Transcribe(ctx, transcribe.Audio{
			Data: download.Data, Filename: fallbackString(raw.filename, "voice.ogg"), MediaType: fallbackString(raw.mediaType, "audio/ogg"),
		})
		if err != nil {
			return domain.Input{}, "", err
		}
		if result.Confidence > 0 && result.Confidence < 0.55 {
			return domain.Input{}, "", errLowTranscriptionConfidence
		}
		languageFallback := result.Language
		if strings.TrimSpace(languageFallback) == "" {
			languageFallback = uiLanguage
		}
		return domain.Input{Kind: domain.InputVoice, Text: result.Text}, ai.DetectSourceLanguage(result.Text, languageFallback), nil
	default:
		return domain.Input{}, "", errors.New("unsupported input")
	}
}

func (b *Service) upsertUser(ctx context.Context, telegramUser telegram.User) (domain.User, error) {
	user, err := b.store.UpsertUser(ctx, domain.User{
		TelegramID: telegramUser.ID, Language: fallbackString(telegramUser.LanguageCode, "ru"), DefaultTone: domain.ToneMix,
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("upsert user: %w", err)
	}
	return user, nil
}

func (b *Service) inputQuota(kind domain.InputKind) (domain.QuotaCategory, int) {
	if kind == domain.InputText {
		return domain.QuotaText, b.config.Limits.TextDaily
	}
	return domain.QuotaMedia, b.config.Limits.MediaDaily
}

func (b *Service) sendText(ctx context.Context, chatID int64, text string, keyboard *telegram.InlineKeyboardMarkup) error {
	if err := validateUpdateLease(ctx); err != nil {
		return err
	}
	_, err := b.telegram.SendMessage(ctx, telegram.SendMessageParams{ChatID: chatID, Text: text, ReplyMarkup: keyboard})
	if err != nil {
		b.metrics.Inc("telegram_send_errors")
	}
	return err
}

func (b *Service) startChatAction(ctx context.Context, chatID int64, action telegram.ChatAction) func() {
	actionCtx, cancel := context.WithCancel(ctx)
	b.chatActions.Add(1)
	go func() {
		defer b.chatActions.Done()
		send := func() {
			requestCtx, requestCancel := context.WithTimeout(actionCtx, 2*time.Second)
			defer requestCancel()
			_ = b.telegram.SendChatAction(requestCtx, telegram.SendChatActionParams{ChatID: chatID, Action: action})
		}
		send()
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-actionCtx.Done():
				return
			case <-ticker.C:
				send()
			}
		}
	}()
	return cancel
}

func (b *Service) now() time.Time { return time.Now().In(b.config.UsageLocation) }

func (b *Service) refundQuota(telegramID int64, reservation quotaReservation) {
	if reservation.id <= 0 || reservation.category == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.store.RefundQuota(ctx, telegramID, reservation.id, reservation.category); err != nil {
		b.metrics.Inc("quota_refund_errors")
		b.logError("quota refund failed", telegramID, "error", safeErrorCode(err))
	}
}

func (b *Service) logError(message string, telegramID int64, args ...any) {
	base := []any{"user", observability.UserHash(b.config.CallbackSecret, telegramID)}
	b.logger.Error(message, append(base, args...)...)
}

func (b *Service) inputDigest(input domain.Input) string {
	hash := hmac.New(sha256.New, []byte(b.config.CallbackSecret))
	_, _ = hash.Write([]byte(input.Kind))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(input.Text))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(input.Image)
	return hex.EncodeToString(hash.Sum(nil))
}

func isImageDocument(document telegram.Document) bool {
	mediaType := strings.ToLower(strings.TrimSpace(document.MIMEType))
	if strings.HasPrefix(mediaType, "image/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(document.FileName)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return true
	default:
		return false
	}
}

func toneForAction(action session.Action) domain.Tone {
	switch action {
	case session.ActionToneSmart:
		return domain.ToneSmart
	case session.ActionTonePlayful:
		return domain.TonePlayful
	case session.ActionToneSharp:
		return domain.ToneSharp
	case session.ActionToneBoundary:
		return domain.ToneBoundary
	default:
		return domain.ToneMix
	}
}

func parseCommand(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return ""
	}
	command := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}
	return command
}

func nextDay(now time.Time) time.Time {
	year, month, day := now.Date()
	return time.Date(year, month, day+1, 0, 0, 0, 0, now.Location())
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func safeErrorCode(err error) string {
	var providerErr *ai.ProviderError
	if errors.As(err, &providerErr) && providerErr.Code != "" {
		return providerErr.Code
	}
	return fmt.Sprintf("%T", err)
}
