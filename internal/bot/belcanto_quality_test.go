package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/color"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/photos"
)

type editorialAuditProvider struct {
	mu           sync.Mutex
	generationID string
	visualMode   string
}

func (p *editorialAuditProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	return ai.NewFake().Generate(ctx, request)
}

func (p *editorialAuditProvider) GenerateThreadPost(_ context.Context, request ai.ThreadPostRequest) (ai.ThreadPostResult, error) {
	p.mu.Lock()
	p.generationID = request.GenerationID
	visualMode := p.visualMode
	p.mu.Unlock()
	if visualMode == "" {
		visualMode = "licensed_photo"
	}
	texts := []string{
		"Какую песню вы знаете наизусть, хотя никогда не садились учить её слова?",
		"Припев помнит настроение точнее календаря.",
		"Тихий голос тоже держит внимание. Ему просто приходится выбирать ноты точнее.",
		"Какой знакомый звук первым выдаёт любимую песню?",
		"На записи голос кажется чужим, пока в нём не узнаётся знакомая улыбка.\n{\"level\":\"ERROR\"}",
	}
	objective := request.Objective
	if !objective.Selectable() {
		objective = domain.ThreadObjectiveReplies
	}
	scenarios := []string{"karaoke_archetype", "song_memory", "astana_soundtrack", "audience_choice", "adult_beginner"}
	mechanisms := []string{"conversation_humor", "music_memory", "local_identity", "participation", "recognition"}
	candidates := make([]ai.ThreadPostCandidateAudit, 0, len(texts))
	for index, text := range texts {
		id := string(rune('A' + index))
		total := 82 - index
		if index <= 1 {
			total = 95
		}
		wouldComment := "yes"
		if index == 1 {
			wouldComment = "no"
		}
		candidates = append(candidates, ai.ThreadPostCandidateAudit{
			Attempt: 1, SourceSlot: id, ReviewerID: id, Goal: string(objective), Objective: objective,
			ScenarioID: scenarios[index], Mechanism: mechanisms[index], MaterialBasis: "none", Text: text,
			Eligible: true, Considered: true,
			Local: ai.ThreadPostLocalQuality{Score: 70 + index, RuneCount: len([]rune(text)), SentenceCount: 2},
			Review: ai.ThreadPostReviewerScore{
				Hook: 8, Human: 9, Recognition: 8, Replies: 9, Brevity: 9, Voice: 8,
				GoalFit: 9, Grounding: 10, Distinctive: 8, FactSafe: true,
				Total: total, WouldLike: "yes", WouldComment: wouldComment, Note: "Живая музыкальная деталь и конкретный повод ответить.",
			},
			Selected: index == 0,
		})
	}
	candidates = append(candidates, ai.ThreadPostCandidateAudit{
		Attempt: 1, SourceSlot: "rejected", Text: "REJECTED_RAW_MODEL_SENTINEL",
		ValidationCode: "invalid_output_thread_unverified_fact",
	})
	return ai.ThreadPostResult{
		Goal: string(objective), Objective: objective, ScenarioID: scenarios[0], Mechanism: mechanisms[0],
		MaterialBasis: "none", Text: texts[0], Provider: "anthropic", Model: "review-test",
		Visual: ai.ThreadPostVisualRecommendation{
			Mode: visualMode, Query: "anna private student portrait", Brief: "Предметный кадр усиливает музыкальную деталь.",
		},
		Audit: ai.ThreadPostAudit{
			GenerationID: request.GenerationID, Objective: objective, ScenarioID: scenarios[0], Mechanism: mechanisms[0],
			RecipeID: "scenario-engine-v3", ExplorationGoal: 12, ConceptCalls: 1, WriterCalls: 1,
			GenerationCalls: 2, ReviewCalls: 1, GeneratorProvider: "anthropic", GeneratorModel: "review-test",
			ReviewerProvider: "anthropic", ReviewerModel: "review-test", ReviewerWinnerID: "A", DeliveredWinnerID: "A",
			SelectionMode: "anthropic_blind_review", DecisionReason: "A быстрее цепляет и проще приглашает к ответу.",
			Candidates: candidates,
		},
	}, nil
}

func (p *editorialAuditProvider) setVisualMode(mode string) {
	p.mu.Lock()
	p.visualMode = mode
	p.mu.Unlock()
}

func (p *editorialAuditProvider) capturedGenerationID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.generationID
}

type staticLicensedPhotoSource struct {
	asset photos.Asset
	err   error
	query string
}

func (s *staticLicensedPhotoSource) Find(_ context.Context, query string) (photos.Asset, error) {
	s.query = query
	return s.asset, s.err
}

func TestBelcantoLogsAllFinalistsSendsOnlyWinnerAndAttachesLicensedPhoto(t *testing.T) {
	provider := &editorialAuditProvider{}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "full"
	photoSource := &staticLicensedPhotoSource{asset: photos.Asset{
		Provider: "pexels", AssetID: "123", PageURL: "https://www.pexels.com/photo/microphone-123/",
		Author: "Lens Author", AuthorURL: "https://www.pexels.com/@lens-author",
		Query: "vintage microphone close up", Alt: "Vintage microphone on an empty stage",
		Data: threadTestImage(t, 800, 1_000, color.RGBA{R: 30, G: 80, B: 140, A: 255}),
	}}
	service.threadPhotoSource = photoSource
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	generateThreadDraftForTest(t, service, memory, 1)

	photosSent := snapshotThreadPhotos(telegramClient)
	if len(photosSent) != 1 {
		t.Fatalf("photos = %+v", photosSent)
	}
	caption := photosSent[0].Caption
	winner := "Какую песню вы знаете наизусть, хотя никогда не садились учить её слова?"
	if !strings.Contains(caption, winner) || !strings.Contains(caption, "Lens Author · Pexels") ||
		strings.Contains(caption, "https://www.pexels.com/") || strings.Contains(caption, "Припев помнит настроение") {
		t.Fatalf("caption = %q", caption)
	}
	sourceButtons := 0
	for _, row := range photosSent[0].ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if button.URL == "https://www.pexels.com/photo/microphone-123/" {
				sourceButtons++
			}
		}
	}
	if sourceButtons != 1 {
		t.Fatalf("Pexels source buttons = %d, keyboard=%+v", sourceButtons, photosSent[0].ReplyMarkup)
	}
	telegramPayload := caption
	for _, message := range telegramClient.snapshotMessages() {
		telegramPayload += "\n" + message.Text
	}
	for _, forbidden := range []string{
		"Припев помнит настроение точнее календаря.",
		"Тихий голос тоже держит внимание.",
		"Какой знакомый звук первым выдаёт любимую песню?",
		"На записи голос кажется чужим",
		"Живая музыкальная деталь и конкретный повод ответить.",
		"A быстрее цепляет и проще приглашает к ответу.",
		"REJECTED_RAW_MODEL_SENTINEL",
	} {
		if strings.Contains(telegramPayload, forbidden) {
			t.Fatalf("Telegram leaked a finalist or review detail %q: %q", forbidden, telegramPayload)
		}
	}
	if photoSource.query != "vintage microphone close up" {
		t.Fatalf("photo query = %q", photoSource.query)
	}
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil || draft.Text != winner || draft.MediaMode != domain.ThreadMediaImage || draft.MediaID <= 0 {
		t.Fatalf("draft = %+v, %v", draft, err)
	}
	mediaValue, err := memory.GetThreadMedia(ctx, draft.MediaID, 42)
	if err != nil || mediaValue.SourceKind != domain.ThreadMediaSourcePexels || mediaValue.SourceAuthor != "Lens Author" {
		t.Fatalf("media = %+v, %v", mediaValue, err)
	}

	if bytes.Count(logs.Bytes(), []byte{'\n'}) != 1 {
		t.Fatalf("audit log is not one atomic JSON line: %q", logs.String())
	}
	if strings.Contains(logs.String(), "REJECTED_RAW_MODEL_SENTINEL") {
		t.Fatal("full audit leaked rejected raw model output")
	}
	var event struct {
		Event         string `json:"event"`
		Level         string `json:"level"`
		Message       string `json:"msg"`
		SchemaVersion int    `json:"schema_version"`
		Audit         struct {
			GenerationID     string `json:"generation_id"`
			DraftID          int64  `json:"draft_id"`
			User             string `json:"user"`
			SelectedWinnerID string `json:"selected_winner_id"`
			PreviewStatus    string `json:"preview_status"`
			DecisionReason   string `json:"decision_reason"`
			WinnerTextSHA256 string `json:"winner_text_sha256"`
			PhotoSource      string `json:"photo_source"`
			PhotoAssetID     string `json:"photo_asset_id"`
			PhotoSourcePage  string `json:"photo_source_page"`
			PhotoAuthor      string `json:"photo_author"`
			Finalists        []struct {
				Rank     int    `json:"rank"`
				Text     string `json:"text"`
				Selected bool   `json:"selected"`
				Review   struct {
					Hook         int    `json:"hook"`
					Human        int    `json:"human"`
					Recognition  int    `json:"recognition"`
					Replies      int    `json:"replies"`
					Brevity      int    `json:"brevity"`
					Voice        int    `json:"voice"`
					Total        int    `json:"total"`
					WouldLike    string `json:"would_like"`
					WouldComment string `json:"would_comment"`
					Note         string `json:"note"`
				} `json:"review"`
			} `json:"finalists"`
			RejectedCandidates []struct {
				ValidationCode string `json:"validation_code"`
				Text           string `json:"text"`
				TextSHA256     string `json:"text_sha256"`
			} `json:"rejected_candidates"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("decode audit log: %v\n%s", err, logs.String())
	}
	if event.Event != "belcanto_threads_editorial_review" || event.Level != "INFO" ||
		event.Message != "Belcanto Threads finalists reviewed" || event.SchemaVersion != 3 ||
		event.Audit.DraftID != draft.ID || len(event.Audit.Finalists) != 5 {
		t.Fatalf("audit event = %+v", event)
	}
	if event.Audit.SelectedWinnerID != "A" || event.Audit.PreviewStatus != "sent_acknowledged" {
		t.Fatalf("audit winner/delivery = %+v", event.Audit)
	}
	if event.Audit.PhotoSource != "pexels" || event.Audit.PhotoAssetID != "123" ||
		event.Audit.PhotoSourcePage != "https://www.pexels.com/photo/microphone-123/" || event.Audit.PhotoAuthor != "Lens Author" {
		t.Fatalf("audit photo provenance = %+v", event.Audit)
	}
	if !event.Audit.Finalists[0].Selected || event.Audit.Finalists[0].Review.Total != event.Audit.Finalists[1].Review.Total {
		t.Fatalf("audit rank did not apply reviewer tie-break: %+v", event.Audit.Finalists)
	}
	if len(event.Audit.RejectedCandidates) != 1 || event.Audit.RejectedCandidates[0].Text != "" ||
		event.Audit.RejectedCandidates[0].TextSHA256 == "" {
		t.Fatalf("rejected audit = %+v", event.Audit.RejectedCandidates)
	}
	selected := 0
	for index, finalist := range event.Audit.Finalists {
		if finalist.Text == "" {
			t.Fatal("full audit omitted an eligible finalist text")
		}
		if finalist.Rank != index+1 || finalist.Review.Hook == 0 || finalist.Review.Human == 0 ||
			finalist.Review.Recognition == 0 || finalist.Review.Replies == 0 || finalist.Review.Brevity == 0 ||
			finalist.Review.Voice == 0 || finalist.Review.Total == 0 || finalist.Review.WouldLike == "" ||
			finalist.Review.WouldComment == "" || finalist.Review.Note == "" {
			t.Fatalf("invalid finalist rank: %+v", finalist)
		}
		if finalist.Selected {
			selected++
			if finalist.Text != winner {
				t.Fatalf("logged winner = %q", finalist.Text)
			}
		}
	}
	if selected != 1 || event.Audit.WinnerTextSHA256 != threadAuditDigest(service.config.CallbackSecret, "text", winner) || event.Audit.DecisionReason == "" ||
		event.Audit.GenerationID != provider.capturedGenerationID() ||
		len(event.Audit.GenerationID) != 32 || event.Audit.User == "42" {
		t.Fatalf("audit identity = %+v selected=%d", event.Audit, selected)
	}
}

func TestBelcantoEditorialAuditRecordsPreviewFailureAfterSendAttempt(t *testing.T) {
	provider := &editorialAuditProvider{}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "full"
	service.threadPhotoSource = &staticLicensedPhotoSource{asset: photos.Asset{
		Provider: "pexels", AssetID: "123", PageURL: "https://www.pexels.com/photo/microphone-123/",
		Author: "Lens Author", AuthorURL: "https://www.pexels.com/@lens-author",
		Query: "vintage microphone close up", Alt: "Vintage microphone on an empty stage",
		Data: threadTestImage(t, 800, 1_000, color.RGBA{R: 30, G: 80, B: 140, A: 255}),
	}}
	service.telegram = &failOnceThreadTelegram{base: telegramClient, failNextPhoto: true}
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	user, brief, generationUpdateID := readyThreadBriefForTest(t, memory, 1)
	if err := service.generateThreadBrief(ctx, generationUpdateID, 42, user, brief); err == nil {
		t.Fatal("expected Telegram preview failure")
	}
	if len(snapshotThreadPhotos(telegramClient)) != 0 {
		t.Fatal("failed Telegram preview was recorded as delivered")
	}
	var event struct {
		Event string `json:"event"`
		Audit struct {
			SelectedWinnerID string `json:"selected_winner_id"`
			PreviewStatus    string `json:"preview_status"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("decode audit: %v\n%s", err, logs.String())
	}
	if event.Event != "belcanto_threads_editorial_review" || event.Audit.SelectedWinnerID != "A" ||
		event.Audit.PreviewStatus != "send_error_unknown" {
		t.Fatalf("failed preview audit = %+v", event)
	}
}

func TestThreadDraftPexelsCaptionStaysWithinTelegramLimit(t *testing.T) {
	draft := domain.ThreadDraft{
		Voice: domain.ThreadVoiceBelcanto, Goal: strings.Repeat("ц", 40),
		Text: strings.Repeat("я", 360), MediaMode: domain.ThreadMediaImage,
	}
	mediaValue := domain.ThreadMedia{
		SourceKind:      domain.ThreadMediaSourcePexels,
		SourceAuthor:    strings.Repeat("а", 160),
		SourcePageURL:   "https://www.pexels.com/" + strings.Repeat("p", 2_000),
		SourceAuthorURL: "https://www.pexels.com/@" + strings.Repeat("a", 2_000),
	}
	caption := threadDraftTextWithMediaAndBrief(draft, mediaValue, domain.ThreadMaterialText)
	if runes := utf8.RuneCountInString(caption); runes > 1_024 {
		t.Fatalf("caption runes = %d", runes)
	}
	if strings.Contains(caption, mediaValue.SourcePageURL) || strings.Contains(caption, mediaValue.SourceAuthorURL) {
		t.Fatal("long attribution URL leaked into caption instead of source button")
	}
}

func TestBelcantoRefinementDoesNotReusePexelsPhotoAgainstReviewerDecision(t *testing.T) {
	provider := &editorialAuditProvider{}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.config.BelcantoReviewLogMode = "off"
	service.threadPhotoSource = &staticLicensedPhotoSource{asset: photos.Asset{
		Provider: "pexels", AssetID: "123", PageURL: "https://www.pexels.com/photo/microphone-123/",
		Author: "Lens Author", AuthorURL: "https://www.pexels.com/@lens-author",
		Query: "vintage microphone close up", Alt: "Vintage microphone on an empty stage",
		Data: threadTestImage(t, 800, 1_000, color.RGBA{R: 20, G: 70, B: 110, A: 255}),
	}}
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	user, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	first := generateThreadDraftForTest(t, service, memory, 10)
	first, err = memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil || first.MediaMode != domain.ThreadMediaImage {
		t.Fatalf("first draft = %+v, %v", first, err)
	}

	provider.setVisualMode("text_only")
	if err := service.prepareThreadDraft(ctx, 1_000_011, 42, user, domain.ThreadVoiceBelcanto, "shorter", &first, nil); err != nil {
		t.Fatal(err)
	}
	refined, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil || refined.MediaMode != domain.ThreadMediaText || refined.MediaID != 0 {
		t.Fatalf("refined draft = %+v, %v", refined, err)
	}
	if len(snapshotThreadPhotos(telegramClient)) != 1 || len(telegramClient.snapshotMessages()) != 1 {
		t.Fatalf("photos=%d messages=%d", len(snapshotThreadPhotos(telegramClient)), len(telegramClient.snapshotMessages()))
	}
}

func TestBelcantoReviewerCanRequireAuthenticSchoolPhoto(t *testing.T) {
	provider := &editorialAuditProvider{}
	provider.setVisualMode("belcanto_photo")
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.config.BelcantoReviewLogMode = "off"
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	generateThreadDraftForTest(t, service, memory, 1)
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil || draft.MediaMode != domain.ThreadMediaImagePending || draft.MediaID != 0 {
		t.Fatalf("authentic-photo draft = %+v, %v", draft, err)
	}
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 || !strings.Contains(messages[0].Text, "редактор рекомендует реальное фото Belcanto") ||
		!strings.Contains(messages[0].Text, "Пришли один проверенный реальный кадр") {
		t.Fatalf("authentic-photo prompt = %+v", messages)
	}
	for _, row := range messages[0].ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(button.Text, "Опубликовать") {
				t.Fatalf("pending authentic photo exposed publish button: %+v", button)
			}
		}
	}
	if len(snapshotThreadPhotos(telegramClient)) != 0 {
		t.Fatal("belcanto_photo mode unexpectedly attached a stock image")
	}
}

func TestBelcantoEditorialAuditMetadataAndOffModes(t *testing.T) {
	provider := &editorialAuditProvider{}
	service, _, _, _, _ := newTestService(t, provider)
	result, err := provider.GenerateThreadPost(context.Background(), ai.ThreadPostRequest{GenerationID: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	draft := domain.ThreadDraft{
		ID: 77, TelegramID: 42, Voice: domain.ThreadVoiceBelcanto,
		Goal: result.Goal, Text: result.Text, MediaMode: domain.ThreadMediaText,
	}

	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "metadata"
	service.logBelcantoEditorialAudit(42, draft, "", result, nil, "sent")
	var event struct {
		Audit struct {
			DecisionReason string `json:"decision_reason"`
			Finalists      []struct {
				Text       string `json:"text"`
				TextSHA256 string `json:"text_sha256"`
				Review     struct {
					Total int    `json:"total"`
					Note  string `json:"note"`
				} `json:"review"`
			} `json:"finalists"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Audit.DecisionReason != "" || len(event.Audit.Finalists) != 5 {
		t.Fatalf("metadata audit = %+v", event.Audit)
	}
	for _, finalist := range event.Audit.Finalists {
		if finalist.Text != "" || finalist.TextSHA256 == "" || finalist.Review.Note != "" || finalist.Review.Total == 0 {
			t.Fatalf("metadata finalist leaked text or omitted scores: %+v", finalist)
		}
	}

	logs.Reset()
	service.config.BelcantoReviewLogMode = "off"
	service.logBelcantoEditorialAudit(42, draft, "", result, nil, "sent")
	if logs.Len() != 0 {
		t.Fatalf("off audit emitted %q", logs.String())
	}
}

func TestBelcantoFullAuditRedactsMaterialBackedModelFreeform(t *testing.T) {
	provider := &editorialAuditProvider{}
	service, _, _, _, _ := newTestService(t, provider)
	result, err := provider.GenerateThreadPost(context.Background(), ai.ThreadPostRequest{
		GenerationID: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	const material = "private material sentinel"
	result.Text = "Материал дня: " + material
	result.MaterialBasis = "material"
	result.Evidence = material
	result.Audit.DecisionReason = "Выбран из-за фразы " + material
	result.Visual.Query = material
	result.Visual.Brief = "Кадр повторяет " + material
	for index := range result.Audit.Candidates {
		result.Audit.Candidates[index].Text = "Подтверждённая деталь: " + material
		result.Audit.Candidates[index].MaterialBasis = "material"
		result.Audit.Candidates[index].Evidence = material
		result.Audit.Candidates[index].Review.Note = "Опирается на " + material
	}
	draft := domain.ThreadDraft{
		ID: 78, BriefID: 79, TelegramID: 42, Voice: domain.ThreadVoiceBelcanto,
		Objective: domain.ThreadObjectiveReplies, ScenarioID: result.ScenarioID,
		Goal: result.Goal, Text: result.Text, MediaMode: domain.ThreadMediaText,
	}
	brief := &domain.ThreadBrief{
		ID: 79, TelegramID: 42, MaterialKind: domain.ThreadMaterialText, MaterialText: material,
	}

	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "full"
	service.logBelcantoEditorialAudit(42, draft, "", result, nil, "sent", brief)
	if strings.Contains(logs.String(), material) {
		t.Fatalf("material-backed full audit leaked operator material: %s", logs.String())
	}
	var event struct {
		Audit struct {
			MaterialSHA256 string `json:"material_sha256"`
			DecisionReason string `json:"decision_reason"`
			PhotoQuery     string `json:"photo_query"`
			PhotoBrief     string `json:"photo_brief"`
			Finalists      []struct {
				Text       string `json:"text"`
				TextSHA256 string `json:"text_sha256"`
				Review     struct {
					Note string `json:"note"`
				} `json:"review"`
			} `json:"finalists"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Audit.MaterialSHA256 != threadAuditDigest(service.config.CallbackSecret, "material", material) || event.Audit.DecisionReason != "" ||
		event.Audit.PhotoQuery != "" || event.Audit.PhotoBrief != "" || len(event.Audit.Finalists) != 5 {
		t.Fatalf("material audit metadata = %+v", event.Audit)
	}
	for _, finalist := range event.Audit.Finalists {
		if finalist.Text != "" || finalist.TextSHA256 == "" || finalist.Review.Note != "" {
			t.Fatalf("material finalist leaked free-form data: %+v", finalist)
		}
	}
}

func TestThreadAuditDigestIsSecretKeyedAndDomainSeparated(t *testing.T) {
	material := threadAuditDigest("secret-one", "material", "короткая фраза")
	if material == threadAuditDigest("secret-two", "material", "короткая фраза") ||
		material == threadAuditDigest("secret-one", "evidence", "короткая фраза") ||
		len(material) != 64 {
		t.Fatalf("audit digest is not keyed/domain-separated: %q", material)
	}
}

func TestBelcantoEditorialAuditBoundsUntrustedMetadata(t *testing.T) {
	provider := &editorialAuditProvider{}
	service, _, _, _, _ := newTestService(t, provider)
	result, err := provider.GenerateThreadPost(context.Background(), ai.ThreadPostRequest{
		GenerationID: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	result.Audit.GeneratorModel = "model\nFORGED\u202e\u200b " + strings.Repeat("м", 300)
	result.Audit.GenerationRequestIDs = []string{
		"request-one\nFORGED", "request-two", "request-three", "request-four-must-be-dropped",
	}
	result.Audit.DecisionReason = "first line\nsecond line " + strings.Repeat("р", 300)
	result.Audit.Candidates[0].Review.Note = "note\ncontinued " + strings.Repeat("н", 300)
	for index := 0; index < 30; index++ {
		result.Audit.Candidates = append(result.Audit.Candidates, ai.ThreadPostCandidateAudit{
			Attempt: 2, SourceSlot: strings.Repeat("s", 100),
			Text:           "NEVER_LOG_REJECTED_" + string(rune('A'+index)),
			ValidationCode: strings.Repeat("v", 200),
		})
	}

	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "full"
	service.logBelcantoEditorialAudit(42, domain.ThreadDraft{
		ID: 88, TelegramID: 42, Voice: domain.ThreadVoiceBelcanto,
		Goal: result.Goal, Text: result.Text, MediaMode: domain.ThreadMediaText,
	}, "", result, nil, "sent")

	var event struct {
		SchemaVersion int `json:"schema_version"`
		Audit         struct {
			GeneratorModel       string   `json:"generator_model"`
			GenerationRequestIDs []string `json:"generation_request_ids"`
			DecisionReason       string   `json:"decision_reason"`
			Finalists            []struct {
				Review struct {
					Note string `json:"note"`
				} `json:"review"`
			} `json:"finalists"`
			RejectedCandidates []struct {
				SourceSlot     string `json:"source_slot"`
				ValidationCode string `json:"validation_code"`
				Text           string `json:"text"`
			} `json:"rejected_candidates"`
		} `json:"audit"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.SchemaVersion != 3 || len(event.Audit.GenerationRequestIDs) != maxBelcantoAuditRequestIDs ||
		len(event.Audit.Finalists) != 5 || len(event.Audit.RejectedCandidates) != maxBelcantoAuditCandidates-5 {
		t.Fatalf("bounded audit shape = %+v", event)
	}
	if strings.ContainsAny(event.Audit.GeneratorModel, "\r\n\u202e\u200b") || utf8.RuneCountInString(event.Audit.GeneratorModel) > maxBelcantoAuditProviderRunes ||
		strings.ContainsAny(event.Audit.DecisionReason, "\r\n") || utf8.RuneCountInString(event.Audit.DecisionReason) > maxBelcantoAuditReasonRunes ||
		strings.ContainsAny(event.Audit.Finalists[0].Review.Note, "\r\n") || utf8.RuneCountInString(event.Audit.Finalists[0].Review.Note) > maxBelcantoAuditReviewNoteRunes {
		t.Fatalf("unbounded audit metadata = %+v", event.Audit)
	}
	for _, rejected := range event.Audit.RejectedCandidates {
		if rejected.Text != "" || utf8.RuneCountInString(rejected.SourceSlot) > maxBelcantoAuditIdentifierRunes ||
			utf8.RuneCountInString(rejected.ValidationCode) > maxBelcantoAuditValidationRunes {
			t.Fatalf("unsafe rejected metadata = %+v", rejected)
		}
	}
	if bytes.Count(logs.Bytes(), []byte{'\n'}) != 1 {
		t.Fatalf("audit log injection created extra records: %q", logs.String())
	}
}

func TestThreadPostDeliverySafetyKeepsFormattingAndNeverMasksReviewedText(t *testing.T) {
	service, _, _, _, _ := newTestService(t, &editorialAuditProvider{})
	multiline := "Какую песню вы узнаете сразу?\n\nТу, где первый вдох уже звучит как припев."
	result := validTestThreadResult(multiline, "test", "test")
	kept, ok := service.selectLastMileThreadPost(result)
	if !ok || kept.Text != multiline || kept.Audit.Candidates[0].Text != multiline {
		t.Fatalf("multiline winner was rewritten: %+v", kept)
	}

	pii := "Ответ пришлите на music@example.com — так будет проще."
	safe := "Какую песню вы узнаете раньше, чем вспоминаете её название?"
	tieWinner := "Какая мелодия первой возвращает вам конкретный момент?"
	result = validTestThreadResult(pii, "test", "test")
	result.Audit.Candidates[0].Review = ai.ThreadPostReviewerScore{Total: 95}
	result.Audit.Candidates[1].Text = safe
	result.Audit.Candidates[1].Review = ai.ThreadPostReviewerScore{Total: 90, WouldComment: "no", WouldLike: "yes", Replies: 9, Human: 9, Hook: 9}
	result.Audit.Candidates[1].Local.Score = 100
	result.Audit.Candidates[2].Text = tieWinner
	result.Audit.Candidates[2].Review = ai.ThreadPostReviewerScore{Total: 90, WouldComment: "yes", WouldLike: "maybe", Replies: 8, Human: 8, Hook: 8}
	result.Audit.Candidates[2].Local.Score = 1
	selected, ok := service.selectLastMileThreadPost(result)
	if !ok || selected.Text != tieWinner || strings.Contains(selected.Text, "скрыт") || selected.Audit.SelectionMode != "last_mile_override" {
		t.Fatalf("PII winner was masked instead of excluded: %+v", selected)
	}
}

func TestThreadPostEditorialBoundaryRejectsMalformedProviderAudit(t *testing.T) {
	service, _, _, _, _ := newTestService(t, &editorialAuditProvider{})
	newResult := func(t *testing.T) ai.ThreadPostResult {
		t.Helper()
		provider := &editorialAuditProvider{}
		result, err := provider.GenerateThreadPost(context.Background(), ai.ThreadPostRequest{
			GenerationID: "0123456789abcdef0123456789abcdef",
		})
		if err != nil {
			t.Fatal(err)
		}
		// The audit provider includes one rejected record after its five
		// finalists; keep a private backing array for every mutation case.
		result.Audit.Candidates = append([]ai.ThreadPostCandidateAudit(nil), result.Audit.Candidates...)
		return result
	}

	if result, ok := service.selectLastMileThreadPost(newResult(t)); !ok || result.Text == "" {
		t.Fatalf("valid editorial result rejected: %+v", result)
	}
	tests := []struct {
		name   string
		mutate func(*ai.ThreadPostResult)
	}{
		{name: "four finalists", mutate: func(result *ai.ThreadPostResult) {
			result.Audit.Candidates[4].Considered = false
			result.Audit.Candidates[4].Eligible = false
		}},
		{name: "six finalists", mutate: func(result *ai.ThreadPostResult) {
			result.Audit.Candidates = append(result.Audit.Candidates, ai.ThreadPostCandidateAudit{
				Attempt: 2, SourceSlot: "extra", ReviewerID: "F", Goal: "discussion",
				Text:     "Какую мелодию вы узнаете раньше, чем вспоминаете её название?",
				Eligible: true, Considered: true,
			})
		}},
		{name: "no selected finalist", mutate: func(result *ai.ThreadPostResult) {
			for index := range result.Audit.Candidates {
				result.Audit.Candidates[index].Selected = false
			}
		}},
		{name: "two selected finalists", mutate: func(result *ai.ThreadPostResult) {
			result.Audit.Candidates[1].Selected = true
		}},
		{name: "winner id mismatch", mutate: func(result *ai.ThreadPostResult) {
			result.Audit.DeliveredWinnerID = "B"
		}},
		{name: "winner text mismatch", mutate: func(result *ai.ThreadPostResult) {
			result.Text = "Какую мелодию вы узнаете раньше, чем вспоминаете её название?"
		}},
		{name: "winner goal mismatch", mutate: func(result *ai.ThreadPostResult) {
			result.Goal = "warmth"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := newResult(t)
			test.mutate(&result)
			if selected, ok := service.selectLastMileThreadPost(result); ok {
				t.Fatalf("malformed provider result accepted: %+v", selected)
			}
		})
	}
}

func TestBelcantoPhotoSourceFailureKeepsWinnerTextReady(t *testing.T) {
	provider := &editorialAuditProvider{}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	service.config.BelcantoReviewLogMode = "off"
	service.threadPhotoSource = &staticLicensedPhotoSource{err: errors.New("Pexels unavailable")}
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	generateThreadDraftForTest(t, service, memory, 1)
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil || draft.MediaMode != domain.ThreadMediaText || draft.Text == "" {
		t.Fatalf("draft = %+v, %v", draft, err)
	}
	if len(telegramClient.snapshotMessages()) != 1 || len(snapshotThreadPhotos(telegramClient)) != 0 {
		t.Fatalf("messages=%+v photos=%+v", telegramClient.snapshotMessages(), snapshotThreadPhotos(telegramClient))
	}
}
