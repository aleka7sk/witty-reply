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
	results := curatedThreadPosts(normalized, threadPostFinalistCount, nil)
	if len(results) != threadPostFinalistCount {
		return ThreadPostResult{}, fmt.Errorf("fake Threads finalist invariant: %w", ErrInvalidResponse)
	}
	audit := ThreadPostAudit{
		GenerationID: normalized.GenerationID, RecipeID: selectThreadPostRecipe(normalized).ID,
		ExplorationGoal: threadPostFinalistCount, GenerationCalls: 1, GeneratorProvider: providerFake,
		GeneratorModel: "deterministic-threads-v2", SelectionMode: "deterministic_fake",
		DecisionReason: "Deterministic local preview provider selected the strongest of five validated editorial examples.",
		Candidates:     make([]ThreadPostCandidateAudit, 0, threadPostFinalistCount),
	}
	candidates := make([]threadPostCandidate, 0, threadPostFinalistCount)
	for index, finalist := range results {
		audit.Candidates = append(audit.Candidates, ThreadPostCandidateAudit{
			Attempt: 1, SourceSlot: fmt.Sprintf("fake_%d", index+1), Goal: finalist.Goal,
			Text: finalist.Text, Eligible: true, Considered: true, Local: scoreThreadPostQuality(finalist.Text),
		})
		candidates = append(candidates, threadPostCandidate{Result: finalist, AuditIndex: index})
	}
	assignBlindReviewerIDs(candidates, audit.Candidates, normalized.Seed, 1)
	winner := bestLocalThreadPostCandidate(candidates, audit.Candidates)
	markThreadPostAuditWinner(&audit, winner)
	result := winner.Result
	result.Provider = providerFake
	result.Model = audit.GeneratorModel
	result.Visual = ThreadPostVisualRecommendation{Mode: "text_only", Query: defaultThreadPhotoQuery}
	result.Audit = audit
	return result, nil
}

func curatedThreadPosts(normalized normalizedThreadPostRequest, limit int, excluded map[string]struct{}) []ThreadPostResult {
	if limit <= 0 {
		return nil
	}
	bank := fakeThreadPostBank(normalized.Voice)
	offset := int(normalized.Seed % uint32(len(bank)))
	switch normalized.Transform {
	case "wittier":
		offset = (offset + 2) % len(bank)
	case "warmer":
		offset = (offset + 4) % len(bank)
	case "shorter":
		offset = (offset + 6) % len(bank)
	case "different_angle":
		offset = (offset + 8) % len(bank)
	}
	results := make([]ThreadPostResult, 0, limit)
	seen := make(map[string]struct{}, len(excluded)+limit)
	for key := range excluded {
		seen[key] = struct{}{}
	}
	for attempt := 0; attempt < len(bank) && len(results) < limit; attempt++ {
		candidate := bank[(offset+attempt)%len(bank)]
		result := ThreadPostResult{Goal: candidate.goal, Text: candidate.text}
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
		results = append(results, result)
	}
	return results
}

type fakeThreadPost struct {
	goal string
	text string
}

func fakeThreadPostBank(voice string) []fakeThreadPost {
	if voice == "alisher" {
		return []fakeThreadPost{
			{goal: "recognition", text: "Взрослая жизнь устроена странно: на созвоны голос находится всегда, на любимую песню — после внутреннего согласования."},
			{goal: "discussion", text: "Мы так долго учимся говорить уверенно, а потом стесняемся спеть одну ноту."},
			{goal: "recognition", text: "У взрослого человека есть отдельный талант: хотеть петь и одновременно ждать письменного разрешения от вселенной."},
			{goal: "discussion", text: "Караоке быстро показывает, кто выбрал песню сердцем, а кто переоценил переговорные навыки."},
			{goal: "recognition", text: "Самая сложная нота — та, перед которой успел придумать мнение всех соседей."},
			{goal: "warmth", text: "Иногда «я не умею петь» означает «я ещё не слышал себя без внутреннего отдела критики»."},
			{goal: "discussion", text: "Микрофон ничего не добавляет к характеру. Он просто перестаёт его скрывать."},
			{goal: "recognition", text: "Перед первой нотой внутренний критик обычно просит слово вне очереди."},
			{goal: "discussion", text: "Есть песни, которые человек выбирает сам. И есть песни, которые внезапно знают о нём больше."},
			{goal: "warmth", text: "Уверенность редко приходит до голоса. Обычно она догоняет его где-то между вдохом и первой фразой."},
			{goal: "recognition", text: "Фраза «я пою только для себя» обычно произносится так, будто у себя очень строгий продюсер."},
			{goal: "recognition", text: "Внутренний критик удивительно музыкален: вступает без приглашения и всегда уверен, что он солист."},
			{goal: "discussion", text: "Люди боятся взять не ту ноту, будто правильные ноты потом подают на них в суд."},
			{goal: "recognition", text: "У каждого есть песня, на которой уверенность внезапно заканчивает испытательный срок."},
			{goal: "discussion", text: "Когда говорят «медведь на ухо наступил», медведя почему-то никто не просит подтвердить версию."},
			{goal: "recognition", text: "Микрофон не пугает. Пугает внезапная перспектива услышать себя без внутреннего пресс-секретаря."},
			{goal: "recognition", text: "Взрослый человек может провести сложные переговоры, но перед микрофоном всё равно ждёт согласования у подростка внутри."},
			{goal: "recognition", text: "Песня занимает несколько минут. Подготовительный стыд иногда выходит режиссёрской версией."},
			{goal: "recognition", text: "Нота может быть мимо. Лицо после неё обычно делает ошибку заметнее."},
			{goal: "discussion", text: "Самый верный способ не сфальшивить — не петь. У него почему-то очень скучный репертуар."},
			{goal: "recognition", text: "Высокую ноту проще взять, чем спокойно принять запись собственного голоса."},
			{goal: "recognition", text: "Человек слышит запись своего голоса и сразу понимает: внутренний диктор всё это время работал удалённо."},
			{goal: "warmth", text: "Если голос дрожит, возможно, он просто первым понял важность момента."},
			{goal: "discussion", text: "Караоке — место, где друзья искренне поддерживают тебя и совершенно не поддерживают выбранную тональность."},
			{goal: "discussion", text: "Какой знакомый припев превращает ваше «я только послушаю» в полноценное выступление?"},
			{goal: "recognition", text: "Микрофон не делает человека громче. Он просто увольняет внутреннего пресс-секретаря."},
			{goal: "discussion", text: "Какую песню вы знаете наизусть, хотя никогда не садились учить её слова?"},
			{goal: "recognition", text: "Фальшивую ноту слышат не все. Попытку сделать вид, что так и задумано, замечают почему-то сразу."},
			{goal: "discussion", text: "Какой исполнитель заставляет вас подпевать даже в магазине, где приходится делать вид, что это кашель?"},
			{goal: "recognition", text: "Наушники создают редкое государство: один гражданин, полный суверенитет и очень спорный вокал."},
			{goal: "discussion", text: "Какая строчка из песни выросла вместе с вами и теперь означает совсем не то, что раньше?"},
			{goal: "recognition", text: "Запись собственного голоса — короткая встреча внутреннего диктора с человеком, на которого он всё это время работал."},
			{goal: "discussion", text: "Какой припев вы бы доверили человеку вместо длинного объяснения своего настроения?"},
			{goal: "recognition", text: "Ритм сбивается реже, чем уверенность. Просто у ритма нет привычки читать воображаемые комментарии."},
			{goal: "discussion", text: "Какую песню нельзя ставить фоном, потому что она немедленно забирает всё внимание?"},
			{goal: "warmth", text: "Любимый голос не обязательно самый ровный. Обычно это тот, в котором слышно живого человека между нотами."},
			{goal: "recognition", text: "Слово «подпевать» звучит скромно. Соседи иногда располагают другой терминологией."},
			{goal: "discussion", text: "Какую мелодию вы узнаете раньше, чем успеваете вспомнить, откуда она?"},
			{goal: "recognition", text: "Человек может забыть слова куплета, но тело почему-то прекрасно помнит, где должен начаться припев."},
			{goal: "warmth", text: "Некоторые песни возвращают не прошлое, а способность на минуту отнестись к нему мягче."},
			{goal: "discussion", text: "Какой трек вы включаете ради одной-единственной секунды, где всё встаёт на место?"},
			{goal: "recognition", text: "Домашний вокал особенно смел, пока чайник, душ и пылесос официально входят в состав группы."},
			{goal: "warmth", text: "Тихий голос тоже умеет держать внимание. Ему просто приходится выбирать слова и ноты точнее."},
			{goal: "discussion", text: "Если бы ваш характер был музыкальным инструментом, что звучало бы первым: барабаны, клавиши или что-то другое?"},
		}
	}
	return []fakeThreadPost{
		{goal: "warmth", text: "Иногда голосу нужен не новый диапазон, а разрешение звучать без извинений."},
		{goal: "recognition", text: "Пение — редкий способ занять пространство и никого при этом не вытеснить."},
		{goal: "warmth", text: "Есть дни, когда лучший разговор с собой начинается не со слов, а с ноты."},
		{goal: "recognition", text: "Свой голос узнаётся не тогда, когда он идеален, а когда перестаёшь прятать его за чужими."},
		{goal: "warmth", text: "Вокал начинается не с громкости. Он начинается с момента, когда перестаёшь уменьшать себя."},
		{goal: "discussion", text: "Астана умеет быть громкой. Иногда особенно приятно ответить ей своей нотой."},
		{goal: "warmth", text: "Музыка не требует быть готовым. Она просит быть настоящим."},
		{goal: "recognition", text: "Голос — это единственный инструмент, который невозможно забыть дома."},
		{goal: "discussion", text: "Какая песня первой вспоминается, когда никому ничего не нужно доказывать?"},
		{goal: "warmth", text: "Иногда одна честная нота возвращает к себе быстрее, чем длинный внутренний разговор."},
		{goal: "recognition", text: "Когда человек говорит «у меня нет голоса», голос уже произнёс эту фразу довольно убедительно."},
		{goal: "warmth", text: "Тишина перед первой нотой — не пустота. Это смелость набирает воздух."},
		{goal: "recognition", text: "Фальшивая нота заканчивается быстро. Страх перед ней иногда репетирует дольше самой песни."},
		{goal: "warmth", text: "Есть песни, которые не хочется исполнять идеально. Их хочется прожить точно."},
		{goal: "recognition", text: "Красивый голос впечатляет. Узнаваемый — остаётся."},
		{goal: "warmth", text: "Пение — редкий разговор, где дыхание успевает сказать правду раньше слов."},
		{goal: "recognition", text: "Не каждая нота обязана быть громкой. Некоторые попадают точно потому, что не спорят с тишиной."},
		{goal: "recognition", text: "Самая узнаваемая часть песни начинается там, где человек перестаёт стараться звучать «правильно»."},
		{goal: "warmth", text: "Песня меняется, когда перестаёшь изображать исполнителя и становишься рассказчиком."},
		{goal: "recognition", text: "Диапазон измеряют нотами. Свободу голоса — тем, сколько себя в них осталось."},
		{goal: "warmth", text: "Микрофон усиливает звук, но не подменяет присутствие. И это хорошая новость."},
		{goal: "recognition", text: "Иногда дыхание сбивается не от сложной фразы, а от мысли, что тебя действительно услышат."},
		{goal: "warmth", text: "Музыкальный слух замечает ноту. Человеческий — честность."},
		{goal: "discussion", text: "Какую песню вы бы спели, если бы никто не оценивал исполнение?"},
		{goal: "discussion", text: "Какую песню вы узнаете по одному вдоху ещё до первой ноты?"},
		{goal: "recognition", text: "Голос — единственный инструмент, который невозможно забыть дома. Зато можно долго делать вид, что он там остался."},
		{goal: "warmth", text: "Тишина перед первой нотой не пустая. В ней дыхание, внимание и маленькое решение всё-таки начать."},
		{goal: "discussion", text: "Какая строчка из песни говорит о вашем настроении точнее любого статуса?"},
		{goal: "discussion", text: "Какую мелодию вы узнаете раньше, чем успеваете вспомнить её название?"},
		{goal: "recognition", text: "Красивый голос привлекает внимание. Узнаваемый остаётся в памяти после последней ноты."},
		{goal: "discussion", text: "Какой музыкальный звук для вас уютнее: шорох пластинки, клавиши, гитара или чей-то тихий голос?"},
		{goal: "warmth", text: "Микрофон усиливает звук, но не подменяет присутствие. Поэтому тихая фраза иногда держит внимание лучше громкой."},
		{goal: "discussion", text: "Какую песню вы бы оставили себе, если бы из всего плейлиста можно было сохранить только одну?"},
		{goal: "warmth", text: "Любимая песня не всегда утешает. Иногда она просто садится рядом и не торопит менять настроение."},
		{goal: "discussion", text: "Какой припев объединяет людей, которые до него были уверены, что у них совершенно разные вкусы?"},
		{goal: "recognition", text: "Первые слова песни иногда забываются. Тело всё равно точно знает, где начинается знакомый ритм."},
		{goal: "discussion", text: "Какую песню невозможно включить фоном, потому что она сразу требует всего внимания?"},
		{goal: "recognition", text: "Одна и та же мелодия в наушниках, машине и пустой комнате звучит как три разных разговора."},
		{goal: "discussion", text: "Какой голос вы узнали бы даже через старый телефон и шум улицы?"},
		{goal: "warmth", text: "Песня не меняет прошлое. Но иногда меняет интонацию, с которой человек его вспоминает."},
		{goal: "discussion", text: "Какой трек вы включаете ради одной секунды, в которой всё неожиданно становится на место?"},
		{goal: "recognition", text: "На записи собственный голос кажется чужим ровно до момента, когда в нём узнаётся знакомая улыбка."},
		{goal: "discussion", text: "Какую песню вы любите не целиком, а за одну строчку, один аккорд или один вдох?"},
		{goal: "discussion", text: "Если бы Астана звучала музыкальным инструментом, что это было бы и почему?"},
	}
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
