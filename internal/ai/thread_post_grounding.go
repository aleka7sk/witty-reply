package ai

import (
	"fmt"
	"strings"
	"unicode"
)

func validateThreadPostGroundedText(text, evidence string, request normalizedThreadPostRequest) error {
	if request.Material == "" || strings.TrimSpace(evidence) == "" || !strings.Contains(request.Material, evidence) {
		return fmt.Errorf("%w: Threads grounded text has no exact approved evidence", ErrInvalidResponse)
	}
	evidenceDigits := threadPostDigitTokens(evidence)
	for token := range threadPostDigitTokens(text) {
		if _, ok := evidenceDigits[token]; !ok {
			return fmt.Errorf("%w: Threads text introduces a digit absent from evidence", ErrInvalidResponse)
		}
	}
	quotes, valid := threadPostQuotedSpans(text)
	if !valid {
		return fmt.Errorf("%w: Threads text contains an unmatched quotation", ErrInvalidResponse)
	}
	for _, quote := range quotes {
		if !strings.Contains(evidence, quote) {
			return fmt.Errorf("%w: Threads text introduces a quotation absent from evidence", ErrInvalidResponse)
		}
	}
	lower := strings.ToLower(text)
	for _, forbidden := range []string{
		"гарантируем", "гарантия результата", "обещаем результат", "результат гарантирован",
		"успей", "успейте", "последнее место", "последние места", "только сегодня",
		"guaranteed result", "last chance", "limited spots", "only today",
	} {
		if strings.Contains(lower, forbidden) {
			return fmt.Errorf("%w: Threads text contains unsupported pressure or guarantee", ErrInvalidResponse)
		}
	}
	if string(request.Objective) != "trial" {
		words := threadPostWords(lower)
		for _, word := range words {
			for _, prefix := range []string{"запиш", "брониру", "регистрир", "куп", "покуп", "жазыл", "сатып", "book", "buy", "register"} {
				if strings.HasPrefix(word, prefix) {
					return fmt.Errorf("%w: Threads text contains sales pressure outside trial objective", ErrInvalidResponse)
				}
			}
		}
	}
	return nil
}

func threadPostDigitTokens(value string) map[string]struct{} {
	result := make(map[string]struct{})
	var token []rune
	flush := func() {
		if len(token) > 0 {
			result[string(token)] = struct{}{}
			token = token[:0]
		}
	}
	for _, character := range value {
		if unicode.IsDigit(character) {
			token = append(token, character)
		} else {
			flush()
		}
	}
	flush()
	return result
}

func threadPostQuotedSpans(value string) ([]string, bool) {
	var result []string
	for _, pair := range [][2]rune{{'«', '»'}, {'“', '”'}, {'"', '"'}} {
		inside := false
		var current []rune
		for _, character := range value {
			if !inside && character == pair[0] {
				inside = true
				current = current[:0]
				continue
			}
			if inside && character == pair[1] {
				span := strings.TrimSpace(string(current))
				if span != "" {
					result = append(result, span)
				}
				inside = false
				continue
			}
			if inside {
				current = append(current, character)
			}
		}
		if inside {
			return nil, false
		}
	}
	return result, true
}
