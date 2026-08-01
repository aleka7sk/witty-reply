package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

// FakeProvider is deterministic and performs the same request/result
// validation as real providers. It is suitable for tests and local demos.
type FakeProvider struct{}

func NewFake() *FakeProvider {
	return &FakeProvider{}
}

func NewFakeProvider() *FakeProvider {
	return NewFake()
}

func (provider *FakeProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.GenerationResult{}, err
	}
	normalized, _, err := prepareRequest(request, "")
	if err != nil {
		return domain.GenerationResult{}, err
	}

	result := fakeResult(normalized)
	if err := ValidateResultForMode(&result, normalized.Tone, normalized.Mode); err != nil {
		return domain.GenerationResult{}, fmt.Errorf("fake provider invariant: %w", err)
	}
	if err := validateFreshReplies(result.Replies, normalized.PreviousReplies); err != nil {
		return domain.GenerationResult{}, fmt.Errorf("fake provider freshness invariant: %w", err)
	}
	result.Provider = providerFake
	result.Model = "deterministic-v2"
	result.Usage = domain.Usage{}
	return result, nil
}

func (provider *FakeProvider) GenerateThreadPost(ctx context.Context, request ThreadPostRequest) (ThreadPostResult, error) {
	if err := ctx.Err(); err != nil {
		return ThreadPostResult{}, err
	}
	normalized, err := normalizeThreadPostRequest(request)
	if err != nil {
		return ThreadPostResult{}, err
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		return ThreadPostResult{}, err
	}
	results := curatedThreadPosts(normalized, threadPostFinalistCount, nil)
	if len(results) != threadPostFinalistCount {
		return ThreadPostResult{}, fmt.Errorf("fake Threads finalist invariant: %w", ErrInvalidResponse)
	}
	audit := ThreadPostAudit{
		GenerationID: normalized.GenerationID, Objective: normalized.Objective, RecipeID: "scenario-engine-v3",
		ExplorationGoal: threadPostConceptCount, ConceptCalls: 1, WriterCalls: 1, GenerationCalls: 2, GeneratorProvider: providerFake,
		GeneratorModel: "deterministic-threads-v3", SelectionMode: "deterministic_fake",
		DecisionReason: "Deterministic local preview selected one of five scenario-diverse editorial examples.",
		Concepts:       make([]ThreadPostConceptAudit, 0, len(plan)),
		Candidates:     make([]ThreadPostCandidateAudit, 0, threadPostFinalistCount),
	}
	for index, scenario := range plan {
		materialBasis, evidence := "none", ""
		if scenario.RequiresMaterial {
			materialBasis, evidence = "material", fakeThreadPostEvidence(normalized.Material)
		}
		audit.Concepts = append(audit.Concepts, ThreadPostConceptAudit{
			ID: fmt.Sprintf("C%02d", index+1), ScenarioID: scenario.ID, Mechanism: scenario.Mechanism,
			Angle: scenario.Instruction, Hook: "deterministic preview concept", Ending: "scenario-specific ending",
			MaterialBasis: materialBasis, Evidence: evidence, Eligible: true,
		})
	}
	candidates := make([]threadPostCandidate, 0, threadPostFinalistCount)
	for index, finalist := range results {
		audit.Candidates = append(audit.Candidates, ThreadPostCandidateAudit{
			Attempt: 1, SourceSlot: fmt.Sprintf("fake_%d", index+1), Goal: finalist.Goal,
			Objective: finalist.Objective, ScenarioID: finalist.ScenarioID, Mechanism: finalist.Mechanism,
			MaterialBasis: finalist.MaterialBasis, Evidence: finalist.Evidence,
			Text: finalist.Text, Eligible: true, Considered: true, Local: scoreThreadPostQuality(finalist.Text),
		})
		candidates = append(candidates, threadPostCandidate{Result: finalist, AuditIndex: index})
	}
	assignBlindReviewerIDs(candidates, audit.Candidates, normalized.Seed, 1)
	winnerPool := candidates
	if normalized.Material != "" {
		winnerPool = make([]threadPostCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.Result.MaterialBasis == "material" {
				winnerPool = append(winnerPool, candidate)
			}
		}
	}
	if len(winnerPool) == 0 {
		return ThreadPostResult{}, fmt.Errorf("fake Threads material winner invariant: %w", ErrInvalidResponse)
	}
	winner := bestLocalThreadPostCandidate(winnerPool, audit.Candidates)
	markThreadPostAuditWinner(&audit, winner)
	audit.ScenarioID = winner.Result.ScenarioID
	audit.Mechanism = winner.Result.Mechanism
	result := winner.Result
	result.Provider = providerFake
	result.Model = audit.GeneratorModel
	result.Visual = ThreadPostVisualRecommendation{Mode: "text_only", Query: SafeThreadPhotoQuery(result.ScenarioID)}
	result.Audit = audit
	return result, nil
}

func curatedThreadPosts(normalized normalizedThreadPostRequest, limit int, excluded map[string]struct{}) []ThreadPostResult {
	if limit <= 0 {
		return nil
	}
	plan, err := selectThreadPostScenarioPlan(normalized)
	if err != nil {
		return nil
	}
	bank := fakeThreadPostBank(normalized.Voice)
	if normalized.Material != "" {
		bank = groundedFakeThreadPostBank(normalized, plan)
	} else {
		allowed := make(map[string]struct{}, len(plan))
		for _, scenario := range plan {
			allowed[scenario.ID] = struct{}{}
		}
		filtered := make([]fakeThreadPost, 0, len(bank))
		for _, candidate := range bank {
			if _, ok := allowed[candidate.scenarioID]; ok {
				filtered = append(filtered, candidate)
			}
		}
		bank = filtered
	}
	if len(bank) == 0 {
		return nil
	}
	offset := int(normalized.Seed % uint32(len(bank)))
	if normalized.Material == "" && normalized.Voice == "alisher" {
		offset = (offset + 7) % len(bank)
	}
	switch {
	case normalized.Material != "":
		offset = 0
	case normalized.Transform == "wittier":
		offset = (offset + 2) % len(bank)
	case normalized.Transform == "warmer":
		offset = (offset + 4) % len(bank)
	case normalized.Transform == "shorter":
		offset = (offset + 6) % len(bank)
	case normalized.Transform == "different_angle":
		offset = (offset + 8) % len(bank)
	}
	poolLimit := len(bank)
	results := make([]ThreadPostResult, 0, poolLimit)
	seen := make(map[string]struct{}, len(excluded)+limit)
	seenScenarios := make(map[string]struct{}, limit)
	for key := range excluded {
		seen[key] = struct{}{}
	}
	for attempt := 0; attempt < len(bank) && len(results) < poolLimit; attempt++ {
		candidate := bank[(offset+attempt)%len(bank)]
		if _, duplicate := seenScenarios[candidate.scenarioID]; duplicate {
			continue
		}
		result := ThreadPostResult{
			Goal: string(normalized.Objective), Objective: normalized.Objective,
			ScenarioID: candidate.scenarioID, Mechanism: candidate.mechanism,
			MaterialBasis: candidate.materialBasis, Evidence: candidate.evidence, Text: candidate.text,
		}
		if result.MaterialBasis == "" {
			result.MaterialBasis = "none"
		}
		if err := validateThreadPostEvidence(result.MaterialBasis, result.Evidence, normalized, threadPostScenarioByID[result.ScenarioID].RequiresMaterial); err != nil {
			continue
		}
		if err := validateThreadPostResult(&result, normalized); err != nil {
			continue
		}
		if err := validateThreadPostEditorialQuality(result, normalized, scoreThreadPostQuality(result.Text)); err != nil {
			continue
		}
		if normalized.DeliveryCheck != nil && !normalized.DeliveryCheck(result.Text) {
			continue
		}
		key := canonicalThreadPost(result.Text)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		seenScenarios[result.ScenarioID] = struct{}{}
		results = append(results, result)
	}
	if limit == threadPostFinalistCount {
		return selectFakeThreadPosts(results, normalized, limit)
	}
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func selectFakeThreadPosts(pool []ThreadPostResult, request normalizedThreadPostRequest, limit int) []ThreadPostResult {
	anchors := threadPostObjectiveAnchorIDs(request)
	minimumMaterial := 0
	if request.Material != "" {
		minimumMaterial = minimumMaterialBackedFinalistCount
	}
	selected := make([]ThreadPostResult, 0, limit)
	var winner []ThreadPostResult
	var search func(int, int)
	search = func(start, questionEndings int) {
		if winner != nil {
			return
		}
		if len(selected) == limit {
			scenarios := make(map[string]struct{}, limit)
			mechanisms := make(map[string]struct{}, limit)
			materialCount := 0
			for _, candidate := range selected {
				scenarios[candidate.ScenarioID] = struct{}{}
				mechanisms[candidate.Mechanism] = struct{}{}
				if candidate.MaterialBasis == "material" {
					materialCount++
				}
			}
			if len(mechanisms) < 4 || materialCount < minimumMaterial ||
				!threadPostPortfolioHasObjectiveAnchor(anchors, scenarios) {
				return
			}
			if request.PreviousScenarioID != "" && request.Transform != "different_angle" {
				if _, preserved := scenarios[request.PreviousScenarioID]; !preserved {
					return
				}
			}
			winner = append([]ThreadPostResult(nil), selected...)
			return
		}
		if len(pool)-start < limit-len(selected) {
			return
		}
		for index := start; index < len(pool); index++ {
			candidate := pool[index]
			isQuestion := strings.HasSuffix(strings.TrimSpace(candidate.Text), "?")
			if isQuestion && questionEndings >= 3 {
				continue
			}
			nearDuplicate := false
			for _, existing := range selected {
				if existing.ScenarioID == candidate.ScenarioID || threadPostsNearDuplicate(candidate.Text, existing.Text) {
					nearDuplicate = true
					break
				}
			}
			if nearDuplicate {
				continue
			}
			selected = append(selected, candidate)
			nextQuestions := questionEndings
			if isQuestion {
				nextQuestions++
			}
			search(index+1, nextQuestions)
			selected = selected[:len(selected)-1]
			if winner != nil {
				return
			}
		}
	}
	search(0, 0)
	return winner
}

type fakeThreadPost struct {
	goal          string // retained only for the unused legacy fixture bank below
	scenarioID    string
	mechanism     string
	materialBasis string
	evidence      string
	text          string
}

func fakeThreadPostBank(voice string) []fakeThreadPost {
	posts := []fakeThreadPost{
		{scenarioID: "karaoke_archetype", mechanism: "conversation_humor", text: "У каждого стола в караоке есть человек, который весь вечер говорит «я не буду», а потом не отдаёт микрофон. Кто вы за своим столом?"},
		{scenarioID: "song_memory", mechanism: "music_memory", text: "Какую песню вы помните не по словам, а по голосу мамы, папы или бабушки? Иногда семейный плейлист хранится именно так."},
		{scenarioID: "astana_soundtrack", mechanism: "local_identity", text: "Какая песня звучит для вас как ночная дорога по Астане? Нужен не официальный гимн, а ваш личный саундтрек."},
		{scenarioID: "audience_choice", mechanism: "participation", text: "Что разобрать следующим: почему свой голос на записи кажется чужим, как выбрать удобную тональность или куда исчезает дыхание в длинной строке?"},
		{scenarioID: "adult_beginner", mechanism: "recognition", text: "Что неловче на первом занятии: спеть перед педагогом или потом услышать запись собственного голоса? У взрослых обычно есть очень конкретный ответ."},
		{scenarioID: "everyday_voice_humor", mechanism: "conversation_humor", text: "Домашний вокал особенно уверен, пока душ, чайник и пылесос официально входят в состав группы. Кто у вас отвечает за ритм?"},
		{scenarioID: "recording_reaction", mechanism: "recognition", text: "Первое прослушивание своего голосового сообщения: отрицание, торг, попытка удалить и только потом смысл сказанного. Какая стадия ваша?"},
		{scenarioID: "after_work_creativity", mechanism: "lifestyle", text: "Какое занятие после работы возвращает вам ощущение, что день состоял не только из задач? Для кого-то это песня, для кого-то — танец или кисти."},
		{scenarioID: "music_hot_take", mechanism: "conversation", text: "Музыкальная позиция, за которую можно спокойно спорить: песня для караоке должна подходить голосу, а не доказывать уважение к оригинальной тональности."},
		{scenarioID: "mini_voice_experiment", mechanism: "practical", text: "Негромко скажите строчку любимой песни, затем спойте её в том же разговорном настроении. Что сохранилось, а что голос зачем-то решил украсить?"},
		{scenarioID: "finish_the_line", mechanism: "participation", text: "Закончите фразу названием песни: «Если бы эта неделя была припевом, она звучала бы как…»"},
		{scenarioID: "format_choice", mechanism: "qualification", text: "Кому спокойнее впервые запеть один на один, а кому легче, когда рядом ещё пара таких же новичков? Интересно, от чего зависит ваш выбор."},
		{scenarioID: "seven_day_challenge", mechanism: "participation", text: "Музыкальный эксперимент на неделю: каждый день выбирать один припев и замечать только одну новую деталь — дыхание, слово, паузу или настроение."},
		{scenarioID: "question_to_teacher", mechanism: "participation", text: "Какой вопрос о голосе вы давно хотели задать педагогу, но он кажется слишком простым? Именно простые вопросы часто дают самые полезные разборы."},
		{scenarioID: "music_hot_take", mechanism: "conversation", text: "Не всякая любимая песня обязана становиться вашей песней для караоке. Какой трек вы обожаете слушать, но никогда не возьмёте в микрофон?"},
		{scenarioID: "karaoke_archetype", mechanism: "conversation_humor", text: "Караоке-компания делится на тех, кто выбирает песню сердцем, и тех, кто слишком поздно вспоминает о тональности. В какой команде вы?"},
		{scenarioID: "song_memory", mechanism: "music_memory", text: "Какая строчка из песни выросла вместе с вами и теперь означает совсем не то, что раньше?"},
		{scenarioID: "astana_soundtrack", mechanism: "local_identity", text: "Если собрать плейлист «Астана после работы», какая песня обязана открывать его, а какая — звучать последней?"},
		{scenarioID: "audience_choice", mechanism: "participation", text: "Выберите тему для короткого разбора: удобная тональность, дыхание перед припевом или страх высокой ноты. Что пригодится раньше?"},
		{scenarioID: "adult_beginner", mechanism: "recognition", text: "Взрослый новичок чаще боится не ошибиться, а выглядеть человеком, который ещё не умеет. В какой сфере вам удалось пережить этот первый шаг?"},
		{scenarioID: "everyday_voice_humor", mechanism: "conversation_humor", text: "В машине концерт начинается уверенно, пока музыка не становится тише на светофоре. У кого ещё внезапно меняется громкость солиста?"},
		{scenarioID: "recording_reaction", mechanism: "recognition", text: "Что удивляет в записи своего голоса сильнее: тембр, интонация или то, насколько иначе звучит знакомая фраза?"},
		{scenarioID: "after_work_creativity", mechanism: "lifestyle", text: "Когда вы в последний раз учились чему-то не для работы, диплома или пользы? Что выбрали просто потому, что хотелось?"},
		{scenarioID: "mini_voice_experiment", mechanism: "practical", text: "Попробуйте пропеть один припев сначала очень серьёзно, потом как обычный рассказ другу. В какой версии слова слышны лучше?"},
		{scenarioID: "finish_the_line", mechanism: "participation", text: "Продолжите без долгих раздумий: «Песня, которую нельзя включать фоном, — это…»"},
		{scenarioID: "format_choice", mechanism: "qualification", text: "Что помогает вам быстрее освоиться в новом деле: личное внимание или маленькая группа, где ошибаются вместе?"},
		{scenarioID: "seven_day_challenge", mechanism: "participation", text: "Небольшой музыкальный челлендж: неделю начинать утро с одной песни и записывать одно слово о настроении после неё. Какой трек взяли бы первым?"},
		{scenarioID: "question_to_teacher", mechanism: "participation", text: "Что в пении кажется вам «врождённым» и поэтому бессмысленным для изучения? Можно собрать эти убеждения для честного разбора."},
		{scenarioID: "music_hot_take", mechanism: "conversation", text: "Хороший кавер не обязан быть похож на оригинал. Какой исполнитель заставил вас заново услышать знакомую песню?"},
		{scenarioID: "karaoke_archetype", mechanism: "conversation_humor", text: "Самая честная должность в караоке — человек, который знает только припев, но отвечает за него как за весь концерт. Есть такой в вашей компании?"},
	}
	for index := range posts {
		posts[index].goal = "replies"
	}
	if voice == "alisher" {
		return append(posts[1:], posts[0])
	}
	return posts
}

func groundedFakeThreadPostBank(request normalizedThreadPostRequest, plan []threadPostScenario) []fakeThreadPost {
	evidence := fakeThreadPostEvidence(request.Material)
	prefixes := map[string]string{
		"teacher_micro_tip":      "Практическая деталь о голосе из материала дня: ",
		"myth_micro_test":        "Для короткой проверки берём только подтверждённую деталь: ",
		"first_minute":           "Сцену первого знакомства задаёт конкретная деталь: ",
		"what_wont_happen":       "Спокойнее, когда заранее известна эта точная деталь: ",
		"normal_mistake":         "Вместо идеальной истории — одна нормальная деталь о голосе: ",
		"student_week":           "Творческую неделю лучше всего показывает конкретная деталь: ",
		"backstage_moment":       "Закулисье начинается не с триумфа, а с этой детали: ",
		"community_event":        "Жизнь музыкального сообщества видна в одной детали: ",
		"transparent_invitation": "В приглашении оставляем только проверенную конкретику: ",
	}
	allowed := make(map[string]threadPostScenario, len(plan))
	bank := make([]fakeThreadPost, 0, len(plan)+len(fakeThreadPostBank(request.Voice)))
	for _, scenario := range plan {
		allowed[scenario.ID] = scenario
		if !scenario.RequiresMaterial {
			continue
		}
		prefix := prefixes[scenario.ID]
		if prefix == "" {
			prefix = "Подтверждённая музыкальная деталь из материала дня: "
		}
		bank = append(bank, fakeThreadPost{
			scenarioID: scenario.ID, mechanism: scenario.Mechanism,
			materialBasis: "material", evidence: evidence, text: prefix + evidence,
		})
	}
	seenEvergreen := make(map[string]struct{}, len(plan))
	for _, candidate := range fakeThreadPostBank(request.Voice) {
		scenario, ok := allowed[candidate.scenarioID]
		if !ok || scenario.RequiresMaterial {
			continue
		}
		if _, duplicate := seenEvergreen[candidate.scenarioID]; duplicate {
			continue
		}
		seenEvergreen[candidate.scenarioID] = struct{}{}
		bank = append(bank, candidate)
	}
	return bank
}

func fakeThreadPostEvidence(material string) string {
	material = strings.TrimSpace(material)
	const budget = 120
	for _, line := range strings.Split(material, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len([]rune(line)) <= budget {
			if _, valid := threadPostQuotedSpans(line); valid {
				return line
			}
		}
		runes := []rune(line)
		if len(runes) > budget {
			runes = runes[:budget]
		}
		for index := len(runes) - 1; index >= 0; index-- {
			if !strings.ContainsRune(".!?;", runes[index]) {
				continue
			}
			candidate := strings.TrimSpace(string(runes[:index+1]))
			if _, valid := threadPostQuotedSpans(candidate); valid && candidate != "" {
				return candidate
			}
		}
	}

	// If the first complete line/sentence is longer than the post budget, take
	// an exact word-bounded span between quotation delimiters. Omitting the
	// delimiters keeps the excerpt exact while preventing a blind rune cut from
	// creating an unmatched quote in every deterministic finalist.
	chunks := strings.FieldsFunc(material, func(character rune) bool {
		return character == '\n' || character == '\r' || character == '«' || character == '»' ||
			character == '“' || character == '”' || character == '"'
	})
	for _, chunk := range chunks {
		candidate := boundedFakeThreadPostEvidence(chunk, budget)
		if candidate == "" || !strings.Contains(material, candidate) {
			continue
		}
		if _, valid := threadPostQuotedSpans(candidate); valid {
			return candidate
		}
	}
	return boundedFakeThreadPostEvidence(material, budget)
}

func boundedFakeThreadPostEvidence(value string, budget int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= budget {
		return value
	}
	runes = runes[:budget]
	cut := len(runes)
	for index := len(runes) - 1; index >= 0; index-- {
		if runes[index] == ' ' || runes[index] == '\t' {
			cut = index
			break
		}
	}
	return strings.TrimSpace(string(runes[:cut]))
}

func fakeResult(request domain.GenerationRequest) domain.GenerationResult {
	mode, confidence := fakeScenario(request)
	texts := fakeReplyBank(mode, request.Input.Text)
	offset := fakeVariantOffset(request.Transform, request.VariantSeed)
	for attempt := 0; attempt < 6; attempt++ {
		result := fakeResultAtOffset(request, mode, confidence, texts, offset)
		if !overlapsPreviousReplies(result.Replies, request.PreviousReplies) {
			return result
		}
		offset = (offset + 1) % 6
	}
	return fakeResultAtOffset(request, mode, confidence, texts, offset)
}

func fakeResultAtOffset(request domain.GenerationRequest, mode domain.ScenarioMode, confidence domain.ModeConfidence, texts map[domain.Tone][]string, offset int) domain.GenerationResult {
	result := domain.GenerationResult{
		Mode: mode, ModeConfidence: confidence,
		Situation: "Нужен короткий уверенный ответ без лишней агрессии.",
	}
	if mode == domain.ScenarioComment {
		result.Situation = "Это публичная публикация: нужен самостоятельный остроумный комментарий, а не ответ автору как собеседнику."
	}
	effectiveTone := request.Tone
	if mode == domain.ScenarioComment && effectiveTone != domain.ToneMeme {
		effectiveTone = domain.ToneMix
	}
	if effectiveTone == domain.ToneMix {
		toneOrder := []domain.Tone{domain.ToneSmart, domain.TonePlayful, domain.ToneBoundary}
		for index, tone := range toneOrder {
			bank := fakeToneBank(texts, tone)
			result.Replies = append(result.Replies, domain.Reply{Tone: tone, Text: bank[(offset+index)%len(bank)]})
		}
	} else {
		bank := fakeToneBank(texts, effectiveTone)
		for index := 0; index < 3; index++ {
			result.Replies = append(result.Replies, domain.Reply{Tone: effectiveTone, Text: bank[(offset+index)%len(bank)]})
		}
	}
	if request.Tone == domain.ToneMeme {
		result.Meme = &domain.Meme{
			Headline: "КОГДА ПАНЧ УЖЕ ГОТОВ",
			Caption:  "а контекст всё ещё печатает…",
			Footer:   "Witty Reply",
			Mood:     "dry",
		}
	}
	applyFakeLanguage(&result, request.Language, offset)
	return result
}

func overlapsPreviousReplies(replies []domain.Reply, previous []string) bool {
	if len(replies) == 0 || len(previous) == 0 {
		return false
	}
	for _, reply := range replies {
		for _, old := range previous {
			if strings.EqualFold(strings.TrimSpace(reply.Text), strings.TrimSpace(old)) {
				return true
			}
		}
	}
	return false
}

func fakeToneBank(texts map[domain.Tone][]string, tone domain.Tone) []string {
	if bank := texts[tone]; len(bank) > 0 {
		return bank
	}
	if bank := texts[domain.ToneBoundary]; len(bank) > 0 {
		return bank
	}
	return []string{"Готов безопасный демонстрационный вариант."}
}

func fakeScenario(request domain.GenerationRequest) (domain.ScenarioMode, domain.ModeConfidence) {
	if request.Mode.Concrete() {
		return request.Mode, domain.ModeConfidenceHigh
	}
	switch request.SourceHint {
	case "forwarded_channel":
		return domain.ScenarioComment, domain.ModeConfidenceHigh
	case "forwarded_chat":
		return domain.ScenarioComment, domain.ModeConfidenceLow
	case "forwarded_user":
		return domain.ScenarioReply, domain.ModeConfidenceHigh
	case "screenshot":
		return domain.ScenarioReply, domain.ModeConfidenceLow
	}
	lower := strings.ToLower(strings.TrimSpace(request.Input.Text))
	publicMarkers := []string{"threads", "тредс", "под пост", "under the post", "публикац", "publication", "форум", "forum", "коммент", "comment", "пациентк", "шевелен"}
	for _, marker := range publicMarkers {
		if strings.Contains(lower, marker) {
			return domain.ScenarioComment, domain.ModeConfidenceHigh
		}
	}
	for _, prefix := range []string{"многие ", "учёные ", "врачи ", "новость:", "исследование:"} {
		if strings.HasPrefix(lower, prefix) {
			return domain.ScenarioComment, domain.ModeConfidenceHigh
		}
	}
	return domain.ScenarioReply, domain.ModeConfidenceHigh
}

func fakeReplyBank(mode domain.ScenarioMode, input string) map[domain.Tone][]string {
	if mode == domain.ScenarioComment {
		if lower := strings.ToLower(input); strings.Contains(lower, "шевелен") || strings.Contains(lower, "пациентк") {
			return map[domain.Tone][]string{
				domain.ToneSmart: {
					"Некоторые впервые чувствуют шевеление ребёнка, когда он наконец устраивается на работу и съезжает.",
					"Потом шевеления отлично чувствуются по звуку холодильника в два часа ночи.",
					"Главное — не пропустить первое уверенное движение в сторону самостоятельной оплаты коммуналки.",
					"Сначала не чувствуешь шевелений, потом по тишине в соседней комнате понимаешь вообще всё.",
					"Иногда первое заметное шевеление — это когда ребёнок просит ключи от машины.",
					"Природа потом компенсирует всё одним вопросом: «А где мой второй носок?»",
				},
				domain.TonePlayful: {
					"До восемнадцати лет наблюдение продолжается, потом объект исследования просит денег на такси.",
					"Wi‑Fi выключите — там такие шевеления начнутся, медицина удивится.",
					"Первый толчок трогательный, тысячный — это Lego под босой ногой.",
					"Некоторые дети берегут первое шевеление до фразы «пора искать работу».",
					"Всё индивидуально: мой активируется исключительно на звук открывающегося холодильника.",
					"Зато подозрительное затишье в детской потом определяется без всяких приборов.",
				},
				domain.ToneBoundary: {
					"Сюжетный поворот: самое сильное шевеление случается в сторону родительского кошелька.",
					"К совершеннолетию шевеления становятся редкими, но финансово очень ощутимыми.",
					"Иногда ребёнок не шевелится, потому что уже удобно устроился на вашем диване в двадцать семь.",
					"Финальная стадия наблюдения — движение коробок к отдельной квартире.",
					"Главное шевеление впереди: из семейного чата во взрослую жизнь.",
					"Некоторые особенно активны ровно в момент, когда родители решили отдохнуть.",
				},
				domain.ToneMeme: {
					"Шевеление обнаружено: ребёнок потянулся к родительской карте.",
					"Ожидание: первый толчок. Реальность: «Мам, закинь на такси».",
					"Когда ребёнок наконец шевельнулся — но только к холодильнику.",
					"УЗИ: спокойно. Выключенный Wi‑Fi: мгновенная активность.",
					"Первое движение во взрослую жизнь отменено: на улице холодно.",
					"Шевеления есть. В направлении дивана.",
				},
			}
		}
		return map[domain.Tone][]string{
			domain.ToneSmart: {
				"Пост закончился, а мысль ещё пару секунд ехала по инерции.",
				"Редкий случай: продолжение шутки уже спрятано в самой формулировке.",
				"Комментарии открылись — и сюжет сразу получил второй сезон.",
				"Одна фраза, а вопросов хватило на полноценный подкаст.",
				"Тот момент, когда контекст оказался смешнее любого сценариста.",
				"Формулировка аккуратно оставила дверь открытой для всего интернета.",
			},
			domain.TonePlayful: {
				"Я зашёл на минуту, а тут уже финал сезона без предыдущих серий.",
				"Сюжет уверенно пошёл туда, где даже автор его не ждал.",
				"Комментарий ничего не добавит — пост уже сделал всю работу сам.",
				"Так, а инструкцию к этому повороту сюжета будут выдавать?",
				"Интернет молча снял очки и перечитал ещё раз.",
				"Вот теперь хочется услышать версию режиссёра.",
			},
			domain.ToneBoundary: {
				"Где-то сейчас здравый смысл просит вернуть его в предыдущий абзац.",
				"Это уже не поворот сюжета, это выезд на встречную полосу повествования.",
				"Фраза открыла портал, а закрывать его снова комментариям.",
				"Начали с наблюдения, закончили заявкой на отдельную вселенную.",
				"У этой мысли был план, но по дороге она выбрала приключение.",
				"С таким разгоном финал должен был быть хотя бы с титрами.",
			},
			domain.ToneMeme: {
				"Когда открыл комментарии, а пост уже пошутил за всех.",
				"Ожидание: контекст. Реальность: второй сезон без первого.",
				"Интернет.exe перечитывает формулировку.",
				"Сюжет загружен. Логика устанавливается отдельно.",
				"Когда мысль свернула, но поворотник решила не включать.",
				"Этот пост: режиссёрская версия будет позже.",
			},
		}
	}

	return map[domain.Tone][]string{
		domain.ToneSmart: {
			"Интересная версия. Жаль, факты в ней не участвовали.",
			"Звучит уверенно — осталось добавить основания.",
			"Давай отделим красивую подачу от сути.",
			"Я услышал мнение. Аргумент тоже будет?",
			"Уверенность вижу. Теперь бы ещё источник.",
			"Версия эффектная, но проверку фактами пока не прошла.",
		},
		domain.TonePlayful: {
			"Смело. Даже реальность на секунду растерялась.",
			"Вот это сюжет — сценаристы уже записывают.",
			"Поворот неожиданный, логика просила передать привет.",
			"Принято. Отправляю в музей уверенных догадок.",
			"Сюжет принят, ждём появления причинно-следственной связи.",
			"Так уверенно, что реальность решила перепроверить себя.",
		},
		domain.ToneSharp: {
			"Громкость не превращает это в аргумент.",
			"Подкол замечен. Смысл пока нет.",
			"Попытка задеть засчитана, результат — нет.",
			"Если цель была впечатлить, попробуй точнее.",
			"Эффектный выпад не заменяет точного смысла.",
			"Слова острые, аргумент всё ещё тупится.",
		},
		domain.ToneBoundary: {
			"Со мной так разговаривать не нужно.",
			"Продолжу разговор, когда тон станет уважительным.",
			"Шутка закончилась там, где началось неуважение.",
			"Эту границу я обозначил. Дальше без подколов.",
			"Такой тон мне не подходит. Вернёмся к сути.",
			"Готов обсуждать вопрос, но без личных выпадов.",
		},
		domain.ToneMeme: {
			"Когда хотел подколоть, но аргумент не загрузился.",
			"Ожидание: эффектный выпад. Реальность: неловкая пауза.",
			"Мой ответ уже готов, а логика всё ещё подключается.",
			"Срочно в мемы: такой уверенности нужен архив.",
			"Когда панч пришёл, а основание осталось дома.",
			"Загрузка аргумента: 0%. Уверенность: 100%.",
		},
	}
}

func fakeVariantOffset(transform string, seed uint32) int {
	offset := int(seed % 6)
	switch strings.ToLower(strings.TrimSpace(transform)) {
	case "funnier":
		offset += 1
	case "sharper", "bolder":
		offset += 2
	case "softer", "subtler":
		offset += 3
	case "shorter", "absurder":
		offset += 4
	case "more", "retry", "new_angle", "mode_switch", "mode_selected":
		offset += 5
	}
	return offset % 6
}

func applyFakeLanguage(result *domain.GenerationResult, language string, offset int) {
	switch fakeLanguageCode(language) {
	case "en":
		result.Situation = "Three concise candidates are ready for the selected social scenario."
		bank := []string{
			"Bold claim. The supporting evidence must still be typing.",
			"That plot twist arrived before the plot.",
			"I heard the confidence; let me know when the argument joins.",
			"A strong opening for a story the facts declined to attend.",
			"Interesting theory. Reality has requested a source.",
			"Let's separate the dramatic delivery from the actual point.",
			"The volume landed. The reasoning is still in transit.",
			"That was impressively certain for a first draft.",
			"I'll continue when the conversation gets respectful.",
		}
		if result.Mode == domain.ScenarioComment {
			bank = []string{
				"One sentence, and the comments already have a sequel ready.",
				"The post ended, but the questions just clocked in.",
				"That thought took the scenic route without warning anyone.",
				"I opened the comments and the post had already done the warm-up act.",
				"Somewhere, the missing context is enjoying a quiet day off.",
				"This plot twist skipped straight to the bonus episode.",
				"The wording left the door open and the whole internet walked in.",
				"A full podcast of questions packed into one sentence.",
				"The director's cut is going to need subtitles.",
			}
		}
		for index := range result.Replies {
			result.Replies[index].Text = bank[(offset+index)%len(bank)]
		}
		if result.Meme != nil {
			result.Meme.Headline = "WHEN THE COMEBACK ARRIVES"
			result.Meme.Caption = "but the argument is still typing…"
		}
	case "kk":
		result.Situation = "Таңдалған сценарийге үш қысқа нұсқа дайын."
		bank := []string{
			"Нық айттың. Енді дәлелін де көрсетесің бе?",
			"Сенімді естіледі — негізі қайда?",
			"Әдемі сөзді нақты ойдан ажыратайық.",
			"Батыл екен. Шындықтың өзі сәл абдырап қалды.",
			"Сюжет қызық, логика жарнамадан кейін оралады.",
			"Қабылданды. Сенімді болжамдар мұрағатына жібердім.",
			"Қатты айту оны дәлелге айналдырмайды.",
			"Қалжың байқалды. Мағынасы әлі жоқ.",
			"Тон құрметті болғанда әңгімені жалғастырамын.",
		}
		if result.Mode == domain.ScenarioComment {
			bank = []string{
				"Бір сөйлем — ал пікірлердің жалғасы дайын тұр.",
				"Пост бітті, сұрақтар енді жұмысқа кірісті.",
				"Бұл ой бұрылысты ескертусіз жасаған екен.",
				"Пікірлерді ашсам, пост әзілді өзі бастап қойыпты.",
				"Жоғалған контекст бүгін демалыста сияқты.",
				"Сюжет бірден қосымша бөлімге өтіп кетті.",
				"Сөйлем есікті ашық қалдырды, интернет түгел кіріп келді.",
				"Бір сөйлемнің ішінде тұтас подкастқа жететін сұрақ бар.",
				"Режиссерлік нұсқаға субтитр қажет болатын сияқты.",
			}
		}
		for index := range result.Replies {
			result.Replies[index].Text = bank[(offset+index)%len(bank)]
		}
		if result.Meme != nil {
			result.Meme.Headline = "ТҮЙРЕУ ЖІБЕРІЛІП ҚОЙҒАНДА"
			result.Meme.Caption = "ал дәлел әлі теріліп жатыр…"
		}
	}
}

func fakeLanguageCode(language string) string {
	lower := strings.ToLower(strings.TrimSpace(language))
	lower = strings.TrimPrefix(lower, "auto-")
	switch {
	case strings.HasPrefix(lower, "en"):
		return "en"
	case strings.HasPrefix(lower, "kk"), strings.HasPrefix(lower, "kz"):
		return "kk"
	default:
		return "ru"
	}
}
