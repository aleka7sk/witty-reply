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
