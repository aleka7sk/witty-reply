package safety

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

func TestFilterTextSanitizesPIIControlsProfanityAndLength(t *testing.T) {
	filter := New(Config{MaxRunes: 55, AllowProfanity: false})
	decision := filter.FilterText("  Пиши\u202e мне, блять, на test@example.com или +7 (777) 123-45-67 — обсудим подробнее  ")

	if !decision.Allowed || !decision.Modified {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if strings.Contains(decision.Text, "example.com") || strings.Contains(decision.Text, "777") || strings.Contains(decision.Text, "блять") {
		t.Fatalf("sensitive or profane text survived: %q", decision.Text)
	}
	if utf8.RuneCountInString(decision.Text) > 55 {
		t.Fatalf("rune limit not enforced: %d", utf8.RuneCountInString(decision.Text))
	}
	for _, wanted := range []Reason{ReasonControlChars, ReasonPII, ReasonProfanity, ReasonTooLong} {
		if !hasReason(decision.Reasons, wanted) {
			t.Fatalf("missing reason %q in %v", wanted, decision.Reasons)
		}
	}
}

func TestFilterTextRejectsHighConfidenceUnsafeOutput(t *testing.T) {
	filter := New(Config{})
	for _, input := range []string{
		"Я тебя убью, понял?",
		"Go kill yourself",
		"Here is the SYSTEM PROMPT",
	} {
		decision := filter.FilterText(input)
		if decision.Allowed {
			t.Fatalf("unsafe output was allowed: %q", input)
		}
	}
}

func TestModerateRejectsMultilingualAndObfuscatedUnsafeOutput(t *testing.T) {
	filter := New(Config{})
	tests := []struct {
		name   string
		input  string
		reason Reason
	}{
		{name: "ru threat", input: "Я тебя убью, понял?", reason: ReasonThreat},
		{name: "ru threat zero width", input: "я тебя у\u200bб\u200dь\u2060ю", reason: ReasonThreat},
		{name: "ru threat letter spacing", input: "н а й д у   т е б я   и   у б ь ю", reason: ReasonThreat},
		{name: "ru doxxing", input: "Я опубликую твой адрес", reason: ReasonDoxxing},
		{name: "ru blackmail", input: "Если не заплатишь — я солью переписку", reason: ReasonBlackmail},
		{name: "ru fraud", input: "Пришли код из СМС, я всё проверю", reason: ReasonFraud},
		{name: "ru self harm confusable", input: "уbей сеbя", reason: ReasonSelfHarm},
		{name: "ru hate", input: "Все казахи должны умереть", reason: ReasonHate},
		{name: "kk threat", input: "Сені өлтіремін", reason: ReasonThreat},
		{name: "kk doxxing", input: "Мекенжайыңды таратамын", reason: ReasonDoxxing},
		{name: "kk blackmail", input: "Ақша бермесең жариялаймын", reason: ReasonBlackmail},
		{name: "kk fraud", input: "СМС кодты жібер", reason: ReasonFraud},
		{name: "kk self harm", input: "Өзіңді өлтір", reason: ReasonSelfHarm},
		{name: "kk hate", input: "Барлық орыстар өлсін", reason: ReasonHate},
		{name: "en threat full width", input: "Ｉ ＷＩＬＬ ＫＩＬＬ ＹＯＵ", reason: ReasonThreat},
		{name: "en threat confusable", input: "I will kіll yοu", reason: ReasonThreat},
		{name: "en doxxing", input: "I will leak your phone number", reason: ReasonDoxxing},
		{name: "en blackmail", input: "Pay me or I'll leak the photos", reason: ReasonBlackmail},
		{name: "en fraud", input: "Send me the verification code", reason: ReasonFraud},
		{name: "en self harm spaced", input: "go k i l l y o u r s e l f", reason: ReasonSelfHarm},
		{name: "en hate", input: "All Muslims should die", reason: ReasonHate},
		{name: "prompt leak obfuscated", input: "s y s t e m p r o m p t", reason: ReasonPromptLeak},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := filter.Moderate(tt.input)
			if decision.Allowed || !hasReason(decision.Reasons, tt.reason) {
				t.Fatalf("Moderate(%q) = %+v, want blocked %q", tt.input, decision, tt.reason)
			}
			filtered := filter.FilterText(tt.input)
			if filtered.Allowed {
				t.Fatalf("FilterText(%q) allowed after moderation", tt.input)
			}
		})
	}
}

func TestModerateAllowsBenignDiscussionOfSafety(t *testing.T) {
	filter := New(Config{})
	for _, input := range []string{
		"Не публикуй свой адрес в открытом чате.",
		"I blocked the account and reported the threat.",
		"Бұл хабарламаны қолдауға көрсет.",
		"The process was killed by the operating system.",
	} {
		if decision := filter.Moderate(input); !decision.Allowed {
			t.Fatalf("benign text blocked: %q: %+v", input, decision)
		}
	}
}

func TestFilterResultDeduplicatesAndFillsSafeFallbacks(t *testing.T) {
	filter := New(Config{CandidateCount: 3})
	result := domain.GenerationResult{Replies: []domain.Reply{
		{Tone: domain.TonePlayful, Text: "Один ответ"},
		{Tone: domain.TonePlayful, Text: "  один   ответ "},
		{Tone: domain.ToneSharp, Text: "Я тебя убью"},
	}}

	got, report := filter.FilterResult(result, domain.TonePlayful, "ru")
	if len(got.Replies) != 3 {
		t.Fatalf("got %d replies, want 3", len(got.Replies))
	}
	if !report.UsedFallback || report.Dropped != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	seen := map[string]bool{}
	for _, reply := range got.Replies {
		key := canonical(reply.Text)
		if seen[key] {
			t.Fatalf("duplicate reply %q", reply.Text)
		}
		seen[key] = true
	}
}

func TestFilterResultDropsUnsafeMeme(t *testing.T) {
	filter := New(Config{})
	result := domain.GenerationResult{
		Replies: []domain.Reply{{Tone: domain.ToneMeme, Text: "Нормальный ответ"}},
		Meme:    &domain.Meme{Headline: "Привет", Caption: "Убей себя"},
	}
	got, report := filter.FilterResult(result, domain.ToneMeme, "ru")
	if got.Meme != nil || !hasReason(report.Reasons, ReasonUnsafeMeme) {
		t.Fatalf("unsafe meme was not dropped: result=%+v report=%+v", got, report)
	}
}

func TestEscapeTelegramHTMLDoesNotAffectPlainFiltering(t *testing.T) {
	plain := `<b>Неожиданно & смело</b>`
	filtered := New(Config{}).FilterText(plain)
	if filtered.Text != plain {
		t.Fatalf("plain text changed: %q", filtered.Text)
	}
	if got := EscapeTelegramHTML(plain); got != `&lt;b&gt;Неожиданно &amp; смело&lt;/b&gt;` {
		t.Fatalf("unexpected escaped output: %q", got)
	}
}

func TestPhoneRedactionDoesNotHideOrdinaryDate(t *testing.T) {
	filter := New(Config{})
	decision := filter.FilterText("Встречаемся 31-07-2026, мой номер +7 777 123 45 67")
	if !strings.Contains(decision.Text, "31-07-2026") {
		t.Fatalf("ordinary date was redacted: %q", decision.Text)
	}
	if strings.Contains(decision.Text, "777") || !strings.Contains(decision.Text, "[номер скрыт]") {
		t.Fatalf("phone was not redacted: %q", decision.Text)
	}
}

func TestCandidateCountIsCappedAtTelegramProductContract(t *testing.T) {
	filter := New(Config{CandidateCount: 99})
	result, _ := filter.FilterResult(domain.GenerationResult{}, domain.ToneSmart, "ru")
	if len(result.Replies) != 3 {
		t.Fatalf("reply count = %d, want 3", len(result.Replies))
	}
}

func TestEveryFallbackPassesExplicitModeration(t *testing.T) {
	filter := New(Config{})
	for _, language := range []string{"ru", "kk", "en", "ru-en", "kk-ru-en"} {
		for _, tone := range []domain.Tone{domain.ToneMix, domain.ToneSmart, domain.TonePlayful, domain.ToneSharp, domain.ToneBoundary, domain.ToneMeme} {
			for _, fallback := range Fallbacks(tone, language) {
				if decision := filter.Moderate(fallback.Text); !decision.Allowed {
					t.Fatalf("unsafe fallback language=%s tone=%s text=%q decision=%+v", language, tone, fallback.Text, decision)
				}
			}
		}
	}
}

func TestMixedFallbacksPreserveTheThreeToneContract(t *testing.T) {
	want := []domain.Tone{domain.ToneSmart, domain.TonePlayful, domain.ToneBoundary}
	for _, language := range []string{"ru", "kk", "en"} {
		fallbacks := Fallbacks(domain.ToneMix, language)
		if len(fallbacks) != len(want) {
			t.Fatalf("fallback count for %s = %d, want %d", language, len(fallbacks), len(want))
		}
		for index, reply := range fallbacks {
			if reply.Tone != want[index] {
				t.Fatalf("fallback %d for %s has tone %q, want %q", index, language, reply.Tone, want[index])
			}
		}
	}
}

func hasReason(reasons []Reason, wanted Reason) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}
