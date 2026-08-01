package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

type threadAnthropicCall struct {
	Structured []byte
	Model      string
	RequestID  string
	Usage      domain.Usage
}

// GenerateThreadPost uses three explicit high-effort stages: twelve auditable
// scenario concepts, five grounded and mechanically diverse finalists, and an
// independent blind review. Hidden chain-of-thought is neither requested nor
// stored; the concepts are concise editorial deliverables.
func (provider *AnthropicProvider) GenerateThreadPost(ctx context.Context, request ThreadPostRequest) (ThreadPostResult, error) {
	if provider == nil {
		return ThreadPostResult{}, fmt.Errorf("%w: nil Anthropic provider", ErrConfiguration)
	}
	normalized, err := normalizeThreadPostRequest(request)
	if err != nil {
		return ThreadPostResult{}, err
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		return ThreadPostResult{}, err
	}
	audit := ThreadPostAudit{
		GenerationID: normalized.GenerationID, Objective: normalized.Objective,
		RecipeID:        "scenario-engine-v3",
		ExplorationGoal: threadPostExplorationGoal, GeneratorProvider: providerAnthropic,
		ReviewerProvider: providerAnthropic,
	}

	concepts, conceptErr := provider.runThreadPostConceptStage(ctx, normalized, plan, &audit)
	if conceptErr != nil {
		if normalized.Material != "" || !mayUseCuratedThreadFallback(conceptErr) {
			return ThreadPostResult{}, conceptErr
		}
		return curatedThreadPostFallback(normalized, safeThreadPostFallbackReason(conceptErr), audit)
	}
	candidates, writerErr := provider.runThreadPostWriterStage(ctx, normalized, concepts, &audit)
	if writerErr != nil {
		if normalized.Material != "" || !mayUseCuratedThreadFallback(writerErr) {
			return ThreadPostResult{}, writerErr
		}
		return curatedThreadPostFallback(normalized, safeThreadPostFallbackReason(writerErr), audit)
	}
	assignBlindReviewerIDs(candidates, audit.Candidates, normalized.Seed, audit.GenerationCalls+1)
	for _, candidate := range candidates {
		audit.Candidates[candidate.AuditIndex].Considered = true
	}

	reviewPrompt := buildThreadPostReviewPrompt(normalized, candidates)
	reviewSchema, err := threadPostReviewJSONSchema(candidates)
	if err != nil {
		return ThreadPostResult{}, err
	}

	var (
		winner         threadPostCandidate
		reviewErr      error
		lastReviewCall threadAnthropicCall
	)
	for attempt := 1; attempt <= 2; attempt++ {
		attemptPrompt := reviewPrompt
		if attempt == 2 {
			attemptPrompt = buildThreadPostReviewRepairPrompt(reviewPrompt)
		}
		reviewBody, bodyErr := provider.threadPostReviewRequestBody(attemptPrompt, reviewSchema)
		if bodyErr != nil {
			return ThreadPostResult{}, fmt.Errorf("%w: encode Anthropic Threads review request: %v", ErrInvalidRequest, bodyErr)
		}
		audit.ReviewCalls++
		reviewCtx, cancelReview := threadPostStageContext(ctx, 5*time.Second)
		reviewCall, callErr := provider.callThreadPostStage(reviewCtx, reviewBody)
		cancelReview()
		lastReviewCall = reviewCall
		recordThreadPostReviewCall(&audit, reviewCall, callErr)
		if callErr != nil {
			// Transport retries are already exhausted inside callThreadPostStage.
			// Authentication, refusal, truncation, and all other API failures must
			// not be disguised as a semantic structured-output repair.
			reviewErr = callErr
			break
		}

		decision, decodeErr := decodeThreadPostReview(reviewCall.Structured, candidates)
		if decodeErr != nil {
			reviewErr = newInvalidOutputError(providerAnthropic, reviewCall.RequestID, decodeErr)
			if attempt == 1 {
				continue
			}
			break
		}
		winner, err = applyThreadPostReviewDecision(candidates, &audit, decision)
		if err != nil {
			reviewErr = newInvalidOutputError(providerAnthropic, reviewCall.RequestID, err)
			if attempt == 1 {
				continue
			}
			break
		}

		reviewErr = nil
		audit.ReviewerWinnerID = decision.DeclaredWinner
		if decision.ScoreOverride {
			audit.SelectionMode = "review_score_override"
			audit.DecisionReason = "The reviewer's declared winner did not match its scorecards; selected the highest weighted score."
			winner.Result.Visual = ThreadPostVisualRecommendation{Mode: "text_only", Query: defaultThreadPhotoQuery}
		} else {
			audit.SelectionMode = "anthropic_blind_review"
			audit.DecisionReason = decision.Reason
			winner.Result.Visual = decision.Visual
		}
		break
	}
	if reviewErr != nil || winner.ReviewerID == "" {
		if err := ctx.Err(); err != nil {
			return ThreadPostResult{}, err
		}
		// A local score cannot semantically prove that a paraphrase is entailed
		// by operator material. Never publish a material-backed candidate when
		// the independent grounding review is unavailable or invalid.
		if normalized.Material != "" {
			if reviewErr != nil {
				return ThreadPostResult{}, reviewErr
			}
			return ThreadPostResult{}, newInvalidOutputError(providerAnthropic, lastReviewCall.RequestID, fmt.Errorf("%w: grounded Threads review produced no winner", ErrInvalidResponse))
		}
		winner = bestLocalThreadPostCandidate(candidates, audit.Candidates)
		audit.SelectionMode = "local_review_fallback"
		audit.DecisionReason = "Independent reviewer was unavailable; selected the highest deterministic local quality score."
		audit.ReviewerError = safeThreadPostFallbackReason(reviewErr)
		winner.Result.Visual = ThreadPostVisualRecommendation{Mode: "text_only", Query: defaultThreadPhotoQuery}
	}
	// Reviewer free-form output has seen approved material and must never become
	// an external Pexels query. Preserve the editorial mode/brief, but replace
	// every model-supplied query with a deterministic scenario-only value.
	winner.Result.Visual.Query = SafeThreadPhotoQuery(winner.Result.ScenarioID)

	markThreadPostAuditWinner(&audit, winner)
	audit.ScenarioID = winner.Result.ScenarioID
	audit.Mechanism = winner.Result.Mechanism
	winner.Result.Provider = providerAnthropic
	winner.Result.Model = audit.GeneratorModel
	if winner.Result.Model == "" {
		winner.Result.Model = provider.model
	}
	winner.Result.Usage = sumUsage(audit.ConceptUsage, audit.WriterUsage, audit.ReviewUsage)
	winner.Result.Audit = audit
	return winner.Result, nil
}

func (provider *AnthropicProvider) runThreadPostConceptStage(
	ctx context.Context,
	request normalizedThreadPostRequest,
	plan []threadPostScenario,
	audit *ThreadPostAudit,
) ([]ThreadPostConcept, error) {
	schema, err := threadPostConceptJSONSchema(plan)
	if err != nil {
		return nil, err
	}
	prompt := buildThreadPostConceptPrompt(request, plan)
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt == 2 {
			prompt += "\n\nThe previous concept envelope failed strict validation. Return a completely corrected envelope with exactly twelve slots C01 through C12, every assigned scenario exactly once, exact material evidence where required, and no extra fields."
		}
		body, bodyErr := provider.threadPostStageRequestBody(threadPostConceptSystemPrompt, prompt, schema)
		if bodyErr != nil {
			return nil, bodyErr
		}
		audit.ConceptCalls++
		audit.GenerationCalls++
		stageCtx, cancel := threadPostStageContext(ctx, provider.threadTimeout+5*time.Second)
		call, callErr := provider.callThreadPostStage(stageCtx, body)
		cancel()
		addUsage(&audit.ConceptUsage, call.Usage)
		addUsage(&audit.GenerationUsage, call.Usage)
		if call.RequestID != "" {
			audit.ConceptRequestIDs = append(audit.ConceptRequestIDs, call.RequestID)
			audit.GenerationRequestIDs = append(audit.GenerationRequestIDs, call.RequestID)
		}
		if call.Model != "" {
			audit.GeneratorModel = call.Model
		}
		if callErr != nil {
			return nil, callErr
		}
		concepts, conceptsAudit, decodeErr := decodeThreadPostConcepts(call.Structured, request, plan)
		if decodeErr == nil {
			audit.Concepts = conceptsAudit
			return concepts, nil
		}
		if attempt == 2 {
			audit.Concepts = conceptsAudit
			return nil, newInvalidOutputError(providerAnthropic, call.RequestID, decodeErr)
		}
	}
	return nil, fmt.Errorf("%w: unreachable concept stage", ErrInvalidResponse)
}

func (provider *AnthropicProvider) runThreadPostWriterStage(
	ctx context.Context,
	request normalizedThreadPostRequest,
	concepts []ThreadPostConcept,
	audit *ThreadPostAudit,
) ([]threadPostCandidate, error) {
	schema, err := threadPostFinalistJSONSchema(request, concepts)
	if err != nil {
		return nil, err
	}
	prompt := buildThreadPostWriterPrompt(request, concepts)
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt == 2 {
			prompt += "\n\nThe previous finalist envelope failed strict validation. Rewrite all five posts: five different scenario_id values, at least four mechanisms, no more than three question endings, no unsupported fact, digit, or quotation, and exact evidence excerpts. Include an objective anchor from: " + strings.Join(threadPostObjectiveAnchorIDs(request), ", ") + ". When approved material exists, at least two finalists must be material-backed and keep their concepts' exact evidence."
		}
		body, bodyErr := provider.threadPostStageRequestBody(threadPostWriterSystemPrompt, prompt, schema)
		if bodyErr != nil {
			return nil, bodyErr
		}
		audit.WriterCalls++
		audit.GenerationCalls++
		stageCtx, cancel := threadPostStageContext(ctx, provider.threadTimeout+5*time.Second)
		call, callErr := provider.callThreadPostStage(stageCtx, body)
		cancel()
		addUsage(&audit.WriterUsage, call.Usage)
		addUsage(&audit.GenerationUsage, call.Usage)
		if call.RequestID != "" {
			audit.WriterRequestIDs = append(audit.WriterRequestIDs, call.RequestID)
			audit.GenerationRequestIDs = append(audit.GenerationRequestIDs, call.RequestID)
		}
		if call.Model != "" {
			audit.GeneratorModel = call.Model
		}
		if callErr != nil {
			return nil, callErr
		}
		candidates, candidateAudit, decodeErr := decodeThreadPostFinalists(call.Structured, request, concepts, attempt)
		offset := len(audit.Candidates)
		for index := range candidates {
			candidates[index].AuditIndex += offset
		}
		audit.Candidates = append(audit.Candidates, candidateAudit...)
		if decodeErr == nil {
			return candidates, nil
		}
		if attempt == 2 {
			return nil, newInvalidOutputError(providerAnthropic, call.RequestID, decodeErr)
		}
	}
	return nil, fmt.Errorf("%w: unreachable writer stage", ErrInvalidResponse)
}

func threadPostStageContext(parent context.Context, reserve time.Duration) (context.Context, context.CancelFunc) {
	deadline, ok := parent.Deadline()
	if !ok {
		return context.WithCancel(parent)
	}
	remaining := time.Until(deadline)
	budget := remaining - reserve
	if budget <= 0 {
		budget = remaining / 2
	}
	if budget <= 0 {
		budget = time.Millisecond
	}
	return context.WithTimeout(parent, budget)
}

func applyThreadPostReviewDecision(
	candidates []threadPostCandidate,
	audit *ThreadPostAudit,
	decision threadPostReviewDecision,
) (threadPostCandidate, error) {
	var winner threadPostCandidate
	for _, candidate := range candidates {
		score, ok := decision.Scores[candidate.ReviewerID]
		if !ok {
			return threadPostCandidate{}, fmt.Errorf("%w: missing reviewer score for safe Threads candidate", ErrInvalidResponse)
		}
		audit.Candidates[candidate.AuditIndex].Review = score
		if candidate.ReviewerID == decision.WinnerID {
			winner = candidate
		}
	}
	if winner.ReviewerID == "" {
		return threadPostCandidate{}, fmt.Errorf("%w: reviewer winner was not a safe Threads candidate", ErrInvalidResponse)
	}
	return winner, nil
}

func bestLocalThreadPostCandidate(candidates []threadPostCandidate, audits []ThreadPostCandidateAudit) threadPostCandidate {
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		left := audits[candidate.AuditIndex]
		right := audits[best.AuditIndex]
		if left.Local.Score > right.Local.Score ||
			(left.Local.Score == right.Local.Score && localLengthDistance(left.Local.RuneCount) < localLengthDistance(right.Local.RuneCount)) ||
			(left.Local.Score == right.Local.Score && localLengthDistance(left.Local.RuneCount) == localLengthDistance(right.Local.RuneCount) && candidate.ReviewerID < best.ReviewerID) {
			best = candidate
		}
	}
	return best
}

func localLengthDistance(runes int) int {
	const target = 190
	if runes > target {
		return runes - target
	}
	return target - runes
}

func markThreadPostAuditWinner(audit *ThreadPostAudit, winner threadPostCandidate) {
	audit.DeliveredWinnerID = winner.ReviewerID
	for index := range audit.Candidates {
		audit.Candidates[index].Selected = index == winner.AuditIndex
	}
}

func curatedThreadPostFallback(normalized normalizedThreadPostRequest, reason string, audit ThreadPostAudit) (ThreadPostResult, error) {
	results := curatedThreadPosts(normalized, threadPostFinalistCount, nil)
	if len(results) != threadPostFinalistCount {
		return ThreadPostResult{}, fmt.Errorf("curated Threads finalist invariant: %w", ErrInvalidResponse)
	}
	candidates := make([]threadPostCandidate, 0, len(results))
	for index, result := range results {
		auditIndex := len(audit.Candidates)
		audit.Candidates = append(audit.Candidates, ThreadPostCandidateAudit{
			Attempt: audit.GenerationCalls + 1, SourceSlot: fmt.Sprintf("curated_%d", index+1),
			Goal: result.Goal, Objective: result.Objective, ScenarioID: result.ScenarioID, Mechanism: result.Mechanism,
			MaterialBasis: result.MaterialBasis, Evidence: result.Evidence,
			Text: result.Text, Eligible: true, Considered: true,
			Local: scoreThreadPostQuality(result.Text),
		})
		candidates = append(candidates, threadPostCandidate{Result: result, AuditIndex: auditIndex})
	}
	assignBlindReviewerIDs(candidates, audit.Candidates, normalized.Seed, audit.GenerationCalls+1)
	winner := bestLocalThreadPostCandidate(candidates, audit.Candidates)
	audit.SelectionMode = "curated_fallback"
	audit.DecisionReason = "Anthropic generation was unavailable; selected the strongest of five validated local editorial finalists."
	markThreadPostAuditWinner(&audit, winner)
	audit.Objective = normalized.Objective
	audit.ScenarioID = winner.Result.ScenarioID
	audit.Mechanism = winner.Result.Mechanism
	result := winner.Result
	result.Provider = providerCurated
	result.Model = "belcanto-scenario-v3"
	result.FallbackReason = reason
	result.Visual = ThreadPostVisualRecommendation{Mode: "text_only", Query: SafeThreadPhotoQuery(result.ScenarioID)}
	result.Usage = sumUsage(audit.GenerationUsage, audit.ReviewUsage)
	result.Audit = audit
	return result, nil
}

func mayUseCuratedThreadFallback(err error) bool {
	if err == nil {
		return true
	}
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		return true
	}
	if providerErr.Status == http.StatusBadRequest || providerErr.Status == http.StatusUnauthorized || providerErr.Status == http.StatusForbidden || providerErr.Status == http.StatusNotFound {
		return false
	}
	return true
}

func safeThreadPostFallbackReason(err error) string {
	if err == nil {
		return "unknown_failure"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && strings.TrimSpace(providerErr.Code) != "" {
		return providerErr.Code
	}
	return "provider_failure"
}

func (provider *AnthropicProvider) threadPostStageRequestBody(system, prompt string, schema json.RawMessage) ([]byte, error) {
	payload := anthropicRequest{
		Model:     provider.model,
		MaxTokens: provider.threadMaxTokens,
		System:    system,
		Messages: []anthropicMessage{{
			Role: "user", Content: []anthropicContent{{Type: "text", Text: prompt}},
		}},
		OutputConfig: anthropicOutputConfig{
			Effort: "high", Format: anthropicFormat{Type: "json_schema", Schema: schema},
		},
	}
	return json.Marshal(payload)
}

func (provider *AnthropicProvider) threadPostReviewRequestBody(prompt string, schema json.RawMessage) ([]byte, error) {
	payload := anthropicRequest{
		Model:     provider.model,
		MaxTokens: provider.threadMaxTokens,
		System:    threadPostReviewerSystemPrompt,
		Messages: []anthropicMessage{{
			Role: "user", Content: []anthropicContent{{Type: "text", Text: prompt}},
		}},
		OutputConfig: anthropicOutputConfig{
			Effort: "high", Format: anthropicFormat{Type: "json_schema", Schema: schema},
		},
	}
	return json.Marshal(payload)
}

func recordThreadPostReviewCall(audit *ThreadPostAudit, call threadAnthropicCall, callErr error) {
	if audit == nil {
		return
	}
	addUsage(&audit.ReviewUsage, call.Usage)
	if call.Model != "" {
		audit.ReviewerModel = call.Model
	}
	requestID := call.RequestID
	if requestID == "" {
		var providerErr *ProviderError
		if errors.As(callErr, &providerErr) {
			requestID = providerErr.RequestID
		}
	}
	if requestID != "" {
		audit.ReviewerRequestID = requestID
	}
}

func (provider *AnthropicProvider) callThreadPostStage(ctx context.Context, body []byte) (threadAnthropicCall, error) {
	var last threadAnthropicCall
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		call, retryAfter, retryable, err := provider.doThreadPostRequest(ctx, body)
		last = call
		if err == nil {
			return call, nil
		}
		if !retryable || attempt >= provider.maxRetries {
			return call, err
		}
		delay := retryAfter
		if delay <= 0 {
			delay = provider.backoff(attempt + 1)
		}
		if err := sleepContext(ctx, delay); err != nil {
			return call, err
		}
	}
}

func (provider *AnthropicProvider) doThreadPostRequest(ctx context.Context, body []byte) (threadAnthropicCall, time.Duration, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, provider.threadTimeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return threadAnthropicCall{}, 0, false, fmt.Errorf("%w: build Threads request: %v", ErrConfiguration, err)
	}
	httpRequest.Header.Set("content-type", "application/json")
	httpRequest.Header.Set("accept", "application/json")
	httpRequest.Header.Set("x-api-key", provider.apiKey)
	httpRequest.Header.Set("anthropic-version", anthropicVersion)
	httpRequest.Header.Set("user-agent", "witty-reply/1")

	response, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return threadAnthropicCall{}, 0, false, ctx.Err()
		}
		retryable := isRetryableNetworkError(err)
		providerErr := &ProviderError{Provider: providerAnthropic, Code: "transport_error", Retryable: retryable, Err: err}
		return threadAnthropicCall{}, 0, retryable, providerErr
	}
	defer response.Body.Close()

	raw, readErr := readLimited(response.Body, defaultMaxResultBytes)
	if readErr != nil {
		retryable := response.StatusCode >= 500 || (response.StatusCode >= 200 && response.StatusCode < 300 && isRetryableNetworkError(readErr))
		providerErr := &ProviderError{
			Provider: providerAnthropic, Code: "read_error", Status: response.StatusCode,
			RequestID: response.Header.Get("request-id"), Retryable: retryable, Err: readErr,
		}
		return threadAnthropicCall{}, 0, retryable, providerErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, retryAfter, retryable, apiErr := provider.apiError(response, raw)
		return threadAnthropicCall{}, retryAfter, retryable, apiErr
	}

	var envelope anthropicResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return threadAnthropicCall{}, 0, false, &ProviderError{
			Provider: providerAnthropic, Code: "invalid_json", Status: response.StatusCode,
			RequestID: response.Header.Get("request-id"), Err: fmt.Errorf("%w: decode API envelope", ErrInvalidResponse),
		}
	}
	call := threadAnthropicCall{
		Model: envelope.Model, RequestID: response.Header.Get("request-id"),
	}
	if call.Model == "" {
		call.Model = provider.model
	}
	if envelope.Usage.InputTokens < 0 || envelope.Usage.OutputTokens < 0 || envelope.Usage.CacheCreationInputTokens < 0 || envelope.Usage.CacheReadInputTokens < 0 {
		return call, 0, false, &ProviderError{Provider: providerAnthropic, Code: "invalid_usage", RequestID: call.RequestID, Err: ErrInvalidResponse}
	}
	call.Usage = domain.Usage{
		InputTokens:  envelope.Usage.InputTokens + envelope.Usage.CacheCreationInputTokens + envelope.Usage.CacheReadInputTokens,
		OutputTokens: envelope.Usage.OutputTokens,
	}

	switch envelope.StopReason {
	case "end_turn", "stop_sequence":
	case "refusal":
		return call, 0, false, &ProviderError{Provider: providerAnthropic, Code: "refusal", RequestID: call.RequestID, Err: ErrRefused}
	case "max_tokens", "model_context_window_exceeded":
		return call, 0, false, &ProviderError{Provider: providerAnthropic, Code: envelope.StopReason, RequestID: call.RequestID, Err: ErrTruncated}
	default:
		return call, 0, false, &ProviderError{
			Provider: providerAnthropic, Code: "unexpected_stop", RequestID: call.RequestID,
			Err: fmt.Errorf("%w: unexpected stop reason %q", ErrInvalidResponse, envelope.StopReason),
		}
	}

	var structured bytes.Buffer
	for _, block := range envelope.Content {
		if block.Type == "refusal" {
			return call, 0, false, &ProviderError{Provider: providerAnthropic, Code: "refusal", RequestID: call.RequestID, Err: ErrRefused}
		}
		if block.Type == "text" {
			structured.WriteString(block.Text)
		}
	}
	call.Structured = structured.Bytes()
	return call, 0, false, nil
}

func addUsage(total *domain.Usage, addition domain.Usage) {
	if total == nil {
		return
	}
	total.InputTokens += addition.InputTokens
	total.OutputTokens += addition.OutputTokens
}

func sumUsage(values ...domain.Usage) domain.Usage {
	var total domain.Usage
	for _, value := range values {
		addUsage(&total, value)
	}
	return total
}
