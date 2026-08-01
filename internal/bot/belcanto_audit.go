package bot

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"
	"unicode"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/observability"
)

const (
	maxBelcantoAuditCandidates      = 20
	maxBelcantoAuditRequestIDs      = 3
	maxBelcantoAuditQualityFlags    = 16
	maxBelcantoAuditIdentifierRunes = 64
	maxBelcantoAuditProviderRunes   = 160
	maxBelcantoAuditReasonRunes     = 240
	maxBelcantoAuditReviewNoteRunes = 160
	maxBelcantoAuditPhotoQueryRunes = 100
	maxBelcantoAuditPhotoBriefRunes = 240
	maxBelcantoAuditPhotoPageRunes  = 512
	maxBelcantoAuditPhotoAssetRunes = 80
	maxBelcantoAuditValidationRunes = 128
	maxBelcantoAuditRequestIDRunes  = 128
)

type belcantoEditorialAuditLog struct {
	GenerationID         string                     `json:"generation_id"`
	DraftID              int64                      `json:"draft_id"`
	User                 string                     `json:"user"`
	Voice                string                     `json:"voice"`
	Transform            string                     `json:"transform"`
	RecipeID             string                     `json:"recipe_id"`
	ExplorationGoal      int                        `json:"exploration_goal"`
	GenerationCalls      int                        `json:"generation_calls"`
	ReviewCalls          int                        `json:"review_calls"`
	GeneratorProvider    string                     `json:"generator_provider"`
	GeneratorModel       string                     `json:"generator_model"`
	GenerationRequestIDs []string                   `json:"generation_request_ids,omitempty"`
	ReviewerProvider     string                     `json:"reviewer_provider"`
	ReviewerModel        string                     `json:"reviewer_model"`
	ReviewerRequestID    string                     `json:"reviewer_request_id,omitempty"`
	ReviewerWinnerID     string                     `json:"reviewer_winner_id"`
	SelectedWinnerID     string                     `json:"selected_winner_id"`
	PreviewStatus        string                     `json:"preview_status"`
	SelectionMode        string                     `json:"selection_mode"`
	DecisionReason       string                     `json:"decision_reason,omitempty"`
	ReviewerError        string                     `json:"reviewer_error,omitempty"`
	WinnerTextSHA256     string                     `json:"winner_text_sha256"`
	VisualMode           string                     `json:"visual_mode"`
	PhotoAttached        bool                       `json:"photo_attached"`
	PhotoSource          string                     `json:"photo_source,omitempty"`
	PhotoAssetID         string                     `json:"photo_asset_id,omitempty"`
	PhotoSourcePage      string                     `json:"photo_source_page,omitempty"`
	PhotoAuthor          string                     `json:"photo_author,omitempty"`
	PhotoQuery           string                     `json:"photo_query,omitempty"`
	PhotoBrief           string                     `json:"photo_brief,omitempty"`
	InputTokens          int                        `json:"input_tokens"`
	OutputTokens         int                        `json:"output_tokens"`
	Finalists            []belcantoFinalistAuditLog `json:"finalists"`
	RejectedCandidates   []belcantoRejectedAuditLog `json:"rejected_candidates,omitempty"`
}

type belcantoFinalistAuditLog struct {
	Rank           int                       `json:"rank,omitempty"`
	Attempt        int                       `json:"attempt"`
	SourceSlot     string                    `json:"source_slot"`
	ID             string                    `json:"id,omitempty"`
	Goal           string                    `json:"goal,omitempty"`
	Eligible       bool                      `json:"eligible"`
	Considered     bool                      `json:"considered"`
	ValidationCode string                    `json:"validation_code,omitempty"`
	Runes          int                       `json:"runes,omitempty"`
	LocalScore     int                       `json:"local_score,omitempty"`
	QualityFlags   []string                  `json:"quality_flags,omitempty"`
	Review         *belcantoReviewerScoreLog `json:"review,omitempty"`
	Selected       bool                      `json:"selected"`
	DeliverySafe   bool                      `json:"delivery_safe"`
	TextSHA256     string                    `json:"text_sha256,omitempty"`
	Text           string                    `json:"text,omitempty"`
}

type belcantoRejectedAuditLog struct {
	Attempt        int    `json:"attempt"`
	SourceSlot     string `json:"source_slot"`
	Eligible       bool   `json:"eligible"`
	ValidationCode string `json:"validation_code"`
	TextSHA256     string `json:"text_sha256,omitempty"`
}

type belcantoReviewerScoreLog struct {
	Hook         int    `json:"hook"`
	Human        int    `json:"human"`
	Recognition  int    `json:"recognition"`
	Replies      int    `json:"replies"`
	Brevity      int    `json:"brevity"`
	Voice        int    `json:"voice"`
	Total        int    `json:"total"`
	WouldLike    string `json:"would_like"`
	WouldComment string `json:"would_comment"`
	Note         string `json:"note,omitempty"`
}

type belcantoMediaChangeAuditLog struct {
	DraftID      int64  `json:"draft_id"`
	Revision     uint32 `json:"revision"`
	User         string `json:"user"`
	Action       string `json:"action"`
	PhotoSource  string `json:"photo_source,omitempty"`
	PhotoAssetID string `json:"photo_asset_id,omitempty"`
	PhotoQuery   string `json:"photo_query,omitempty"`
}

func (b *Service) logBelcantoMediaChanged(
	telegramID int64,
	draft domain.ThreadDraft,
	mediaValue *domain.ThreadMedia,
	action string,
) {
	mode := b.config.BelcantoReviewLogMode
	if mode == "off" {
		return
	}
	entry := belcantoMediaChangeAuditLog{
		DraftID: draft.ID, Revision: draft.Revision,
		User:   observability.UserHash(b.config.CallbackSecret, telegramID),
		Action: boundedBelcantoAuditMetadata(action, maxBelcantoAuditIdentifierRunes),
	}
	if mediaValue != nil {
		entry.PhotoSource = boundedBelcantoAuditMetadata(string(mediaValue.EffectiveSourceKind()), maxBelcantoAuditIdentifierRunes)
	}
	if mode == "full" && mediaValue != nil {
		entry.PhotoAssetID = boundedBelcantoAuditMetadata(mediaValue.SourceAssetID, maxBelcantoAuditPhotoAssetRunes)
		entry.PhotoQuery = boundedBelcantoAuditMetadata(mediaValue.SourceQuery, maxBelcantoAuditPhotoQueryRunes)
	}
	b.logger.Info(
		"Belcanto Threads media changed",
		"event", "belcanto_threads_media_changed",
		"schema_version", 1,
		slog.Any("audit", entry),
	)
}

func (b *Service) logBelcantoEditorialAudit(
	telegramID int64,
	draft domain.ThreadDraft,
	transform string,
	result ai.ThreadPostResult,
	previewMedia *domain.ThreadMedia,
	previewStatus string,
) {
	mode := b.config.BelcantoReviewLogMode
	if mode == "off" {
		return
	}
	full := mode == "full"
	audit := result.Audit
	entry := belcantoEditorialAuditLog{
		GenerationID: boundedBelcantoAuditMetadata(audit.GenerationID, maxBelcantoAuditIdentifierRunes), DraftID: draft.ID,
		User:            observability.UserHash(b.config.CallbackSecret, telegramID),
		Voice:           boundedBelcantoAuditMetadata(string(draft.Voice), maxBelcantoAuditIdentifierRunes),
		Transform:       boundedBelcantoAuditMetadata(transform, maxBelcantoAuditIdentifierRunes),
		RecipeID:        boundedBelcantoAuditMetadata(audit.RecipeID, maxBelcantoAuditIdentifierRunes),
		ExplorationGoal: audit.ExplorationGoal, GenerationCalls: audit.GenerationCalls, ReviewCalls: audit.ReviewCalls,
		GeneratorProvider: boundedBelcantoAuditMetadata(audit.GeneratorProvider, maxBelcantoAuditProviderRunes),
		GeneratorModel:    boundedBelcantoAuditMetadata(audit.GeneratorModel, maxBelcantoAuditProviderRunes),
		GenerationRequestIDs: boundedBelcantoAuditMetadataSlice(
			audit.GenerationRequestIDs, maxBelcantoAuditRequestIDs, maxBelcantoAuditRequestIDRunes,
		),
		ReviewerProvider: boundedBelcantoAuditMetadata(audit.ReviewerProvider, maxBelcantoAuditProviderRunes),
		ReviewerModel:    boundedBelcantoAuditMetadata(audit.ReviewerModel, maxBelcantoAuditProviderRunes),
		ReviewerRequestID: boundedBelcantoAuditMetadata(
			audit.ReviewerRequestID, maxBelcantoAuditRequestIDRunes,
		),
		ReviewerWinnerID: boundedBelcantoAuditMetadata(audit.ReviewerWinnerID, maxBelcantoAuditIdentifierRunes),
		SelectedWinnerID: boundedBelcantoAuditMetadata(audit.DeliveredWinnerID, maxBelcantoAuditIdentifierRunes),
		PreviewStatus:    boundedBelcantoAuditMetadata(previewStatus, maxBelcantoAuditIdentifierRunes),
		SelectionMode:    boundedBelcantoAuditMetadata(audit.SelectionMode, maxBelcantoAuditIdentifierRunes),
		ReviewerError:    boundedBelcantoAuditMetadata(audit.ReviewerError, maxBelcantoAuditValidationRunes),
		WinnerTextSHA256: threadAuditDigest(draft.Text),
		VisualMode:       boundedBelcantoAuditMetadata(result.Visual.Mode, maxBelcantoAuditIdentifierRunes),
		PhotoAttached:    draft.MediaMode == domain.ThreadMediaImage,
		InputTokens:      result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
		Finalists:          make([]belcantoFinalistAuditLog, 0, len(audit.Candidates)),
		RejectedCandidates: make([]belcantoRejectedAuditLog, 0, len(audit.Candidates)),
	}
	if previewMedia != nil && draft.MediaMode == domain.ThreadMediaImage {
		entry.PhotoSource = boundedBelcantoAuditMetadata(
			string(previewMedia.EffectiveSourceKind()), maxBelcantoAuditIdentifierRunes,
		)
	}
	if full {
		entry.DecisionReason = boundedBelcantoAuditMetadata(audit.DecisionReason, maxBelcantoAuditReasonRunes)
		entry.PhotoQuery = boundedBelcantoAuditMetadata(result.Visual.Query, maxBelcantoAuditPhotoQueryRunes)
		entry.PhotoBrief = boundedBelcantoAuditMetadata(result.Visual.Brief, maxBelcantoAuditPhotoBriefRunes)
		if previewMedia != nil && previewMedia.EffectiveSourceKind() == domain.ThreadMediaSourcePexels {
			entry.PhotoAssetID = boundedBelcantoAuditMetadata(previewMedia.SourceAssetID, maxBelcantoAuditPhotoAssetRunes)
			entry.PhotoSourcePage = boundedBelcantoAuditMetadata(previewMedia.SourcePageURL, maxBelcantoAuditPhotoPageRunes)
			entry.PhotoAuthor = boundedBelcantoAuditMetadata(previewMedia.SourceAuthor, maxBelcantoAuditProviderRunes)
		}
	}
	candidates := append([]ai.ThreadPostCandidateAudit(nil), audit.Candidates...)
	sort.SliceStable(candidates, func(left, right int) bool {
		leftFinalist := candidates[left].Eligible && candidates[left].Considered
		rightFinalist := candidates[right].Eligible && candidates[right].Considered
		if leftFinalist != rightFinalist {
			return leftFinalist
		}
		if leftFinalist {
			if candidates[left].Review.Total > 0 || candidates[right].Review.Total > 0 {
				return belcantoAuditReviewBetter(candidates[left], candidates[right])
			}
			if candidates[left].Local.Score != candidates[right].Local.Score {
				return candidates[left].Local.Score > candidates[right].Local.Score
			}
			if belcantoAuditLengthDistance(candidates[left].Local.RuneCount) != belcantoAuditLengthDistance(candidates[right].Local.RuneCount) {
				return belcantoAuditLengthDistance(candidates[left].Local.RuneCount) < belcantoAuditLengthDistance(candidates[right].Local.RuneCount)
			}
			return candidates[left].ReviewerID < candidates[right].ReviewerID
		}
		if candidates[left].Attempt != candidates[right].Attempt {
			return candidates[left].Attempt < candidates[right].Attempt
		}
		return candidates[left].SourceSlot < candidates[right].SourceSlot
	})
	if len(candidates) > maxBelcantoAuditCandidates {
		candidates = candidates[:maxBelcantoAuditCandidates]
	}
	rank := 0
	for _, candidate := range candidates {
		if !candidate.Eligible || !candidate.Considered {
			code := candidate.ValidationCode
			if code == "" {
				code = "not_selected_for_review"
			}
			rejected := belcantoRejectedAuditLog{
				Attempt:        candidate.Attempt,
				SourceSlot:     boundedBelcantoAuditMetadata(candidate.SourceSlot, maxBelcantoAuditIdentifierRunes),
				Eligible:       candidate.Eligible,
				ValidationCode: boundedBelcantoAuditMetadata(code, maxBelcantoAuditValidationRunes),
			}
			if candidate.Text != "" {
				rejected.TextSHA256 = threadAuditDigest(candidate.Text)
			}
			entry.RejectedCandidates = append(entry.RejectedCandidates, rejected)
			continue
		}
		item := belcantoFinalistAuditLog{
			Attempt:    candidate.Attempt,
			SourceSlot: boundedBelcantoAuditMetadata(candidate.SourceSlot, maxBelcantoAuditIdentifierRunes),
			ID:         boundedBelcantoAuditMetadata(candidate.ReviewerID, maxBelcantoAuditIdentifierRunes),
			Goal:       boundedBelcantoAuditMetadata(candidate.Goal, maxBelcantoAuditIdentifierRunes),
			Eligible:   candidate.Eligible, Considered: candidate.Considered,
			ValidationCode: boundedBelcantoAuditMetadata(candidate.ValidationCode, maxBelcantoAuditValidationRunes),
			Runes:          candidate.Local.RuneCount,
			LocalScore:     candidate.Local.Score,
			QualityFlags: boundedBelcantoAuditMetadataSlice(
				candidate.Local.Flags, maxBelcantoAuditQualityFlags, maxBelcantoAuditIdentifierRunes,
			),
			Selected: candidate.Selected,
		}
		rank++
		item.Rank = rank
		item.DeliverySafe = b.threadPostDeliverySafe(candidate.Text)
		if item.DeliverySafe {
			item.TextSHA256 = threadAuditDigest(candidate.Text)
		} else if candidate.Text != "" {
			item.TextSHA256 = threadAuditDigest(candidate.Text)
		}
		if candidate.Review.Total > 0 {
			review := candidate.Review
			item.Review = &belcantoReviewerScoreLog{
				Hook: review.Hook, Human: review.Human, Recognition: review.Recognition,
				Replies: review.Replies, Brevity: review.Brevity, Voice: review.Voice,
				Total:        review.Total,
				WouldLike:    boundedBelcantoAuditMetadata(review.WouldLike, maxBelcantoAuditIdentifierRunes),
				WouldComment: boundedBelcantoAuditMetadata(review.WouldComment, maxBelcantoAuditIdentifierRunes),
			}
			if full {
				item.Review.Note = boundedBelcantoAuditMetadata(review.Note, maxBelcantoAuditReviewNoteRunes)
			}
		}
		// Full text is intentionally limited to candidates that passed both the
		// editorial validator and final delivery moderation. All other model
		// output is represented only by a safe code and digest.
		if full && item.DeliverySafe {
			item.Text = candidate.Text
		}
		entry.Finalists = append(entry.Finalists, item)
	}
	b.logger.Info(
		"Belcanto Threads finalists reviewed",
		"event", "belcanto_threads_editorial_review",
		"schema_version", 2,
		slog.Any("audit", entry),
	)
}

func belcantoAuditReviewBetter(left, right ai.ThreadPostCandidateAudit) bool {
	if left.Review.Total != right.Review.Total {
		return left.Review.Total > right.Review.Total
	}
	if belcantoAuditIntentRank(left.Review.WouldComment) != belcantoAuditIntentRank(right.Review.WouldComment) {
		return belcantoAuditIntentRank(left.Review.WouldComment) > belcantoAuditIntentRank(right.Review.WouldComment)
	}
	if belcantoAuditIntentRank(left.Review.WouldLike) != belcantoAuditIntentRank(right.Review.WouldLike) {
		return belcantoAuditIntentRank(left.Review.WouldLike) > belcantoAuditIntentRank(right.Review.WouldLike)
	}
	if left.Review.Replies != right.Review.Replies {
		return left.Review.Replies > right.Review.Replies
	}
	if left.Review.Human != right.Review.Human {
		return left.Review.Human > right.Review.Human
	}
	if left.Review.Hook != right.Review.Hook {
		return left.Review.Hook > right.Review.Hook
	}
	return left.ReviewerID < right.ReviewerID
}

func belcantoAuditIntentRank(value string) int {
	switch value {
	case "yes":
		return 2
	case "maybe":
		return 1
	default:
		return 0
	}
}

func belcantoAuditLengthDistance(runes int) int {
	const target = 190
	if runes > target {
		return runes - target
	}
	return target - runes
}

func boundedBelcantoAuditMetadata(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

func boundedBelcantoAuditMetadataSlice(values []string, maxItems, maxRunes int) []string {
	if maxItems <= 0 || len(values) == 0 {
		return nil
	}
	if len(values) > maxItems {
		values = values[:maxItems]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if bounded := boundedBelcantoAuditMetadata(value, maxRunes); bounded != "" {
			result = append(result, bounded)
		}
	}
	return result
}

func threadAuditDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
