package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

const minimumBlindReviewCandidates = 3

type threadAnthropicCall struct {
	Structured []byte
	Model      string
	RequestID  string
	Usage      domain.Usage
}

// GenerateThreadPost deliberately uses two independent Anthropic calls on the
// normal path: a high-effort private exploration that exposes five finalists,
// followed by a blind editorial review. Only the locally validated winner is
// returned to Telegram; all checkable finalist data remains in Audit.
func (provider *AnthropicProvider) GenerateThreadPost(ctx context.Context, request ThreadPostRequest) (ThreadPostResult, error) {
	if provider == nil {
		return ThreadPostResult{}, fmt.Errorf("%w: nil Anthropic provider", ErrConfiguration)
	}
	normalized, err := normalizeThreadPostRequest(request)
	if err != nil {
		return ThreadPostResult{}, err
	}
	recipe := selectThreadPostRecipe(normalized)
	audit := ThreadPostAudit{
		GenerationID: normalized.GenerationID, RecipeID: recipe.ID,
		ExplorationGoal: threadPostExplorationGoal, GeneratorProvider: providerAnthropic,
		ReviewerProvider: providerAnthropic,
	}
	prompt := buildThreadPostPrompt(normalized)

	generationCtx, cancelGeneration := threadPostStageContext(ctx, provider.timeout+5*time.Second)
	candidates, generationErr := provider.generateThreadPostCandidates(generationCtx, normalized, prompt, 1, &audit)
	cancelGeneration()
	pool := append([]threadPostCandidate(nil), candidates...)
	repairAttempted := shouldRepairThreadPostGeneration(generationErr, len(candidates))
	if repairAttempted {
		if err := ctx.Err(); err != nil {
			return ThreadPostResult{}, err
		}
		reasons := threadPostRepairReasons(audit.Candidates, generationErr)
		repairPrompt := buildThreadPostRepairPrompt(prompt, reasons)
		repairCtx, cancelRepair := threadPostStageContext(ctx, provider.timeout+5*time.Second)
		repaired, repairErr := provider.generateThreadPostCandidates(repairCtx, normalized, repairPrompt, 2, &audit)
		cancelRepair()
		pool = append(pool, repaired...)
		if repairErr != nil {
			generationErr = repairErr
		} else {
			generationErr = nil
		}
	}

	candidates = selectThreadPostFinalists(pool, audit.Candidates, threadPostFinalistCount)
	if repairAttempted && len(candidates) > 0 && len(candidates) < threadPostFinalistCount {
		candidates = appendCuratedThreadPostFinalists(normalized, candidates, &audit, threadPostFinalistCount)
	}
	if len(candidates) < threadPostFinalistCount {
		if err := ctx.Err(); err != nil {
			return ThreadPostResult{}, err
		}
		if generationErr != nil && !mayUseCuratedThreadFallback(generationErr) {
			return ThreadPostResult{}, generationErr
		}
		reason := safeThreadPostFallbackReason(generationErr)
		return curatedThreadPostFallback(normalized, reason, audit)
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
	reviewBody, err := provider.threadPostReviewRequestBody(reviewPrompt, reviewSchema)
	if err != nil {
		return ThreadPostResult{}, fmt.Errorf("%w: encode Anthropic Threads review request: %v", ErrInvalidRequest, err)
	}
	audit.ReviewCalls++
	reviewCtx, cancelReview := threadPostStageContext(ctx, 5*time.Second)
	reviewCall, reviewErr := provider.callThreadPostStage(reviewCtx, reviewBody)
	cancelReview()
	addUsage(&audit.ReviewUsage, reviewCall.Usage)
	audit.ReviewerModel = reviewCall.Model
	audit.ReviewerRequestID = reviewCall.RequestID

	var winner threadPostCandidate
	if reviewErr == nil {
		decision, decodeErr := decodeThreadPostReview(reviewCall.Structured, candidates)
		if decodeErr == nil {
			winner, err = applyThreadPostReviewDecision(candidates, &audit, decision)
			if err == nil {
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
			} else {
				reviewErr = newInvalidOutputError(providerAnthropic, reviewCall.RequestID, err)
			}
		} else {
			reviewErr = newInvalidOutputError(providerAnthropic, reviewCall.RequestID, decodeErr)
		}
	}
	if reviewErr != nil || winner.ReviewerID == "" {
		if err := ctx.Err(); err != nil {
			return ThreadPostResult{}, err
		}
		winner = bestLocalThreadPostCandidate(candidates, audit.Candidates)
		audit.SelectionMode = "local_review_fallback"
		audit.DecisionReason = "Independent reviewer was unavailable; selected the highest deterministic local quality score."
		audit.ReviewerError = safeThreadPostFallbackReason(reviewErr)
		winner.Result.Visual = ThreadPostVisualRecommendation{Mode: "text_only", Query: defaultThreadPhotoQuery}
	}

	markThreadPostAuditWinner(&audit, winner)
	winner.Result.Provider = providerAnthropic
	winner.Result.Model = audit.GeneratorModel
	if winner.Result.Model == "" {
		winner.Result.Model = provider.model
	}
	winner.Result.Usage = sumUsage(audit.GenerationUsage, audit.ReviewUsage)
	winner.Result.Audit = audit
	return winner.Result, nil
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

func shouldRepairThreadPostGeneration(err error, candidateCount int) bool {
	if err == nil {
		return candidateCount < threadPostFinalistCount
	}
	var providerErr *ProviderError
	return errors.As(err, &providerErr) && strings.HasPrefix(providerErr.Code, "invalid_output_thread_")
}

func selectThreadPostFinalists(
	pool []threadPostCandidate,
	audits []ThreadPostCandidateAudit,
	limit int,
) []threadPostCandidate {
	ordered := append([]threadPostCandidate(nil), pool...)
	sort.SliceStable(ordered, func(left, right int) bool {
		leftAudit := audits[ordered[left].AuditIndex]
		rightAudit := audits[ordered[right].AuditIndex]
		if leftAudit.Local.Score != rightAudit.Local.Score {
			return leftAudit.Local.Score > rightAudit.Local.Score
		}
		if localLengthDistance(leftAudit.Local.RuneCount) != localLengthDistance(rightAudit.Local.RuneCount) {
			return localLengthDistance(leftAudit.Local.RuneCount) < localLengthDistance(rightAudit.Local.RuneCount)
		}
		if leftAudit.Attempt != rightAudit.Attempt {
			return leftAudit.Attempt < rightAudit.Attempt
		}
		return leftAudit.SourceSlot < rightAudit.SourceSlot
	})
	unique := make([]threadPostCandidate, 0, min(limit, len(pool)))
	seen := make(map[string]struct{}, len(pool))
	selectedTexts := make([]string, 0, min(limit, len(pool)))
	for _, candidate := range ordered {
		key := canonicalThreadPost(candidate.Result.Text)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		nearDuplicate := false
		for _, selected := range selectedTexts {
			if threadPostsNearDuplicate(candidate.Result.Text, selected) {
				nearDuplicate = true
				break
			}
		}
		if nearDuplicate {
			continue
		}
		seen[key] = struct{}{}
		selectedTexts = append(selectedTexts, candidate.Result.Text)
		unique = append(unique, candidate)
	}
	if len(unique) > limit {
		unique = unique[:limit]
	}
	return unique
}

func appendCuratedThreadPostFinalists(
	normalized normalizedThreadPostRequest,
	candidates []threadPostCandidate,
	audit *ThreadPostAudit,
	limit int,
) []threadPostCandidate {
	excluded := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		excluded[canonicalThreadPost(candidate.Result.Text)] = struct{}{}
	}
	fills := curatedThreadPosts(normalized, limit-len(candidates), excluded)
	for index, result := range fills {
		auditIndex := len(audit.Candidates)
		audit.Candidates = append(audit.Candidates, ThreadPostCandidateAudit{
			Attempt: audit.GenerationCalls + 1, SourceSlot: fmt.Sprintf("curated_fill_%d", index+1),
			Goal: result.Goal, Text: result.Text, Eligible: true, Local: scoreThreadPostQuality(result.Text),
		})
		candidates = append(candidates, threadPostCandidate{Result: result, AuditIndex: auditIndex})
	}
	return selectThreadPostFinalists(candidates, audit.Candidates, limit)
}

func (provider *AnthropicProvider) generateThreadPostCandidates(
	ctx context.Context,
	normalized normalizedThreadPostRequest,
	prompt string,
	attempt int,
	audit *ThreadPostAudit,
) ([]threadPostCandidate, error) {
	body, err := provider.threadPostGenerationRequestBody(prompt)
	if err != nil {
		return nil, fmt.Errorf("%w: encode Anthropic Threads request: %v", ErrInvalidRequest, err)
	}
	audit.GenerationCalls++
	call, callErr := provider.callThreadPostStage(ctx, body)
	addUsage(&audit.GenerationUsage, call.Usage)
	if call.Model != "" {
		audit.GeneratorModel = call.Model
	}
	if call.RequestID != "" {
		audit.GenerationRequestIDs = append(audit.GenerationRequestIDs, call.RequestID)
	}
	if callErr != nil {
		return nil, callErr
	}
	candidates, candidatesAudit, decodeErr := decodeThreadPostCandidates(call.Structured, normalized, attempt)
	offset := len(audit.Candidates)
	for index := range candidates {
		candidates[index].AuditIndex += offset
	}
	audit.Candidates = append(audit.Candidates, candidatesAudit...)
	if decodeErr != nil {
		return nil, newInvalidOutputError(providerAnthropic, call.RequestID, decodeErr)
	}
	if len(candidates) < minimumBlindReviewCandidates {
		return candidates, &ProviderError{
			Provider: providerAnthropic, Code: "invalid_output_thread_insufficient_finalists", RequestID: call.RequestID,
			Err: fmt.Errorf("%w: fewer than three safe Threads finalists", ErrInvalidResponse),
		}
	}
	return candidates, nil
}

func buildThreadPostRepairPrompt(original string, reasons []string) string {
	if len(reasons) == 0 {
		reasons = []string{"invalid_output_thread_semantic"}
	}
	return original + "\n\nThe previous finalist set did not yield enough safe publication candidates. Safe validator categories: " + strings.Join(reasons, ", ") + ". Privately explore at least ten entirely fresh approaches from the original editorial controls, correct every listed condition in all five returned finalists, and do not reuse or reveal rejected text, scratch work, or private reasoning."
}

func threadPostRepairReasons(audits []ThreadPostCandidateAudit, generationErr error) []string {
	seen := make(map[string]struct{})
	for _, candidate := range audits {
		if candidate.ValidationCode != "" {
			seen[candidate.ValidationCode] = struct{}{}
		}
	}
	if generationErr != nil {
		seen[safeThreadPostFallbackReason(generationErr)] = struct{}{}
	}
	reasons := make([]string, 0, len(seen))
	for reason := range seen {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return reasons
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
			Goal: result.Goal, Text: result.Text, Eligible: true, Considered: true,
			Local: scoreThreadPostQuality(result.Text),
		})
		candidates = append(candidates, threadPostCandidate{Result: result, AuditIndex: auditIndex})
	}
	assignBlindReviewerIDs(candidates, audit.Candidates, normalized.Seed, audit.GenerationCalls+1)
	winner := bestLocalThreadPostCandidate(candidates, audit.Candidates)
	audit.SelectionMode = "curated_fallback"
	audit.DecisionReason = "Anthropic generation was unavailable; selected the strongest of five validated local editorial finalists."
	markThreadPostAuditWinner(&audit, winner)
	result := winner.Result
	result.Provider = providerCurated
	result.Model = "belcanto-editorial-v2"
	result.FallbackReason = reason
	result.Visual = ThreadPostVisualRecommendation{Mode: "text_only", Query: defaultThreadPhotoQuery}
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

func (provider *AnthropicProvider) threadPostGenerationRequestBody(prompt string) ([]byte, error) {
	payload := anthropicRequest{
		Model:     provider.model,
		MaxTokens: provider.maxTokens,
		System:    threadPostSystemPrompt,
		Messages: []anthropicMessage{{
			Role: "user", Content: []anthropicContent{{Type: "text", Text: prompt}},
		}},
		OutputConfig: anthropicOutputConfig{
			Effort: "high", Format: anthropicFormat{Type: "json_schema", Schema: threadPostJSONSchema()},
		},
	}
	return json.Marshal(payload)
}

func (provider *AnthropicProvider) threadPostReviewRequestBody(prompt string, schema json.RawMessage) ([]byte, error) {
	effort := provider.effort
	if effort == "low" {
		effort = "medium"
	}
	payload := anthropicRequest{
		Model:     provider.model,
		MaxTokens: provider.maxTokens,
		System:    threadPostReviewerSystemPrompt,
		Messages: []anthropicMessage{{
			Role: "user", Content: []anthropicContent{{Type: "text", Text: prompt}},
		}},
		OutputConfig: anthropicOutputConfig{
			Effort: effort, Format: anthropicFormat{Type: "json_schema", Schema: schema},
		},
	}
	return json.Marshal(payload)
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
	attemptCtx, cancel := context.WithTimeout(ctx, provider.timeout)
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
