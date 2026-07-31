package bot

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/domain"
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
	voice := "Belcanto"
	if draft.Voice == domain.ThreadVoiceAlisher {
		voice = "Алишер"
	}
	goal := threadGoalLabel(draft.Goal)
	return fmt.Sprintf(
		"🎼 Belcanto Threads\n\nПост готов — его можно публиковать без правок.\n\nАвтор: %s\nЦель: %s\nТон: тёплый и остроумный · без прямой продажи\n\n%s\n\n%d/500",
		voice, goal, draft.Text, utf8.RuneCountInString(draft.Text),
	)
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
	return "Эта версия поста уже устарела. Открой /threads — я подготовлю свежую."
}

func threadDraftStateText(draft domain.ThreadDraft) string {
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
