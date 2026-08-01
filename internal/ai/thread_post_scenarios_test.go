package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

func TestThreadPostScenarioPlanHasTwelveDistinctSafeMechanics(t *testing.T) {
	request, err := normalizeThreadPostRequest(ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveReplies,
		MaterialKind: domain.ThreadMaterialNone, PreviousScenarioID: "karaoke_archetype", Transform: "different_angle",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectThreadPostScenarioPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != threadPostConceptCount {
		t.Fatalf("plan size = %d", len(plan))
	}
	scenarios := make(map[string]struct{}, len(plan))
	mechanisms := make(map[string]struct{}, len(plan))
	for _, scenario := range plan {
		if scenario.RequiresMaterial || scenario.ID == request.PreviousScenarioID {
			t.Fatalf("unsafe/repeated scenario in no-material plan: %+v", scenario)
		}
		scenarios[scenario.ID] = struct{}{}
		mechanisms[scenario.Mechanism] = struct{}{}
	}
	if len(scenarios) != threadPostConceptCount || len(mechanisms) < 6 {
		t.Fatalf("scenario/mechanism diversity = %d/%d", len(scenarios), len(mechanisms))
	}
}

func TestThreadPostScenarioPlanPreservesAngleExceptDifferentAngle(t *testing.T) {
	for _, test := range []struct {
		transform string
		want      bool
	}{
		{transform: "warmer", want: true},
		{transform: "shorter", want: true},
		{transform: "wittier", want: true},
		{transform: "no_sell", want: true},
		{transform: "different_angle", want: false},
	} {
		request, err := normalizeThreadPostRequest(ThreadPostRequest{
			Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveReplies,
			MaterialKind: domain.ThreadMaterialNone, PreviousScenarioID: "karaoke_archetype", Transform: test.transform,
		})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := selectThreadPostScenarioPlan(request)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, scenario := range plan {
			found = found || scenario.ID == request.PreviousScenarioID
		}
		if found != test.want {
			t.Errorf("transform %s preserves previous=%v, want %v", test.transform, found, test.want)
		}
	}
}

func TestThreadPostMaterialScenarioPlanReservesGroundedQuotaAndAnchors(t *testing.T) {
	material := strings.Join([]string{
		"Педагог предложил сначала проговорить сложную строку, а затем спеть её в разговорном ритме.",
		"На занятии голос сорвался на высокой ноте, и педагог спокойно предложил повторить короткий фрагмент.",
		"Перед выступлением участники вместе проверили микрофон и начало песни.",
		"Для проверки тональности педагог предложил негромко спеть первый куплет.",
	}, "\n")
	for _, objective := range []domain.ThreadObjective{
		domain.ThreadObjectiveReach, domain.ThreadObjectiveReplies, domain.ThreadObjectiveTrust,
		domain.ThreadObjectiveCommunity, domain.ThreadObjectiveTrial,
	} {
		request, err := normalizeThreadPostRequest(ThreadPostRequest{
			Voice: domain.ThreadVoiceBelcanto, Objective: objective,
			MaterialKind: domain.ThreadMaterialText, Material: material,
			PreviousScenarioID: "karaoke_archetype", Transform: "warmer",
		})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := selectThreadPostScenarioPlan(request)
		if err != nil {
			t.Fatal(err)
		}
		if got := countMaterialRequiredScenarios(plan); got < minimumMaterialScenarios {
			t.Errorf("objective=%s material scenarios=%d", objective, got)
		}
		scenarios := make(map[string]struct{}, len(plan))
		for _, scenario := range plan {
			scenarios[scenario.ID] = struct{}{}
		}
		if _, preserved := scenarios[request.PreviousScenarioID]; !preserved {
			t.Errorf("objective=%s did not preserve previous scenario", objective)
		}
		if !threadPostPortfolioHasObjectiveAnchor(threadPostObjectiveAnchorIDs(request), scenarios) {
			t.Errorf("objective=%s plan has no objective anchor: %#v", objective, scenarios)
		}
	}

	request, err := normalizeThreadPostRequest(ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveReplies,
		MaterialKind: domain.ThreadMaterialText, Material: material,
		PreviousScenarioID: "teacher_micro_tip", Transform: "different_angle",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectThreadPostScenarioPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := countMaterialRequiredScenarios(plan); got < minimumMaterialScenarios {
		t.Fatalf("different-angle material scenarios=%d", got)
	}
	for _, scenario := range plan {
		if scenario.ID == request.PreviousScenarioID {
			t.Fatalf("different-angle plan retained %s", scenario.ID)
		}
	}
}

func TestThreadPostGroundingRequiresExactEvidenceForDigitsAndQuotes(t *testing.T) {
	material := "Пробный урок в воскресенье стоит 5000 ₸. Ученица сказала: «Мне было спокойно»."
	request, err := normalizeThreadPostRequest(ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveTrial,
		MaterialKind: domain.ThreadMaterialText, Material: material,
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := ThreadPostResult{
		Goal: "trial", Objective: domain.ThreadObjectiveTrial, ScenarioID: "transparent_invitation", Mechanism: "conversion",
		MaterialBasis: "material", Evidence: material, Text: "Пробный урок в воскресенье стоит 5000 ₸. «Мне было спокойно». Запись — сообщением.",
	}
	if err := validateThreadPostResult(&valid, request); err != nil {
		t.Fatalf("grounded result rejected: %v", err)
	}
	newDigit := valid
	newDigit.Text = strings.Replace(newDigit.Text, "5000", "6000", 1)
	if got := invalidOutputCode(validateThreadPostResult(&newDigit, request)); got != "invalid_output_thread_ungrounded_digit" {
		t.Fatalf("ungrounded digit code = %q", got)
	}
	newQuote := valid
	newQuote.Text = strings.Replace(newQuote.Text, "Мне было спокойно", "Я сразу полюбила урок", 1)
	if got := invalidOutputCode(validateThreadPostResult(&newQuote, request)); got != "invalid_output_thread_ungrounded_quote" {
		t.Fatalf("ungrounded quote code = %q", got)
	}
}

func TestThreadPostWriterRejectsMissingMaterialAndObjectiveQuotas(t *testing.T) {
	material := strings.Join([]string{
		"Педагог предложил сначала проговорить сложную строку, а затем спеть её в разговорном ритме.",
		"На занятии голос сорвался на высокой ноте, и педагог спокойно предложил повторить короткий фрагмент.",
		"Перед выступлением участники вместе проверили микрофон и начало песни.",
		"Для проверки тональности педагог предложил негромко спеть первый куплет.",
	}, "\n")
	materialRequest, err := normalizeThreadPostRequest(ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveReach,
		MaterialKind: domain.ThreadMaterialText, Material: material,
	})
	if err != nil {
		t.Fatal(err)
	}
	materialPlan, err := selectThreadPostScenarioPlan(materialRequest)
	if err != nil {
		t.Fatal(err)
	}
	evidence := materialEvidenceForPlan(materialPlan)
	concepts := testScenarioConcepts(materialRequest, materialPlan, evidence)
	evergreen := finalistsByScenario(t, materialRequest, materialPlan, []string{
		"karaoke_archetype", "astana_soundtrack", "song_memory", "adult_beginner", "recording_reaction",
	}, nil)
	_, _, err = decodeThreadPostFinalists(mustJSON(t, threadPostFinalistEnvelope{Finalists: evergreen}), materialRequest, concepts, 1)
	if err == nil || !strings.Contains(err.Error(), "at least two material-backed") {
		t.Fatalf("missing material quota error = %v", err)
	}

	evergreenRequest, err := normalizeThreadPostRequest(ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveReach, MaterialKind: domain.ThreadMaterialNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	evergreenPlan, err := selectThreadPostScenarioPlan(evergreenRequest)
	if err != nil {
		t.Fatal(err)
	}
	evergreenConcepts := testScenarioConcepts(evergreenRequest, evergreenPlan, nil)
	withoutAnchor := finalistsByScenario(t, evergreenRequest, evergreenPlan, []string{
		"song_memory", "audience_choice", "adult_beginner", "after_work_creativity", "mini_voice_experiment",
	}, nil)
	_, _, err = decodeThreadPostFinalists(mustJSON(t, threadPostFinalistEnvelope{Finalists: withoutAnchor}), evergreenRequest, evergreenConcepts, 1)
	if err == nil || !strings.Contains(err.Error(), "objective anchor") {
		t.Fatalf("missing objective anchor error = %v", err)
	}
}

func TestAnthropicThreadPostRunsConceptWriterAndBlindReviewStages(t *testing.T) {
	requestValue := ThreadPostRequest{
		GenerationID: strings.Repeat("c", 32), Voice: domain.ThreadVoiceBelcanto,
		Objective: domain.ThreadObjectiveReplies, MaterialKind: domain.ThreadMaterialNone, Language: "ru", Seed: 7,
	}
	normalized, err := normalizeThreadPostRequest(requestValue)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		t.Fatal(err)
	}
	concepts := testScenarioConcepts(normalized, plan, nil)
	finalists := testScenarioFinalists(normalized, plan)
	if len(finalists) != threadPostFinalistCount {
		t.Fatalf("fixture finalists = %d", len(finalists))
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := calls.Add(1)
		payload := decodeThreadTestRequest(t, request)
		if payload.OutputConfig.Effort != "high" || payload.MaxTokens != 9_000 {
			t.Errorf("stage effort/tokens = %q/%d", payload.OutputConfig.Effort, payload.MaxTokens)
		}
		writer.Header().Set("request-id", "stage-"+string(rune('0'+call)))
		switch payload.System {
		case threadPostConceptSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostConceptEnvelope{Concepts: concepts}), 11, 12)
		case threadPostWriterSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostFinalistEnvelope{Finalists: finalists}), 13, 14)
		case threadPostReviewerSystemPrompt:
			ids := reviewerSchemaIDs(t, payload.OutputConfig.Format.Schema)
			writeAnthropicThreadResponse(t, writer, mustMarshalThreadReview(t, ids, ids[0]), 15, 16)
		default:
			t.Fatalf("unexpected stage system prompt: %.80s", payload.System)
		}
	}))
	defer server.Close()

	provider, err := NewAnthropic(AnthropicConfig{
		APIKey: "test", BaseURL: server.URL, Timeout: time.Second, ThreadTimeout: time.Second,
		ThreadMaxTokens: 9_000, MaxRetries: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.GenerateThreadPost(context.Background(), requestValue)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || result.Audit.ConceptCalls != 1 || result.Audit.WriterCalls != 1 || result.Audit.ReviewCalls != 1 {
		t.Fatalf("calls/audit = %d/%+v", calls.Load(), result.Audit)
	}
	if len(result.Audit.Concepts) != threadPostConceptCount || len(result.Audit.Candidates) != threadPostFinalistCount {
		t.Fatalf("concept/finalist audit = %d/%d", len(result.Audit.Concepts), len(result.Audit.Candidates))
	}
	if result.Objective != domain.ThreadObjectiveReplies || result.ScenarioID == "" || result.Mechanism == "" {
		t.Fatalf("winner contract = %+v", result)
	}
	if result.Usage.InputTokens != 39 || result.Usage.OutputTokens != 42 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestAnthropicThreadPostRepairsInvalidBlindReviewOnce(t *testing.T) {
	requestValue := ThreadPostRequest{
		GenerationID: strings.Repeat("r", 32), Voice: domain.ThreadVoiceBelcanto,
		Objective: domain.ThreadObjectiveReplies, MaterialKind: domain.ThreadMaterialNone, Language: "ru", Seed: 11,
	}
	normalized, err := normalizeThreadPostRequest(requestValue)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		t.Fatal(err)
	}
	concepts := testScenarioConcepts(normalized, plan, nil)
	finalists := testScenarioFinalists(normalized, plan)

	var calls atomic.Int32
	var reviews atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		payload := decodeThreadTestRequest(t, request)
		switch payload.System {
		case threadPostConceptSystemPrompt:
			writer.Header().Set("request-id", "concept")
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostConceptEnvelope{Concepts: concepts}), 1, 2)
		case threadPostWriterSystemPrompt:
			writer.Header().Set("request-id", "writer")
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostFinalistEnvelope{Finalists: finalists}), 3, 4)
		case threadPostReviewerSystemPrompt:
			review := reviews.Add(1)
			isRepair := strings.Contains(payload.messageText(), "previous review envelope failed strict service validation")
			if review == 1 {
				if isRepair {
					t.Error("first review unexpectedly used repair prompt")
				}
				writer.Header().Set("request-id", "review-bad")
				writeAnthropicThreadResponse(t, writer, []byte(`{"winner_id":"Z"}`), 5, 6)
				return
			}
			if review != 2 {
				t.Fatalf("unexpected review attempt %d", review)
			}
			if !isRepair {
				t.Error("second review did not use the corrective prompt")
			}
			writer.Header().Set("request-id", "review-good")
			ids := reviewerSchemaIDs(t, payload.OutputConfig.Format.Schema)
			reviewRaw := mustMarshalThreadReview(t, ids, ids[0])
			var reviewEnvelope threadPostReviewerEnvelope
			if err := json.Unmarshal(reviewRaw, &reviewEnvelope); err != nil {
				t.Fatal(err)
			}
			reviewEnvelope.Visual.Query = "anna private student portrait"
			writeAnthropicThreadResponse(t, writer, mustJSON(t, reviewEnvelope), 7, 8)
		default:
			t.Fatalf("unexpected stage system prompt: %.80s", payload.System)
		}
	}))
	defer server.Close()

	provider, err := NewAnthropic(AnthropicConfig{
		APIKey: "test", BaseURL: server.URL, Timeout: time.Second, ThreadTimeout: time.Second,
		ThreadMaxTokens: 9_000, MaxRetries: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.GenerateThreadPost(context.Background(), requestValue)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || reviews.Load() != 2 || result.Audit.ReviewCalls != 2 {
		t.Fatalf("calls/reviews/audit = %d/%d/%d", calls.Load(), reviews.Load(), result.Audit.ReviewCalls)
	}
	if result.Audit.ReviewUsage != (domain.Usage{InputTokens: 12, OutputTokens: 14}) {
		t.Fatalf("review usage = %+v", result.Audit.ReviewUsage)
	}
	if result.Audit.ReviewerRequestID != "review-good" || result.Audit.ReviewerModel != "claude-sonnet-5-threads-test" {
		t.Fatalf("review metadata = %q/%q", result.Audit.ReviewerRequestID, result.Audit.ReviewerModel)
	}
	if result.Audit.SelectionMode != "anthropic_blind_review" || result.Audit.ReviewerError != "" {
		t.Fatalf("review outcome = %+v", result.Audit)
	}
	if result.Visual.Query != SafeThreadPhotoQuery(result.ScenarioID) || strings.Contains(result.Visual.Query, "anna") || strings.Contains(result.Visual.Query, "private") {
		t.Fatalf("unsafe reviewer query escaped: %+v", result.Visual)
	}
	if result.Usage != (domain.Usage{InputTokens: 16, OutputTokens: 20}) {
		t.Fatalf("total usage = %+v", result.Usage)
	}
}

func TestAnthropicThreadPostReviewerDeliversStrongestMaterialBackedFinalist(t *testing.T) {
	materialLines := []string{
		"Педагог предложил сначала проговорить сложную строку, а затем спеть её в разговорном ритме.",
		"На занятии голос сорвался на высокой ноте, и педагог спокойно предложил повторить короткий фрагмент.",
		"Перед выступлением участники вместе проверили микрофон и начало песни.",
		"Для проверки тональности педагог предложил негромко спеть первый куплет.",
	}
	requestValue := ThreadPostRequest{
		GenerationID: strings.Repeat("m", 32), Voice: domain.ThreadVoiceBelcanto,
		Objective: domain.ThreadObjectiveReach, MaterialKind: domain.ThreadMaterialText,
		Material: strings.Join(materialLines, "\n"), Language: "ru", Seed: 1,
	}
	normalized, err := normalizeThreadPostRequest(requestValue)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		t.Fatal(err)
	}
	evidence := materialEvidenceForPlan(plan)
	concepts := testScenarioConcepts(normalized, plan, evidence)
	finalists := finalistsByScenario(t, normalized, plan, []string{
		"teacher_micro_tip", "normal_mistake", "karaoke_archetype", "astana_soundtrack", "song_memory",
	}, evidence)

	reviewerCandidates := make([]threadPostCandidate, 0, len(finalists))
	reviewerAudits := make([]ThreadPostCandidateAudit, len(finalists))
	for index, finalist := range finalists {
		reviewerCandidates = append(reviewerCandidates, threadPostCandidate{Result: finalist, AuditIndex: index})
	}
	assignBlindReviewerIDs(reviewerCandidates, reviewerAudits, normalized.Seed, 3)
	materialIDs := make([]string, 0, 2)
	evergreenID := ""
	for _, candidate := range reviewerCandidates {
		if candidate.Result.MaterialBasis == "material" {
			materialIDs = append(materialIDs, candidate.ReviewerID)
		} else if evergreenID == "" {
			evergreenID = candidate.ReviewerID
		}
	}
	if len(materialIDs) != 2 || evergreenID == "" {
		t.Fatalf("reviewer fixture IDs: material=%v evergreen=%q", materialIDs, evergreenID)
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		payload := decodeThreadTestRequest(t, request)
		switch payload.System {
		case threadPostConceptSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostConceptEnvelope{Concepts: concepts}), 1, 1)
		case threadPostWriterSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostFinalistEnvelope{Finalists: finalists}), 1, 1)
		case threadPostReviewerSystemPrompt:
			ids := reviewerSchemaIDs(t, payload.OutputConfig.Format.Schema)
			scorecards := make(map[string]threadPostReviewerCard, len(ids))
			for _, id := range ids {
				score := 7
				if id == evergreenID {
					score = 10
				} else if id == materialIDs[0] {
					score = 9
				} else if id == materialIDs[1] {
					score = 8
				}
				scorecards[id] = threadPostReviewerCard{
					Hook: score, Human: score, Recognition: score, Replies: score, Brevity: score,
					Voice: score, GoalFit: score, Grounding: score, Distinctive: score, FactSafe: true,
					WouldLike: "yes", WouldComment: "yes", Note: "Проверяемая редакторская оценка кандидата.",
				}
			}
			raw, marshalErr := json.Marshal(threadPostReviewerEnvelope{
				WinnerID: evergreenID, WinnerReason: "Evergreen набрал больше, но материал обязан определять доставленный пост.",
				Scorecards: scorecards,
				Visual:     threadPostReviewerVisual{Mode: "text_only", Query: "vintage microphone", Brief: ""},
			})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			writeAnthropicThreadResponse(t, writer, raw, 1, 1)
		default:
			t.Fatalf("unexpected stage system prompt: %.80s", payload.System)
		}
	}))
	defer server.Close()
	provider, err := NewAnthropic(AnthropicConfig{
		APIKey: "test", BaseURL: server.URL, Timeout: time.Second, ThreadTimeout: time.Second, MaxRetries: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.GenerateThreadPost(context.Background(), requestValue)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || result.MaterialBasis != "material" || result.Evidence == "" {
		t.Fatalf("calls=%d result=%+v", calls.Load(), result)
	}
	if result.Audit.DeliveredWinnerID != materialIDs[0] || result.Audit.ReviewerWinnerID != evergreenID || result.Audit.SelectionMode != "review_score_override" {
		t.Fatalf("material winner audit = %+v", result.Audit)
	}
}

func TestMaterialBackedThreadPostFailsClosedAfterTwoInvalidReviews(t *testing.T) {
	material := strings.Join([]string{
		"Пробный урок пройдёт в воскресенье в 18:30.",
		"На первой минуте педагог спрашивает, какую песню вы любите.",
		"На пробном не нужно петь песню целиком.",
		"Можно начать индивидуально или в мини-группе.",
		"Педагог предлагает сначала проговорить сложную строку.",
	}, "\n")
	requestValue := ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveTrial,
		MaterialKind: domain.ThreadMaterialText, Material: material, Language: "ru",
	}
	normalized, err := normalizeThreadPostRequest(requestValue)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		t.Fatal(err)
	}
	evidence := map[string]string{
		"transparent_invitation": "Пробный урок пройдёт в воскресенье в 18:30.",
		"first_minute":           "На первой минуте педагог спрашивает, какую песню вы любите.",
		"what_wont_happen":       "На пробном не нужно петь песню целиком.",
		"format_choice":          "Можно начать индивидуально или в мини-группе.",
		"teacher_micro_tip":      "Педагог предлагает сначала проговорить сложную строку.",
	}
	concepts := testScenarioConcepts(normalized, plan, evidence)
	finalists := make([]ThreadPostResult, 0, 5)
	for _, scenario := range plan[:5] {
		excerpt := evidence[scenario.ID]
		finalists = append(finalists, ThreadPostResult{
			Goal: "trial", Objective: domain.ThreadObjectiveTrial, ScenarioID: scenario.ID, Mechanism: scenario.Mechanism,
			MaterialBasis: "material", Evidence: excerpt, Text: excerpt,
		})
	}
	var calls atomic.Int32
	var reviews atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		payload := decodeThreadTestRequest(t, request)
		switch payload.System {
		case threadPostConceptSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostConceptEnvelope{Concepts: concepts}), 1, 1)
		case threadPostWriterSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostFinalistEnvelope{Finalists: finalists}), 1, 1)
		case threadPostReviewerSystemPrompt:
			review := reviews.Add(1)
			writer.Header().Set("request-id", "review-"+twoDigits(int(review)))
			if review == 2 && !strings.Contains(payload.messageText(), "previous review envelope failed strict service validation") {
				t.Error("second review did not use the corrective prompt")
			}
			writeAnthropicThreadResponse(t, writer, []byte(`{"winner_id":"Z"}`), 1, 1)
		}
	}))
	defer server.Close()
	provider, err := NewAnthropic(AnthropicConfig{APIKey: "test", BaseURL: server.URL, Timeout: time.Second, ThreadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.GenerateThreadPost(context.Background(), requestValue)
	if err == nil || !errors.Is(err, ErrInvalidResponse) || calls.Load() != 4 || reviews.Load() != 2 {
		t.Fatalf("calls=%d reviews=%d error=%v", calls.Load(), reviews.Load(), err)
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.RequestID != "review-02" || providerErr.Code != "invalid_output_thread_review" {
		t.Fatalf("provider error = %#v", providerErr)
	}
}

func TestAnthropicThreadPostDoesNotSemanticRetryReviewerAuthenticationError(t *testing.T) {
	requestValue := ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveReplies,
		MaterialKind: domain.ThreadMaterialNone, Language: "ru", Seed: 13,
	}
	normalized, err := normalizeThreadPostRequest(requestValue)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		t.Fatal(err)
	}
	concepts := testScenarioConcepts(normalized, plan, nil)
	finalists := testScenarioFinalists(normalized, plan)

	var calls atomic.Int32
	var reviews atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		payload := decodeThreadTestRequest(t, request)
		switch payload.System {
		case threadPostConceptSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostConceptEnvelope{Concepts: concepts}), 1, 1)
		case threadPostWriterSystemPrompt:
			writeAnthropicThreadResponse(t, writer, mustJSON(t, threadPostFinalistEnvelope{Finalists: finalists}), 1, 1)
		case threadPostReviewerSystemPrompt:
			reviews.Add(1)
			writer.Header().Set("request-id", "review-auth")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
		}
	}))
	defer server.Close()
	provider, err := NewAnthropic(AnthropicConfig{
		APIKey: "test", BaseURL: server.URL, Timeout: time.Second, ThreadTimeout: time.Second,
		MaxRetries: 3, RetryBase: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.GenerateThreadPost(context.Background(), requestValue)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || reviews.Load() != 1 || result.Audit.ReviewCalls != 1 {
		t.Fatalf("calls/reviews/audit = %d/%d/%d", calls.Load(), reviews.Load(), result.Audit.ReviewCalls)
	}
	if result.Audit.SelectionMode != "local_review_fallback" || result.Audit.ReviewerError != "authentication_error" {
		t.Fatalf("review fallback = %+v", result.Audit)
	}
	if result.Audit.ReviewerRequestID != "review-auth" {
		t.Fatalf("review request ID = %q", result.Audit.ReviewerRequestID)
	}
}

func TestFakeThreadPostPortfoliosMeetDiversityAnchorAndMaterialContractsAcrossSeeds(t *testing.T) {
	provider := NewFake()
	transforms := []string{"", "wittier", "warmer", "shorter", "different_angle", "no_sell"}
	objectives := []domain.ThreadObjective{
		domain.ThreadObjectiveReach, domain.ThreadObjectiveReplies, domain.ThreadObjectiveTrust,
		domain.ThreadObjectiveCommunity, domain.ThreadObjectiveTrial,
	}
	material := "Педагог предлагает сначала проговорить строку песни, затем негромко спеть её в разговорном ритме."
	for _, objective := range objectives {
		for _, withMaterial := range []bool{false, true} {
			if objective == domain.ThreadObjectiveTrial && !withMaterial {
				continue
			}
			for _, transform := range transforms {
				for seed := uint32(0); seed < 32; seed++ {
					request := ThreadPostRequest{
						Voice: domain.ThreadVoiceBelcanto, Objective: objective,
						MaterialKind: domain.ThreadMaterialNone, Transform: transform, Seed: seed,
					}
					if withMaterial {
						request.MaterialKind = domain.ThreadMaterialText
						request.Material = material
					}
					result, err := provider.GenerateThreadPost(context.Background(), request)
					if err != nil {
						t.Fatalf("objective=%s material=%v transform=%s seed=%d: %v", objective, withMaterial, transform, seed, err)
					}
					mechanisms := make(map[string]struct{})
					scenarios := make(map[string]struct{})
					materialFinalists := 0
					considered := 0
					for _, candidate := range result.Audit.Candidates {
						if !candidate.Eligible || !candidate.Considered {
							continue
						}
						considered++
						mechanisms[candidate.Mechanism] = struct{}{}
						scenarios[candidate.ScenarioID] = struct{}{}
						if candidate.MaterialBasis == "material" && candidate.Evidence != "" {
							materialFinalists++
						}
					}
					normalized, normalizeErr := normalizeThreadPostRequest(request)
					if normalizeErr != nil {
						t.Fatal(normalizeErr)
					}
					if considered != threadPostFinalistCount || len(mechanisms) < 4 ||
						!threadPostPortfolioHasObjectiveAnchor(threadPostObjectiveAnchorIDs(normalized), scenarios) {
						t.Fatalf("invalid fake portfolio objective=%s material=%v transform=%s seed=%d candidates=%+v", objective, withMaterial, transform, seed, result.Audit.Candidates)
					}
					if withMaterial && (materialFinalists < minimumMaterialBackedFinalistCount || result.MaterialBasis != "material" || result.Evidence == "") {
						t.Fatalf("ungrounded fake portfolio objective=%s transform=%s seed=%d result=%+v", objective, transform, seed, result)
					}
				}
			}
		}
	}
}

func TestTrialDifferentAngleUsesAlternativeAnchor(t *testing.T) {
	request := ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveTrial,
		MaterialKind:       domain.ThreadMaterialText,
		Material:           "Педагог предлагает сначала проговорить строку песни, затем негромко спеть её в разговорном ритме.",
		PreviousScenarioID: "transparent_invitation", Transform: "different_angle", Seed: 9,
	}
	normalized, err := normalizeThreadPostRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	anchors := threadPostObjectiveAnchorIDs(normalized)
	if len(anchors) != 3 || anchors[0] != "first_minute" {
		t.Fatalf("alternative anchors = %#v", anchors)
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		t.Fatal(err)
	}
	planScenarios := make(map[string]struct{}, len(plan))
	for _, scenario := range plan {
		planScenarios[scenario.ID] = struct{}{}
	}
	if _, repeated := planScenarios[request.PreviousScenarioID]; repeated {
		t.Fatalf("different-angle plan repeated %s", request.PreviousScenarioID)
	}
	if !threadPostPortfolioHasObjectiveAnchor(anchors, planScenarios) {
		t.Fatalf("plan has no alternative trial anchor: %#v", planScenarios)
	}
	result, err := NewFake().GenerateThreadPost(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	portfolio := make(map[string]struct{})
	for _, candidate := range result.Audit.Candidates {
		if candidate.Eligible && candidate.Considered {
			portfolio[candidate.ScenarioID] = struct{}{}
		}
	}
	if _, repeated := portfolio[request.PreviousScenarioID]; repeated || !threadPostPortfolioHasObjectiveAnchor(anchors, portfolio) {
		t.Fatalf("invalid different-angle portfolio: %#v", portfolio)
	}
}

func TestFakeThreadPostUsesSafeExactExcerptFromLongQuotedMaterial(t *testing.T) {
	material := "Педагог отметил: «На занятии в 18:30 мы сначала долго проговариваем строку песни, затем слушаем согласные, отмечаем паузы, проверяем дыхание и только после этого негромко соединяем весь куплет с мелодией»."
	request := ThreadPostRequest{
		Voice: domain.ThreadVoiceBelcanto, Objective: domain.ThreadObjectiveTrust,
		MaterialKind: domain.ThreadMaterialText, Material: material,
	}
	result, err := NewFake().GenerateThreadPost(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.MaterialBasis != "material" || result.Evidence == "" || !strings.Contains(material, result.Evidence) {
		t.Fatalf("unsafe fake evidence = %q", result.Evidence)
	}
	if _, valid := threadPostQuotedSpans(result.Text); !valid {
		t.Fatalf("fake result has unmatched quote: %q", result.Text)
	}
	for digit := range threadPostDigitTokens(result.Text) {
		if _, supported := threadPostDigitTokens(result.Evidence)[digit]; !supported {
			t.Fatalf("unsupported digit %q in %q", digit, result.Text)
		}
	}
}

func testScenarioConcepts(request normalizedThreadPostRequest, plan []threadPostScenario, evidenceByScenario map[string]string) []ThreadPostConcept {
	concepts := make([]ThreadPostConcept, 0, len(plan))
	for index, scenario := range plan {
		basis, evidence := "none", ""
		if excerpt := evidenceByScenario[scenario.ID]; excerpt != "" {
			basis, evidence = "material", excerpt
		} else if scenario.RequiresMaterial {
			basis, evidence = "material", request.Material
		}
		concepts = append(concepts, ThreadPostConcept{
			ID: "C" + twoDigits(index+1), ScenarioID: scenario.ID, Mechanism: scenario.Mechanism,
			Angle: "Конкретный живой угол для сценария.", Hook: "Человеческое начало.", Ending: "Понятный повод отреагировать.",
			MaterialBasis: basis, Evidence: evidence,
		})
	}
	return concepts
}

func testScenarioFinalists(request normalizedThreadPostRequest, plan []threadPostScenario) []ThreadPostResult {
	byScenario := make(map[string]fakeThreadPost)
	for _, post := range fakeThreadPostBank(request.Voice) {
		if _, exists := byScenario[post.scenarioID]; !exists {
			byScenario[post.scenarioID] = post
		}
	}
	results := make([]ThreadPostResult, 0, threadPostFinalistCount)
	mechanisms := make(map[string]struct{})
	for _, scenario := range plan {
		post, ok := byScenario[scenario.ID]
		if !ok {
			continue
		}
		if len(results) == threadPostFinalistCount-1 && len(mechanisms) < 3 {
			continue
		}
		results = append(results, ThreadPostResult{
			Goal: string(request.Objective), Objective: request.Objective,
			ScenarioID: scenario.ID, Mechanism: scenario.Mechanism, MaterialBasis: "none", Text: post.text,
		})
		mechanisms[scenario.Mechanism] = struct{}{}
		if len(results) == threadPostFinalistCount {
			break
		}
	}
	return results
}

func materialEvidenceForPlan(plan []threadPostScenario) map[string]string {
	excerpts := []string{
		"Педагог предложил сначала проговорить сложную строку, а затем спеть её в разговорном ритме.",
		"На занятии голос сорвался на высокой ноте, и педагог спокойно предложил повторить короткий фрагмент.",
		"Перед выступлением участники вместе проверили микрофон и начало песни.",
		"Для проверки тональности педагог предложил негромко спеть первый куплет.",
	}
	result := make(map[string]string)
	index := 0
	for _, scenario := range plan {
		if !scenario.RequiresMaterial {
			continue
		}
		result[scenario.ID] = excerpts[index%len(excerpts)]
		index++
	}
	return result
}

func finalistsByScenario(
	t *testing.T,
	request normalizedThreadPostRequest,
	plan []threadPostScenario,
	scenarioIDs []string,
	evidenceByScenario map[string]string,
) []ThreadPostResult {
	t.Helper()
	planByID := make(map[string]threadPostScenario, len(plan))
	for _, scenario := range plan {
		planByID[scenario.ID] = scenario
	}
	bankByScenario := make(map[string]fakeThreadPost)
	for _, candidate := range fakeThreadPostBank(request.Voice) {
		if _, exists := bankByScenario[candidate.scenarioID]; !exists {
			bankByScenario[candidate.scenarioID] = candidate
		}
	}
	results := make([]ThreadPostResult, 0, len(scenarioIDs))
	for _, scenarioID := range scenarioIDs {
		scenario, exists := planByID[scenarioID]
		if !exists {
			t.Fatalf("scenario %s is absent from plan", scenarioID)
		}
		result := ThreadPostResult{
			Goal: string(request.Objective), Objective: request.Objective,
			ScenarioID: scenario.ID, Mechanism: scenario.Mechanism, MaterialBasis: "none",
		}
		if scenario.RequiresMaterial {
			result.MaterialBasis = "material"
			result.Evidence = evidenceByScenario[scenarioID]
			result.Text = result.Evidence
			if result.Evidence == "" {
				t.Fatalf("scenario %s has no material evidence", scenarioID)
			}
		} else {
			candidate, ok := bankByScenario[scenarioID]
			if !ok {
				t.Fatalf("scenario %s has no evergreen fixture", scenarioID)
			}
			result.Text = candidate.text
		}
		results = append(results, result)
	}
	return results
}

func twoDigits(value int) string {
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
