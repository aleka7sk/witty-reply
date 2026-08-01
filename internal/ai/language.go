package ai

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var nonLanguageTextPattern = regexp.MustCompile(`(?i)(?:https?://|www\.)\S+|\b\S+@\S+\.\S+\b`)

var (
	russianLanguageWords = wordSet(
		"я", "ты", "тебя", "это", "как", "почему", "что", "когда", "зачем", "если",
		"просто", "очень", "слишком", "нужно", "можно", "нет", "давай", "ещё", "еще",
		"говорить", "ответ", "попробуй", "скажи", "получилось", "смешно", "привет",
		"спасибо", "пожалуйста", "опять", "всё", "все", "кажется", "реально", "нормально",
		"человек", "сообщение", "придумал", "придумала", "знаю", "хочу",
	)
	kazakhLanguageWords = wordSet(
		"мен", "сен", "сені", "бұл", "қалай", "неге", "қашан", "егер", "бірақ",
		"үшін", "және", "керек", "жақсы", "бар", "жоқ", "айт", "ғой", "ма", "ме",
		"сәлем", "рақмет", "өтінемін", "қайта", "бәрі", "адам", "хабарлама", "дұрыс",
	)
)

// DetectSourceLanguage identifies Russian, Kazakh, English, or natural
// code-switching from source text. fallback is used only when the source has no
// useful language signal (for example, an emoji-only message). The result is a
// constrained language hint suitable for GenerationRequest.Language.
func DetectSourceLanguage(text, fallback string) string {
	fallback = fallbackLanguage(fallback)
	text = strings.ToLower(norm.NFKC.String(nonLanguageTextPattern.ReplaceAllString(text, " ")))

	latinLetters, cyrillicLetters, kazakhLetters := 0, 0, 0
	firstLatin, firstCyrillic := -1, -1
	for index, r := range text {
		switch {
		case unicode.In(r, unicode.Latin):
			latinLetters++
			if firstLatin < 0 {
				firstLatin = index
			}
		case unicode.In(r, unicode.Cyrillic):
			cyrillicLetters++
			if firstCyrillic < 0 {
				firstCyrillic = index
			}
			if strings.ContainsRune("әғқңөұүһі", r) {
				kazakhLetters++
			}
		}
	}

	words := languageWords(text)
	ruScore, kkScore := wordScore(words, russianLanguageWords), wordScore(words, kazakhLanguageWords)
	cyrillicLanguage := ""
	switch {
	case cyrillicLetters == 0:
	case kazakhLetters > 0 || kkScore > ruScore:
		cyrillicLanguage = "kk"
	case ruScore > 0:
		cyrillicLanguage = "ru"
	case fallback == "kk":
		// Russian and Kazakh share Cyrillic letters. Telegram locale is only
		// used to resolve text that contains no distinguishing letters/words.
		cyrillicLanguage = "kk"
	default:
		cyrillicLanguage = "ru"
	}

	hasEnglish := latinLetters >= 2
	hasCyrillic := cyrillicLetters >= 2
	if hasEnglish && hasCyrillic {
		parts := make([]string, 0, 3)
		if firstLatin >= 0 && firstLatin < firstCyrillic {
			parts = append(parts, "en")
		}
		if kazakhLetters > 0 && ruScore > 0 && kkScore > 0 {
			if firstLatin >= firstCyrillic {
				parts = append(parts, "kk", "ru")
			} else {
				parts = append(parts, "ru", "kk")
			}
		} else {
			parts = append(parts, cyrillicLanguage)
		}
		if firstLatin >= firstCyrillic {
			parts = append(parts, "en")
		}
		return uniqueLanguageParts(parts)
	}
	if hasCyrillic {
		if kazakhLetters > 0 && ruScore > 0 && kkScore > 0 {
			return "kk-ru"
		}
		return cyrillicLanguage
	}
	if hasEnglish {
		return "en"
	}
	return fallback
}

func fallbackLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(language, "_", "-")))
	switch {
	case strings.HasPrefix(language, "kk"), strings.HasPrefix(language, "kz"):
		return "kk"
	case strings.HasPrefix(language, "en"):
		return "en"
	default:
		return "ru"
	}
}

func languageWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r)
	})
}

func wordSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func wordScore(words []string, lexicon map[string]struct{}) int {
	score := 0
	for _, word := range words {
		if _, ok := lexicon[word]; ok {
			score++
		}
	}
	return score
}

func uniqueLanguageParts(parts []string) string {
	seen := make(map[string]bool, len(parts))
	unique := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		unique = append(unique, part)
	}
	return strings.Join(unique, "-")
}
