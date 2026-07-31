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
	if err := ValidateResult(&result, normalized.Tone); err != nil {
		return domain.GenerationResult{}, fmt.Errorf("fake provider invariant: %w", err)
	}
	result.Provider = providerFake
	result.Model = "deterministic-v1"
	result.Usage = domain.Usage{}
	return result, nil
}

func fakeResult(request domain.GenerationRequest) domain.GenerationResult {
	texts := map[domain.Tone][]string{
		domain.ToneSmart: {
			"Интересная версия. Жаль, факты в ней не участвовали.",
			"Звучит уверенно — осталось добавить основания.",
			"Давай отделим красивую подачу от сути.",
			"Я услышал мнение. Аргумент тоже будет?",
		},
		domain.TonePlayful: {
			"Смело. Даже реальность на секунду растерялась.",
			"Вот это сюжет — сценаристы уже записывают.",
			"Поворот неожиданный, логика просила передать привет.",
			"Принято. Отправляю в музей уверенных догадок.",
		},
		domain.ToneSharp: {
			"Громкость не превращает это в аргумент.",
			"Подкол замечен. Смысл пока нет.",
			"Попытка задеть засчитана, результат — нет.",
			"Если цель была впечатлить, попробуй точнее.",
		},
		domain.ToneBoundary: {
			"Со мной так разговаривать не нужно.",
			"Продолжу разговор, когда тон станет уважительным.",
			"Шутка закончилась там, где началось неуважение.",
			"Эту границу я обозначил. Дальше без подколов.",
		},
		domain.ToneMeme: {
			"Когда хотел подколоть, но аргумент не загрузился.",
			"Ожидание: эффектный выпад. Реальность: неловкая пауза.",
			"Мой ответ уже готов, а логика всё ещё подключается.",
			"Срочно в мемы: такой уверенности нужен архив.",
		},
	}

	result := domain.GenerationResult{Situation: "Нужен короткий уверенный ответ без лишней агрессии."}
	if request.Tone == domain.ToneMix {
		toneOrder := []domain.Tone{domain.ToneSmart, domain.TonePlayful, domain.ToneBoundary}
		for index, tone := range toneOrder {
			result.Replies = append(result.Replies, domain.Reply{Tone: tone, Text: texts[tone][index]})
		}
	} else {
		for _, text := range texts[request.Tone][:3] {
			result.Replies = append(result.Replies, domain.Reply{Tone: request.Tone, Text: text})
		}
	}
	if request.Tone == domain.ToneMeme {
		result.Meme = &domain.Meme{
			Headline: "КОГДА ПОДКОЛ УЖЕ ОТПРАВЛЕН",
			Caption:  "а аргумент всё ещё печатает…",
			Footer:   "Witty Reply",
			Mood:     "dry",
		}
	}
	if strings.HasPrefix(strings.ToLower(request.Language), "en") {
		result.Situation = "A short, confident answer is needed without needless aggression."
		for index := range result.Replies {
			result.Replies[index].Text = fmt.Sprintf("Demo reply %d in the requested tone.", index+1)
		}
		if result.Meme != nil {
			result.Meme.Headline = "WHEN THE COMEBACK ARRIVES"
			result.Meme.Caption = "but the argument is still typing…"
		}
	} else if strings.HasPrefix(strings.ToLower(request.Language), "kk") {
		kazakh := map[domain.Tone][]string{
			domain.ToneSmart:    {"Нық айттың. Енді дәлелін де көрсетесің бе?", "Сенімді естіледі — негізі қайда?", "Әдемі сөзді нақты ойдан ажыратайық."},
			domain.TonePlayful:  {"Батыл екен. Шындықтың өзі сәл абдырап қалды.", "Сюжет қызық, логика жарнамадан кейін оралады.", "Қабылданды. Сенімді болжамдар мұрағатына жібердім."},
			domain.ToneSharp:    {"Қатты айту оны дәлелге айналдырмайды.", "Қалжың байқалды. Мағынасы әлі жоқ.", "Тиюге тырыстың, бірақ дәл тимеді."},
			domain.ToneBoundary: {"Менімен бұлай сөйлесудің қажеті жоқ.", "Тон құрметті болғанда әңгімені жалғастырамын.", "Қалжың құрметсіздік басталған жерде бітті."},
			domain.ToneMeme:     {"Түйреп өтпек болды, бірақ дәлелі жүктелмеді.", "Күту: әсерлі жауап. Шындық: ыңғайсыз үнсіздік.", "Менің жауабым дайын, ал логика әлі қосылып жатыр."},
		}
		result.Situation = "Артық агрессиясыз қысқа әрі нық жауап керек."
		for index := range result.Replies {
			result.Replies[index].Text = kazakh[result.Replies[index].Tone][index]
		}
		if result.Meme != nil {
			result.Meme.Headline = "ТҮЙРЕУ ЖІБЕРІЛІП ҚОЙҒАНДА"
			result.Meme.Caption = "ал дәлел әлі теріліп жатыр…"
		}
	}
	return result
}
