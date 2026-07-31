package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

type language string

const (
	langRU language = "ru"
	langKK language = "kk"
	langEN language = "en"
)

func userLanguage(code string) language {
	code = strings.ToLower(strings.TrimSpace(code))
	switch {
	case strings.HasPrefix(code, "kk"), strings.HasPrefix(code, "kz"):
		return langKK
	case strings.HasPrefix(code, "en"):
		return langEN
	default:
		return langRU
	}
}

func startText(lang language, privacyURL string, voiceExternal bool) string {
	voiceRU := ""
	voiceKK := ""
	voiceEN := ""
	if voiceExternal {
		voiceRU = " Голосовые сначала передаются отдельному сервису распознавания речи."
		voiceKK = " Дауыстық жазба алдымен бөлек сөйлеуді тану сервисіне жіберіледі."
		voiceEN = " Voice notes are first sent to a separate speech-to-text provider."
	}
	policyRU := policyLink("Политика конфиденциальности", privacyURL)
	policyKK := policyLink("Құпиялылық саясаты", privacyURL)
	policyEN := policyLink("Privacy policy", privacyURL)
	switch lang {
	case langKK:
		return "Мен — Witty Reply. Хабарламаны, постты, скриншотты немесе дауыстық жазбаны жібер: адамға жауап керек пе, әлде постқа тапқыр пікір керек пе — өзім анықтап, үш нұсқа ұсынамын.\n\nБот жеке чаттарыңды өзі оқымайды. Кәдімгі режим ештеңені жарияламайды; Belcanto операторлық режимі постты тек қолмен растағаннан кейін жариялайды. Өңдеу үшін жіберген контент AI-провайдерге беріледі." + voiceKK + "\n\n" + policyKK + "\n\nЖалғастыруға келісесің бе?"
	case langEN:
		return "I'm Witty Reply. Send a message, post, screenshot, or voice note. I'll detect whether you need a direct reply or a witty public comment and suggest three options.\n\nI cannot read your private chats. The ordinary flow never publishes; the operator-only Belcanto workspace publishes a post only after an explicit confirmation. Submitted content is processed by the configured AI provider." + voiceEN + "\n\n" + policyEN + "\n\nDo you agree to continue?"
	default:
		return "Я — Ответочка. Пришли сообщение, пост, скриншот или голосовое. Я пойму, нужно ответить человеку или залететь с остроумным комментарием, и предложу три готовых варианта.\n\nБот не читает личные чаты сам. Обычный режим ничего не публикует; операторский раздел Belcanto отправляет пост в Threads только после явного подтверждения. Отправленный контент обрабатывается внешним AI-провайдером." + voiceRU + "\n\n" + policyRU + "\n\nСогласен продолжить?"
	}
}

func consentAcceptedText(lang language) string {
	switch lang {
	case langKK:
		return "Дайын. Хабарламаны, постты немесе скриншотты жібер — режимді өзім анықтаймын. Қажет болса, оны бір батырмамен ауыстыра аласың."
	case langEN:
		return "Ready. Send a message, post, or screenshot and I'll detect the scenario. You can correct it with one tap."
	default:
		return "Готово. Пришли сообщение, пост или скриншот — я сам определю, нужен ответ человеку или комментарий под публикацией. Если захочешь, режим можно переключить одним нажатием."
	}
}

func consentRequiredText(lang language) string {
	switch lang {
	case langKK:
		return "Алдымен AI-өңдеуге келісім керек. /start командасын басып, шарттарды раста."
	case langEN:
		return "I need your consent before sending content to an AI provider. Open /start and confirm first."
	default:
		return "Сначала нужно согласие на AI-обработку. Открой /start и подтверди условия."
	}
}

func helpText(lang language) string {
	switch lang {
	case langKK:
		return "Жіберуге болады:\n• жеке хабарлама, пост немесе қайта жіберілген мәтін;\n• чат не жарияланым скриншоты (10 МБ дейін);\n• дауыстық жазба (20 МБ дейін).\n\nРежим автоматты анықталады. Нақты таңдау үшін мәтінді «ответь:» немесе «коммент:» деп баста. Belcanto командасы /threads арқылы дайын пост жасап, оны жариялау алдында растауды сұрай алады.\n\nКомандалар: /new, /cancel, /style, /plan, /privacy, /threads, /delete_me."
	case langEN:
		return "Send:\n• a private message, post, or forwarded text;\n• a chat or publication screenshot (up to 10 MB);\n• a voice note (up to 20 MB).\n\nThe mode is automatic. Start with “reply:” or “comment:” to force it. Belcanto operators can use /threads for a publish-ready school post.\n\nCommands: /new, /cancel, /style, /plan, /privacy, /threads, /delete_me."
	default:
		return "Можно прислать:\n• личное сообщение, пост или пересланный текст;\n• скриншот переписки или публикации до 10 МБ;\n• голосовое до 20 МБ.\n\nРежим определяется автоматически. Для точного выбора начни текст с «ответь:» или «коммент:». После результата режим можно переключить без повторной отправки исходника. Для команды Belcanto: /threads сам подготовит готовый пост и попросит только подтвердить публикацию.\n\nКоманды: /new, /cancel, /style, /plan, /privacy, /threads, /delete_me."
	}
}

func privacyText(lang language, privacyURL string, voiceExternal bool) string {
	voiceRU := ""
	voiceKK := ""
	voiceEN := ""
	if voiceExternal {
		voiceRU = " Голосовые также обрабатывает отдельный speech-to-text провайдер."
		voiceKK = " Дауыстық жазбаны бөлек speech-to-text провайдері де өңдейді."
		voiceEN = " Voice notes are also processed by a separate speech-to-text provider."
	}
	switch lang {
	case langKK:
		return "Құпиялылық: бастапқы контент кезекте шифрланған түрде қысқа уақыт сақталады, ал ашық мәтін логтарға жазылмайды. Жасалған жауаптар, Belcanto Threads нобайлары және ашық feedback әдетте 7 күн сақталады. Threads нобайының дәл мәтіні Meta-ға тек оператор растағаннан кейін жіберіледі." + voiceKK + " /delete_me барлық профиль деректерін жояды.\n\n" + policyLink("Толық саясат", privacyURL)
	case langEN:
		return "Privacy: submitted content is held briefly as an encrypted queue payload; plaintext content is never written to logs. Generated replies, Belcanto Threads drafts, and explicit feedback are normally retained for 7 days. The exact Threads preview is sent to Meta only after an operator confirms publication." + voiceEN + " /delete_me removes all profile data.\n\n" + policyLink("Full policy", privacyURL)
	default:
		return "Приватность: отправленный контент кратковременно хранится в очереди только в зашифрованном виде; открытый текст не попадает в логи. Сгенерированные ответы, черновики Belcanto Threads и явный feedback обычно хранятся 7 дней. Точный текст Threads-поста передаётся Meta только после подтверждения оператора." + voiceRU + " /delete_me удаляет все данные профиля.\n\n" + policyLink("Полная политика", privacyURL)
	}
}

func policyLink(label, privacyURL string) string {
	if strings.TrimSpace(privacyURL) == "" {
		return label + ": /privacy"
	}
	return label + ": " + strings.TrimSpace(privacyURL)
}

func unsupportedText(lang language) string {
	switch lang {
	case langKK:
		return "Бұл форматты әлі түсінбеймін. Мәтін, скриншот немесе дауыстық жазба жібер."
	case langEN:
		return "I don't support that format yet. Send text, a screenshot, or a voice note."
	default:
		return "Такой формат пока не поддерживается. Пришли текст, скриншот или голосовое."
	}
}

func processingErrorText(lang language) string {
	switch lang {
	case langKK:
		return "Жауапты қазір дайындай алмадым. Бұл сұрау үшін лимит қайта алынбайды — сәл кейінірек қайталап көр."
	case langEN:
		return "I couldn't prepare the replies right now. Try again shortly."
	default:
		return "Сейчас не получилось подготовить ответы. Попробуй ещё раз чуть позже."
	}
}

func voiceDisabledText(lang language) string {
	switch lang {
	case langKK:
		return "Дауыстық жазбаларды тану бұл іске қосуда өшірулі. Мәтінді көшіріп жіберші."
	case langEN:
		return "Voice transcription is disabled in this deployment. Please paste the message as text."
	default:
		return "Распознавание голосовых в этом запуске отключено. Пришли сообщение текстом."
	}
}

func voiceLowConfidenceText(lang language) string {
	switch lang {
	case langKK:
		return "Дауыстық жазбаны сенімді тани алмадым. Мәтінді көшіріп жібер немесе анығырақ жазба жібер."
	case langEN:
		return "I couldn't transcribe that voice note reliably. Paste the text or send a clearer recording."
	default:
		return "Не получилось надёжно распознать голосовое. Пришли текст или более чёткую запись."
	}
}

func invalidInputText(lang language) string {
	switch lang {
	case langKK:
		return "Файлды сенімді түрде оқи алмадым. Анығырақ скриншот немесе мәтін жібер."
	case langEN:
		return "I couldn't read that safely. Send a clearer screenshot or paste the text."
	default:
		return "Не получилось надёжно прочитать файл. Пришли более чёткий скриншот или вставь текст."
	}
}

func contextClearedText(lang language) string {
	switch lang {
	case langKK:
		return "Алдыңғы контекст өшірілді. Жаңа хабарлама жібер."
	case langEN:
		return "Previous context cleared. Send a new message."
	default:
		return "Предыдущий контекст удалён. Пришли новое сообщение."
	}
}

func cancelledText(lang language) string {
	switch lang {
	case langKK:
		return "Тоқтатылды."
	case langEN:
		return "Cancelled."
	default:
		return "Отменено."
	}
}

func styleText(lang language, tone domain.Tone, used, limit int) string {
	label := toneLabel(lang, tone)
	switch lang {
	case langKK:
		return fmt.Sprintf("Негізгі стиль: %s\nСақталған мысалдар: %d/%d\n\nЖұлдызша батырмасы арқылы ұнаған жауапты стиль мысалы ретінде сақта.", label, used, limit)
	case langEN:
		return fmt.Sprintf("Default style: %s\nSaved examples: %d/%d\n\nUse the star button under a result to save a reply as a style example.", label, used, limit)
	default:
		return fmt.Sprintf("Основной стиль: %s\nСохранённые примеры: %d/%d\n\nКнопка со звёздочкой под результатом сохранит понравившийся ответ как пример твоего стиля.", label, used, limit)
	}
}

func styleChangedText(lang language, tone domain.Tone) string {
	switch lang {
	case langKK:
		return "Негізгі стиль өзгертілді: " + toneLabel(lang, tone)
	case langEN:
		return "Default style changed to: " + toneLabel(lang, tone)
	default:
		return "Основной стиль изменён: " + toneLabel(lang, tone)
	}
}

func styleResetText(lang language) string {
	switch lang {
	case langKK:
		return "Стиль мен сақталған мысалдар өшірілді."
	case langEN:
		return "Style preferences and saved examples were reset."
	default:
		return "Настройки стиля и сохранённые примеры сброшены."
	}
}

func styleSavedText(lang language) string {
	switch lang {
	case langKK:
		return "Сақталды. Келесі жауаптар осы стильге жақынырақ болады."
	case langEN:
		return "Saved. Future replies will lean closer to this style."
	default:
		return "Сохранил. Следующие ответы будут ближе к этому стилю."
	}
}

func styleLimitText(lang language, limit int) string {
	switch lang {
	case langKK:
		return fmt.Sprintf("Тегін профильде %d стиль мысалына дейін сақтауға болады. /style арқылы ескілерін өшір.", limit)
	case langEN:
		return fmt.Sprintf("The free profile stores up to %d style examples. Reset them through /style first.", limit)
	default:
		return fmt.Sprintf("В бесплатном профиле можно сохранить до %d примеров. Сначала сбрось их через /style.", limit)
	}
}

func feedbackThanksText(lang language) string {
	switch lang {
	case langKK:
		return "Рақмет, белгіледім."
	case langEN:
		return "Thanks — noted."
	default:
		return "Спасибо, учёл."
	}
}

func deleteConfirmText(lang language) string {
	switch lang {
	case langKK:
		return "Профильді, жауаптарды, feedback пен стиль мысалдарын біржола жою керек пе?"
	case langEN:
		return "Permanently delete your profile, generated replies, feedback, and style examples?"
	default:
		return "Безвозвратно удалить профиль, сгенерированные ответы, feedback и примеры стиля?"
	}
}

func deletedText(lang language) string {
	switch lang {
	case langKK:
		return "Барлық профиль деректері жойылды. Қайта бастау үшін /start."
	case langEN:
		return "All profile data was deleted. Use /start if you want to begin again."
	default:
		return "Все данные профиля удалены. Если захочешь начать заново — /start."
	}
}

func deleteCancelledText(lang language) string {
	switch lang {
	case langKK:
		return "Жоюдан бас тартылды."
	case langEN:
		return "Deletion cancelled."
	default:
		return "Удаление отменено."
	}
}

func expiredText(lang language) string {
	switch lang {
	case langKK:
		return "Бұл нәтиженің бастапқы контексті жадтан жойылған. Хабарламаны қайта жібер."
	case langEN:
		return "The private context for this result has expired. Send the source message again."
	default:
		return "Исходный контекст этого результата уже удалён из памяти. Пришли сообщение ещё раз."
	}
}

func modeChoiceText(lang language) string {
	switch lang {
	case langKK:
		return "Мұнда екі түрлі мақсат болуы мүмкін. Не жазғымыз келеді?"
	case langEN:
		return "I can read this in two different ways. What do you want to write?"
	default:
		return "Здесь возможны два разных сценария. Что хотим написать?"
	}
}

func quotaText(lang language, decision domain.QuotaDecision) string {
	reset := decision.ResetsAt.Format("15:04")
	switch lang {
	case langKK:
		return fmt.Sprintf("Бүгінгі лимит аяқталды (%d/%d). %s-де жаңарады.", decision.Used, decision.Limit, reset)
	case langEN:
		return fmt.Sprintf("Today's limit is used (%d/%d). It resets at %s.", decision.Used, decision.Limit, reset)
	default:
		return fmt.Sprintf("Лимит на сегодня закончился (%d/%d). Обновится в %s.", decision.Used, decision.Limit, reset)
	}
}

func planText(lang language, stats domain.UserStats, resetsAt time.Time) string {
	remaining := stats.DailyLimit - stats.UsedToday
	if remaining < 0 {
		remaining = 0
	}
	switch lang {
	case langKK:
		return fmt.Sprintf("Жоспар: %s\nБүгін қолданылды: %d\nНегізгі лимит: %d\nҚалды: %d\nЖаңару: %s", stats.Plan, stats.UsedToday, stats.DailyLimit, remaining, resetsAt.Format("02.01 15:04 MST"))
	case langEN:
		return fmt.Sprintf("Plan: %s\nUsed today: %d\nBase limit: %d\nRemaining: %d\nReset: %s", stats.Plan, stats.UsedToday, stats.DailyLimit, remaining, resetsAt.Format("02 Jan 15:04 MST"))
	default:
		return fmt.Sprintf("Тариф: %s\nИспользовано сегодня: %d\nБазовый лимит: %d\nОсталось: %d\nОбновление: %s", stats.Plan, stats.UsedToday, stats.DailyLimit, remaining, resetsAt.Format("02.01 15:04 MST"))
	}
}

func resultsText(lang language, result domain.GenerationResult) string {
	var builder strings.Builder
	if result.Mode == domain.ScenarioComment {
		switch lang {
		case langKK:
			builder.WriteString("🔥 Режим: пікірлерге кіру\n\nҮш түрлі әзіл:\n\n")
		case langEN:
			builder.WriteString("🔥 Mode: Comment under the post\n\nThree different angles:\n\n")
		default:
			builder.WriteString("🔥 Режим: Залететь в комменты\n\nТри шутки с разными заходами:\n\n")
		}
		for index, reply := range result.Replies {
			fmt.Fprintf(&builder, "%d. %s\n%s", index+1, commentCandidateLabel(lang, index), reply.Text)
			if index < len(result.Replies)-1 {
				builder.WriteString("\n\n")
			}
		}
		return builder.String()
	}
	switch lang {
	case langKK:
		builder.WriteString("↩️ Режим: адамға жауап беру\n\nМына үшеудің қайсысы саған жақын?\n\n")
	case langEN:
		builder.WriteString("↩️ Mode: Reply to the person\n\nWhich one sounds most like you?\n\n")
	default:
		builder.WriteString("↩️ Режим: Ответить человеку\n\nКакой вариант больше похож на тебя?\n\n")
	}
	for index, reply := range result.Replies {
		fmt.Fprintf(&builder, "%d. %s\n%s", index+1, toneLabel(lang, reply.Tone), reply.Text)
		if index < len(result.Replies)-1 {
			builder.WriteString("\n\n")
		}
	}
	return builder.String()
}

func commentCandidateLabel(lang language, index int) string {
	labels := map[language][]string{
		langRU: {"🏆 Самый сильный", "🧠 Тонкий", "🤪 Дикий"},
		langKK: {"🏆 Ең мықты", "🧠 Нәзік", "🤪 Еркін"},
		langEN: {"🏆 Strongest", "🧠 Subtle", "🤪 Wild"},
	}
	values := labels[lang]
	if index >= 0 && index < len(values) {
		return values[index]
	}
	return toneLabel(lang, domain.ToneMix)
}

func toneLabel(lang language, tone domain.Tone) string {
	labels := map[language]map[domain.Tone]string{
		langRU: {domain.ToneSmart: "Умно", domain.TonePlayful: "С подколом", domain.ToneSharp: "Остро", domain.ToneBoundary: "Уверенно", domain.ToneMeme: "Мем", domain.ToneMix: "Вариант"},
		langKK: {domain.ToneSmart: "Ақылды", domain.TonePlayful: "Әзілмен", domain.ToneSharp: "Өткір", domain.ToneBoundary: "Нық", domain.ToneMeme: "Мем", domain.ToneMix: "Нұсқа"},
		langEN: {domain.ToneSmart: "Smart", domain.TonePlayful: "Playful", domain.ToneSharp: "Sharp", domain.ToneBoundary: "Firm", domain.ToneMeme: "Meme", domain.ToneMix: "Option"},
	}
	if label := labels[lang][tone]; label != "" {
		return label
	}
	return labels[lang][domain.ToneMix]
}
