package bot

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/photos"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
)

func belcantoOwnerOnlyText(lang language) string {
	switch lang {
	case langKK:
		return "Бұл бөлім тек Belcanto командасына қолжетімді."
	case langEN:
		return "This section is available only to the Belcanto team."
	default:
		return "Этот раздел доступен только команде Belcanto."
	}
}

func belcantoGeneratorUnavailableText(lang language) string {
	if lang == langEN {
		return "Belcanto post generation is unavailable with the current AI provider."
	}
	return "Генератор постов Belcanto недоступен у текущего AI-провайдера."
}

func belcantoGenerationErrorText(lang language) string {
	if lang == langEN {
		return "I couldn't prepare a reliable post. Nothing was published. Use the retry button below, or send /threads to start over."
	}
	return "Не получилось подготовить надёжный пост. Ничего не опубликовано. Нажми «Повторить генерацию» ниже или отправь /threads, чтобы начать заново."
}

func belcantoRefinementErrorText(lang language) string {
	if lang == langEN {
		return "I couldn't prepare a reliable revision. Nothing was published and the current draft is unchanged. Choose the edit again below, or send /threads to start over."
	}
	return "Не получилось подготовить надёжную правку. Ничего не опубликовано, текущий черновик сохранён без изменений. Выбери нужную правку ещё раз ниже или отправь /threads, чтобы начать заново."
}

func threadBriefObjectivePromptText() string {
	return "🎼 Новый пост Belcanto\n\nСначала выбери задачу публикации. От неё зависят сценарии, структура и критерии независимого редактора — пост на ответы не будет оцениваться так же, как пост на доверие или запись."
}

func threadBriefMaterialPromptText(objective domain.ThreadObjective) string {
	if objective == domain.ThreadObjectiveTrial {
		return fmt.Sprintf(
			"Цель: %s\n\nДля поста на пробное занятие обязательно пришли одним текстовым сообщением точные подтверждённые условия: формат, цену, дату или расписание, количество мест и способ записи — только то, что действительно актуально. Не добавляй личные данные; имя или дословную цитату человека можно использовать только с его разрешения, а данные детей лучше полностью обезличить.\n\nБез этого материала генератор не будет сочинять предложение. Если условий пока нет, смени цель.",
			threadObjectiveLabel(objective),
		)
	}
	return fmt.Sprintf(
		"Цель: %s\n\nПришли одним текстовым сообщением материал дня: реальную фразу, наблюдение, событие, вопрос аудитории, решение педагога или точные условия предложения. Используем только подтверждённые факты из этого сообщения и ничего не придумаем. Не добавляй личные данные; имя или дословную цитату человека можно использовать только с его разрешения, а данные детей лучше полностью обезличить.\n\nНе вставляй инструкции для AI — просто опиши, что действительно было или что Belcanto действительно предлагает. Если материала сегодня нет, выбери «Без материала дня»: тогда генератор возьмёт только безопасный evergreen-сценарий.",
		threadObjectiveLabel(objective),
	)
}

func threadBriefTrialMaterialRequiredText() string {
	return "Для цели «пробное занятие» нужен реальный материал с подтверждёнными условиями. Генерировать предложение из догадок не буду: пришли материал или смени цель."
}

func threadBriefMaterialInvalidText() string {
	return "Нужен один текстовый материал дня до 6000 символов. Фото можно будет прикрепить к уже готовому посту отдельным безопасным шагом."
}

func threadBriefCancelledText() string {
	return "Подготовка поста отменена. Ничего не сгенерировано и не опубликовано."
}

func threadFinalistSetText(set domain.ThreadFinalistSet) string {
	var message strings.Builder
	hasUnselectable := false
	message.WriteString("🎼 Belcanto Threads — пять вариантов\n\n")
	message.WriteString("Цель: ")
	message.WriteString(threadObjectiveLabel(set.Objective))
	message.WriteString("\n\nНезависимый редактор отметил свой выбор звездой. Выбери текст, который станет точным черновиком для публикации; до выбора ничего не опубликовано.\n")
	for _, candidate := range set.Candidates {
		message.WriteString("\n")
		message.WriteString(fmt.Sprintf("%d. ", candidate.Position+1))
		if candidate.Recommended {
			message.WriteString("⭐ выбор редактора · ")
		}
		message.WriteString(threadScenarioLabel(candidate.ScenarioID))
		if candidate.MaterialBasis == "material" {
			message.WriteString(" · по материалу дня")
		} else {
			message.WriteString(" · evergreen, материал не используется")
		}
		if !candidate.Selectable {
			hasUnselectable = true
			message.WriteString(" · ⚠️ только для сравнения")
		}
		message.WriteString("\n")
		message.WriteString(candidate.Text)
		message.WriteString("\n")
	}
	if hasUnselectable {
		message.WriteString("\nВариант без кнопки не прошёл независимый факт-чек и не может стать публикуемым черновиком.")
	}
	return message.String()
}

func threadFinalistSetCancelledText(restored bool) string {
	if restored {
		return "Новые варианты отменены. Возвращаю предыдущий черновик — он не изменён."
	}
	return "Подготовка поста отменена. Ничего не опубликовано."
}

func threadDraftText(draft domain.ThreadDraft) string {
	return threadDraftTextWithContext(draft, nil, "", "")
}

func threadDraftTextWithBrief(
	draft domain.ThreadDraft,
	materialKind domain.ThreadMaterialKind,
	materialBasis string,
) string {
	return threadDraftTextWithContext(draft, nil, materialKind, materialBasis)
}

func threadDraftTextWithMediaAndBrief(
	draft domain.ThreadDraft,
	mediaValue domain.ThreadMedia,
	materialKind domain.ThreadMaterialKind,
	materialBasis string,
) string {
	return threadDraftTextWithContext(draft, &mediaValue, materialKind, materialBasis)
}

func threadDraftTextWithContext(
	draft domain.ThreadDraft,
	mediaValue *domain.ThreadMedia,
	materialKind domain.ThreadMaterialKind,
	materialBasis string,
) string {
	voice := "Belcanto"
	if draft.Voice == domain.ThreadVoiceAlisher {
		voice = "Алишер"
	}
	objective := threadObjectiveLabel(draft.Objective)
	scenario := threadScenarioLabel(draft.ScenarioID)
	material := "без материала — только evergreen"
	contentRights := ""
	if materialBasis == "material" || (materialBasis == "" && materialKind == domain.ThreadMaterialText) {
		material = "использован подтверждённый материал дня"
		contentRights = "\n\nПеред публикацией проверь факты и разрешения на имена/цитаты; детей обезличь."
	} else if draft.Objective == domain.ThreadObjectiveLegacy {
		material = "старый формат без brief"
	}
	status := "Пост готов — его можно публиковать без правок."
	format := "📝 только текст"
	rights := ""
	if draft.MediaMode == domain.ThreadMediaImagePending {
		status = "Текст готов. Для этого замысла редактор рекомендует реальное фото Belcanto."
		format = "🖼 требуется реальное фото"
		rights = "\n\nПришли один проверенный реальный кадр школы, пространства или музыкальной детали — либо выбери «Оставить только текст»."
	} else if draft.MediaMode == domain.ThreadMediaImage {
		format = "🖼 фото + текст"
		rights = "\n\nНажимая «Права есть — опубликовать», ты подтверждаешь право Belcanto использовать фото и согласие всех узнаваемых людей; для детей — согласие законного представителя."
		if mediaValue != nil && mediaValue.EffectiveSourceKind() == domain.ThreadMediaSourcePexels {
			format = "🖼 лицензированное фото + текст"
			rights = "\n\nФото: " + mediaValue.SourceAuthor + " · Pexels" +
				"\n\nПубликуя, ты подтверждаешь нейтральный контекст без впечатления поддержки Belcanto людьми или брендами на фото."
		}
	}
	return fmt.Sprintf(
		"🎼 Belcanto Threads\n\n%s\n\nАвтор: %s\nЦель: %s\nСценарий: %s\nМатериал: %s\nОтбор: победитель из пяти разных сценариев\nФормат: %s\n\n%s\n\n%d/500%s",
		status, voice, objective, scenario, material, format, draft.Text, utf8.RuneCountInString(draft.Text), rights+contentRights,
	)
}

func threadImagePromptText(hasPrevious bool) string {
	lead := "Пришли одно своё реальное фото Belcanto."
	if hasPrevious {
		lead = "Пришли новое реальное фото Belcanto. Прежнее останется сохранено, пока замена не будет принята."
	}
	return "🖼 " + lead + "\n\nПодойдёт живой кадр школы, занятия, пространства или музыкальная деталь. Он заменит предложенную лицензированную иллюстрацию, если она была. Загружай только проверенный реальный кадр, а не AI-изображение. Подпись к фото не нужна — готовый текст уже сохранён.\n\nЕсли в кадре есть люди, убедись, что они согласны на публичную публикацию. Отправь фото не альбомом."
}

func threadImageInvalidText() string {
	return "Не получилось подготовить это фото для Threads. Нужен JPEG или PNG до 8 МБ, шириной от 320 до 1440 px и с соотношением сторон не более 10:1. Черновик сохранён — пришли другое фото."
}

func threadImageAlbumText() string {
	return "Для этой версии нужен один кадр, не альбом. Отправь одно фото отдельным сообщением."
}

func threadLicensedPhotoDisabledText() string {
	return "Pexels сейчас не подключён. Текстовый черновик и прежнее фото не изменены. Проверь THREADS_PHOTO_PROVIDER и PEXELS_API_KEY, затем перезапусти приложение."
}

func threadLicensedPhotoUnavailableText(hasPrevious bool) string {
	if hasPrevious {
		return "Не удалось найти другое подходящее фото в Pexels. Прежнее фото и текст сохранены без изменений — можно попробовать ещё раз."
	}
	return "Не удалось найти подходящее фото в Pexels. Текстовый черновик сохранён без изменений — можно попробовать ещё раз или загрузить своё фото."
}

func threadLicensedPhotoErrorText(err error, hasPrevious bool) string {
	switch {
	case errors.Is(err, photos.ErrAuthentication):
		return "Pexels не принял API-ключ. Черновик не изменён. Проверь PEXELS_API_KEY и перезапусти приложение."
	case errors.Is(err, photos.ErrRateLimited):
		return "Лимит Pexels временно исчерпан. Черновик не изменён — попробуй ещё раз позже или загрузи своё фото."
	case errors.Is(err, photos.ErrUnavailable):
		return "Pexels временно недоступен. Черновик не изменён — попробуй ещё раз позже или загрузи своё фото."
	default:
		return threadLicensedPhotoUnavailableText(hasPrevious)
	}
}

func threadImageRequiredText() string {
	return "Сначала пришли фото для этой публикации или выбери «Оставить только текст»."
}

func threadImageDeliveryUnavailableText() string {
	return "Фото и текст сохранены, но публичная доставка изображения в Threads не настроена. Публикации не было. Укажи THREADS_MEDIA_BASE_URL с публичным HTTPS-адресом приложения."
}

func threadObjectiveLabel(objective domain.ThreadObjective) string {
	switch objective {
	case domain.ThreadObjectiveReach:
		return "охват"
	case domain.ThreadObjectiveTrust:
		return "доверие"
	case domain.ThreadObjectiveTrial:
		return "пробное занятие"
	case domain.ThreadObjectiveCommunity:
		return "сообщество"
	case domain.ThreadObjectiveReplies:
		return "содержательные ответы"
	default:
		return "редакционная"
	}
}

var threadScenarioLabels = map[string]string{
	"karaoke_archetype":      "караоке-архетип",
	"song_memory":            "песенная память",
	"astana_soundtrack":      "музыкальная Астана",
	"audience_choice":        "выбор аудитории",
	"adult_beginner":         "взрослый новичок",
	"everyday_voice_humor":   "голос в обычной жизни",
	"recording_reaction":     "реакция на голос в записи",
	"after_work_creativity":  "творчество после работы",
	"music_hot_take":         "музыкальная позиция",
	"mini_voice_experiment":  "мини-опыт с голосом",
	"finish_the_line":        "продолжите фразу",
	"format_choice":          "выбор формата",
	"seven_day_challenge":    "семидневная музыкальная практика",
	"question_to_teacher":    "вопрос педагогу",
	"teacher_micro_tip":      "одна подсказка педагога",
	"myth_micro_test":        "миф и мини-проверка",
	"first_minute":           "первая встреча поминутно",
	"what_wont_happen":       "чего не будет",
	"normal_mistake":         "нормальная ошибка",
	"student_week":           "неделя ученика",
	"backstage_moment":       "закулисный момент",
	"community_event":        "событие сообщества",
	"transparent_invitation": "прозрачное приглашение",
	"legacy_unspecified":     "старый редакционный формат",
}

func threadScenarioLabel(scenarioID string) string {
	if label := threadScenarioLabels[strings.TrimSpace(scenarioID)]; label != "" {
		return label
	}
	return "редакционный сценарий"
}

func knownThreadScenario(scenarioID string) bool {
	_, ok := threadScenarioLabels[strings.TrimSpace(scenarioID)]
	return ok
}

func threadsNotConnectedText() string {
	return "🔌 Пост готов, но Threads пока не подключён. Публикации не было. Добавь THREADS_USER_ID и THREADS_ACCESS_TOKEN, включи THREADS_PROVIDER=meta и перезапусти приложение."
}

func threadDraftStaleText() string {
	return "Эта карточка уже устарела. Используй кнопки под последним предпросмотром."
}

func threadDraftStateText(draft domain.ThreadDraft) string {
	if draft.MediaMode == domain.ThreadMediaImagePending {
		return threadImageRequiredText()
	}
	switch draft.State {
	case domain.ThreadDraftPublishing:
		return "Публикация уже выполняется."
	case domain.ThreadDraftPublished:
		return "Этот пост уже опубликован."
	case domain.ThreadDraftUnknown:
		return threadPublishUnknownText()
	case domain.ThreadDraftCancelled:
		return "Этот черновик отменён."
	default:
		return threadDraftStaleText()
	}
}

func threadPublishFailedText() string {
	return "Не удалось опубликовать. Пост сохранён, в Threads ничего не подтверждено. Можно нажать «Опубликовать» ещё раз."
}

func threadPublishUnknownText() string {
	return "⚠️ Threads не подтвердил результат. Автоматически повторять не буду, чтобы не создать дубль. Проверь аккаунт вручную."
}

func threadPublishedText(publication threadspub.Publication) string {
	if strings.TrimSpace(publication.Permalink) != "" {
		return "✅ Опубликовано в Threads.\n\nОткрыть публикацию: " + publication.Permalink
	}
	if strings.TrimSpace(publication.ID) != "" {
		return "✅ Опубликовано в Threads.\n\nID публикации: " + publication.ID
	}
	return "✅ Опубликовано в Threads."
}

func threadDraftCancelledText() string {
	return "Черновик отменён. Ничего не опубликовано."
}
