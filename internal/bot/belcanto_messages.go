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
		return "I couldn't prepare a reliable post. Nothing was published. Send /threads to try again."
	}
	return "Не получилось подготовить надёжный пост. Ничего не опубликовано. Отправь /threads, чтобы попробовать заново."
}

func threadDraftText(draft domain.ThreadDraft) string {
	return threadDraftTextWithOptionalMedia(draft, nil)
}

func threadDraftTextWithMedia(draft domain.ThreadDraft, mediaValue domain.ThreadMedia) string {
	return threadDraftTextWithOptionalMedia(draft, &mediaValue)
}

func threadDraftTextWithOptionalMedia(draft domain.ThreadDraft, mediaValue *domain.ThreadMedia) string {
	voice := "Belcanto"
	if draft.Voice == domain.ThreadVoiceAlisher {
		voice = "Алишер"
	}
	goal := threadGoalLabel(draft.Goal)
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
				"\n\nНажимая «Права есть — опубликовать», ты подтверждаешь нейтральный уместный контекст без впечатления, будто изображённые люди или бренды поддерживают Belcanto."
		}
	}
	return fmt.Sprintf(
		"🎼 Belcanto Threads\n\n%s\n\nАвтор: %s\nЦель: %s\nФормат: %s\nТон: тёплый и остроумный · без прямой продажи\n\n%s\n\n%d/500%s",
		status, voice, goal, format, draft.Text, utf8.RuneCountInString(draft.Text), rights,
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

func threadGoalLabel(goal string) string {
	switch strings.ToLower(strings.TrimSpace(goal)) {
	case "recognition":
		return "узнавание себя"
	case "warmth":
		return "тёплое узнавание"
	default:
		return "обсуждение"
	}
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
