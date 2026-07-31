// Package safety applies deterministic, last-mile checks to model output.
// It is deliberately conservative: a model/provider safety policy remains the
// first line of defence, while this package prevents obviously unsafe or
// malformed text from reaching Telegram.
package safety

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"golang.org/x/text/unicode/norm"
)

const (
	defaultMaxRunes       = 240
	defaultCandidateCount = 3
	maxCandidateCount     = 3
)

// Reason identifies why output was changed or rejected. Values are safe to
// use in metrics; they never contain user or model content.
type Reason string

const (
	ReasonEmpty         Reason = "empty"
	ReasonControlChars  Reason = "control_chars"
	ReasonTooLong       Reason = "too_long"
	ReasonPII           Reason = "pii"
	ReasonProfanity     Reason = "profanity"
	ReasonThreat        Reason = "threat"
	ReasonDoxxing       Reason = "doxxing"
	ReasonBlackmail     Reason = "blackmail"
	ReasonFraud         Reason = "fraud"
	ReasonHate          Reason = "hate_or_degradation"
	ReasonVulnerability Reason = "vulnerability_attack"
	ReasonSelfHarm      Reason = "self_harm"
	ReasonPromptLeak    Reason = "prompt_leak"
	ReasonDuplicate     Reason = "duplicate"
	ReasonUnsafeMeme    Reason = "unsafe_meme"
	ReasonFallback      Reason = "fallback"
)

type Config struct {
	MaxRunes       int
	CandidateCount int
	AllowProfanity bool
}

type Decision struct {
	Text     string
	Allowed  bool
	Modified bool
	Reasons  []Reason
}

// ModerationDecision is the fail-closed, deterministic verdict applied after
// provider-side moderation and before any generated text reaches Telegram.
// It intentionally reports only stable reason codes and never echoes content.
type ModerationDecision struct {
	Allowed bool
	Reasons []Reason
}

type Report struct {
	Dropped      int
	Modified     int
	UsedFallback bool
	Reasons      []Reason
}

type Filter struct {
	maxRunes       int
	candidateCount int
	allowProfanity bool
}

func New(cfg Config) *Filter {
	if cfg.MaxRunes <= 0 {
		cfg.MaxRunes = defaultMaxRunes
	}
	if cfg.CandidateCount <= 0 {
		cfg.CandidateCount = defaultCandidateCount
	} else if cfg.CandidateCount > maxCandidateCount {
		cfg.CandidateCount = maxCandidateCount
	}
	return &Filter{
		maxRunes:       cfg.MaxRunes,
		candidateCount: cfg.CandidateCount,
		allowProfanity: cfg.AllowProfanity,
	}
}

var (
	emailPattern     = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+`)
	phonePattern     = regexp.MustCompile(`(?:\+?\d[\d ()-]{7,}\d)`)
	cardPattern      = regexp.MustCompile(`(?:\d[ -]?){12,19}\d`)
	spacePattern     = regexp.MustCompile(`\s+`)
	profanityPattern = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])(?:бля(?:ть|дь)?|сука|нахуй|пош[её]л нахуй|хуй|пиздец|ебать)([^\p{L}\p{N}]|$)`)
)

type phraseRule struct {
	reason  Reason
	phrases []string
}

// These are deliberately high-confidence phrases, not a claim of complete
// semantic moderation. Provider safeguards remain the first layer; this list
// is an independent multilingual last-mile guard for a short-reply product.
var blockedPhraseRules = []phraseRule{
	{reason: ReasonThreat, phrases: []string{
		// Russian.
		"я тебя убью", "убью тебя", "я тебя прикончу", "прикончу тебя",
		"я тебя зарежу", "зарежу тебя", "я тебя пристрелю", "пристрелю тебя",
		"я тебя покалечу", "сломаю тебе ноги", "тебе конец", "ты труп",
		"найду и убью", "найду тебя и убью", "взорву твой дом", "сожгу твой дом",
		// Kazakh.
		"сені өлтіремін", "сені өлтірем", "өлтіремін сені", "өлтірем сені",
		"сені пышақтаймын", "пышақтап өлтіремін", "сені атып тастаймын",
		"үйіңді өртеймін", "тауып алып өлтіремін", "сен өліксің",
		// English.
		"i will kill you", "i'll kill you", "im going to kill you", "i am going to kill you",
		"gonna kill you", "i will murder you", "i will shoot you", "i will stab you",
		"i will hurt you", "i will break your legs", "you are dead", "you're dead",
		"hunt you down", "find you and kill you", "burn your house", "bomb your house",
	}},
	{reason: ReasonDoxxing, phrases: []string{
		// Russian.
		"солью твой адрес", "сливаю твой адрес", "выложу твой адрес", "опубликую твой адрес",
		"разошлю твой адрес", "солью твой номер", "выложу твой номер", "опубликую твой номер",
		"солью твои данные", "опубликую твои данные", "найду где ты живешь", "найду где ты живёшь",
		"задокшу тебя", "задоксю тебя",
		// Kazakh.
		"мекенжайыңды жариялаймын", "мекенжайыңды таратамын", "телефон нөміріңді жариялаймын",
		"телефон нөміріңді таратамын", "жеке деректеріңді жариялаймын", "жеке деректеріңді таратамын",
		"қайда тұратыныңды табамын",
		// English.
		"i will leak your address", "leak your address", "post your address", "publish your address",
		"share your home address", "leak your phone number", "post your phone number",
		"publish your phone number", "expose your personal information", "find where you live",
		"i will dox you", "i'll dox you", "dox you", "doxx you",
	}},
	{reason: ReasonBlackmail, phrases: []string{
		// Russian.
		"заплати иначе солью", "если не заплатишь я солью", "если не заплатишь солью",
		"сделай это иначе выложу", "иначе отправлю всем", "иначе расскажу всем",
		"солью переписку если", "у меня есть компромат на тебя", "буду тебя шантажировать",
		// Kazakh.
		"ақша бермесең жариялаймын", "ақша бермесең таратамын", "төлемесең жариялаймын",
		"істемесең бәріне айтамын", "әйтпесе бәріне айтамын", "сені бопсалаймын",
		// English.
		"pay me or i will leak", "pay me or i'll leak", "pay or i leak", "unless you pay i will publish",
		"send money or i will expose", "do it or i will post", "i will blackmail you", "blackmail you",
	}},
	{reason: ReasonFraud, phrases: []string{
		// Russian.
		"пришли код из смс", "сообщи код из смс", "дай пароль от аккаунта",
		"пришли пароль от аккаунта", "переведи деньги на безопасный счет",
		"переведи деньги на безопасный счёт",
		// Kazakh.
		"смс кодты жібер", "растау кодын жібер", "құпиясөзіңді жібер",
		"қауіпсіз шотқа ақша аудар",
		// English.
		"send me the verification code", "share your verification code",
		"send your password", "share your password", "transfer money to a safe account",
		"send me your one time code", "share your one time code",
	}},
	{reason: ReasonSelfHarm, phrases: []string{
		// Russian.
		"убей себя", "убейся", "выпились", "покончи с собой", "повесься", "утопись",
		"сдохни", "тебе лучше умереть",
		// Kazakh.
		"өзіңді өлтір", "өзіңді өлтіріп таста", "өз өзіңе қол жұмса", "асылып өл",
		"өліп қал", "өлгенің жақсы",
		// English.
		"kill yourself", "go kill yourself", "kys", "end your life", "hang yourself",
		"go die", "you should die", "better off dead",
	}},
	{reason: ReasonHate, phrases: []string{
		"грязная раса", "недочеловек", "ваша раса должна исчезнуть",
		"лас нәсіл", "сендердің нәсілдерің жойылсын",
		"subhuman", "dirty race", "your race should disappear",
	}},
	{reason: ReasonVulnerability, phrases: []string{
		// High-confidence body-shaming and pregnancy-directed degradation.
		"жируха", "жиробас", "толстуха", "жирная беременная", "толстая беременная",
		"беременная корова", "смешной выкидыш",
		"семіз жүкті әйел", "жүкті сиыр", "семіз сиыр",
		"fat pregnant woman", "pregnant cow", "fat cow", "land whale", "funny miscarriage",
	}},
	{reason: ReasonPromptLeak, phrases: []string{
		"system prompt", "developer message", "anthropic_api_key",
		"telegram_bot_token", "вот мои системные инструкции",
	}},
}

var (
	hateGroupTokens = matchTokenSet(
		"race", "races", "muslim", "muslims", "jew", "jews", "christian", "christians",
		"black", "blacks", "white", "whites", "asian", "asians", "russian", "russians",
		"kazakh", "kazakhs", "gay", "gays", "lesbian", "lesbians", "transgender",
		"раса", "расы", "казахи", "казахов", "русские", "русских", "евреи", "евреев",
		"мусульмане", "мусульман", "христиане", "христиан", "геи", "геев", "лесбиянки",
		"нәсіл", "қазақтар", "орыстар", "еврейлер", "мұсылмандар", "христиандар",
	)
	hateHarmTokens = matchTokenSet(
		"die", "dead", "killed", "destroyed", "disappear", "exterminated",
		"умереть", "сдохнуть", "уничтожить", "уничтожены", "исчезнуть",
		"өлсін", "өлуі", "өлтіру", "жойылсын", "жойылуы",
	)
	hateIntentTokens = matchTokenSet(
		"all", "every", "should", "must", "deserve",
		"все", "всех", "каждый", "должны", "надо", "пусть",
		"барлық", "әрбір", "керек",
	)
)

// FilterText normalizes plain output, redacts common contact data, enforces a
// rune-safe length limit, and rejects high-confidence unsafe phrases.
func (f *Filter) FilterText(text string) Decision {
	original := text
	text, controlsRemoved := normalizePlainText(text)
	reasons := make([]Reason, 0, 4)
	if controlsRemoved {
		reasons = append(reasons, ReasonControlChars)
	}
	if text == "" {
		return Decision{Allowed: false, Modified: original != "", Reasons: append(reasons, ReasonEmpty)}
	}

	moderation := f.Moderate(text)
	if !moderation.Allowed {
		return Decision{Allowed: false, Modified: true, Reasons: append(reasons, moderation.Reasons...)}
	}

	redacted := emailPattern.ReplaceAllString(text, "[email скрыт]")
	redacted = cardPattern.ReplaceAllString(redacted, "[номер скрыт]")
	redacted = redactPhoneCandidates(redacted)
	if redacted != text {
		text = redacted
		reasons = append(reasons, ReasonPII)
	}

	if !f.allowProfanity {
		for profanityPattern.MatchString(text) {
			masked := profanityPattern.ReplaceAllString(text, "$1***$2")
			if masked == text {
				break
			}
			text = masked
		}
		if text != redacted {
			reasons = append(reasons, ReasonProfanity)
		}
	}

	if utf8.RuneCountInString(text) > f.maxRunes {
		text = truncateRunes(text, f.maxRunes)
		reasons = append(reasons, ReasonTooLong)
	}
	return Decision{
		Text:     text,
		Allowed:  true,
		Modified: text != original,
		Reasons:  uniqueReasons(reasons),
	}
}

// Moderate performs the explicit last-mile moderation stage without rewriting
// or returning user/model content. Matching uses NFKC normalization, removes
// format controls and combining marks, folds common cross-script confusables,
// and detects punctuation- or letter-spaced evasions. Unknown content is not
// declared semantically safe; this is one deterministic layer after provider
// safeguards, not a general-purpose moderation model.
func (f *Filter) Moderate(text string) ModerationDecision {
	_ = f // Kept as a method so policy can become configuration-backed later.
	forms := newMatchForms(text)
	if forms.words == "" {
		return ModerationDecision{Allowed: false, Reasons: []Reason{ReasonEmpty}}
	}
	for _, rule := range blockedPhraseRules {
		for _, phrase := range rule.phrases {
			if forms.contains(phrase) {
				return ModerationDecision{Allowed: false, Reasons: []Reason{rule.reason}}
			}
		}
	}
	if matchesHateTemplate(forms.tokens) {
		return ModerationDecision{Allowed: false, Reasons: []Reason{ReasonHate}}
	}
	return ModerationDecision{Allowed: true}
}

// FilterResult returns a result containing exactly CandidateCount safe,
// distinct replies. Unsafe or missing candidates are replaced by deterministic
// language- and tone-aware fallbacks.
func (f *Filter) FilterResult(result domain.GenerationResult, tone domain.Tone, language string) (domain.GenerationResult, Report) {
	return f.FilterResultForMode(result, domain.ScenarioReply, tone, language)
}

// FilterResultForMode keeps last-mile fallbacks in the same social voice as
// the provider result. A rejected public comment must never turn into a
// defensive private-chat reply.
func (f *Filter) FilterResultForMode(result domain.GenerationResult, mode domain.ScenarioMode, tone domain.Tone, language string) (domain.GenerationResult, Report) {
	report := Report{}
	filtered := make([]domain.Reply, 0, f.candidateCount)
	seen := make(map[string]struct{}, f.candidateCount)

	for _, reply := range result.Replies {
		decision := f.FilterText(reply.Text)
		report.Reasons = append(report.Reasons, decision.Reasons...)
		if !decision.Allowed {
			report.Dropped++
			continue
		}
		key := canonical(decision.Text)
		if _, exists := seen[key]; exists {
			report.Dropped++
			report.Reasons = append(report.Reasons, ReasonDuplicate)
			continue
		}
		seen[key] = struct{}{}
		reply.Text = decision.Text
		if strings.TrimSpace(reply.Note) != "" {
			noteDecision := f.FilterText(reply.Note)
			report.Reasons = append(report.Reasons, noteDecision.Reasons...)
			if noteDecision.Allowed {
				reply.Note = noteDecision.Text
				if noteDecision.Modified {
					report.Modified++
				}
			} else {
				reply.Note = ""
			}
		}
		if _, ok := domain.ValidTones[reply.Tone]; !ok {
			reply.Tone = tone
		}
		if decision.Modified {
			report.Modified++
		}
		filtered = append(filtered, reply)
		if len(filtered) == f.candidateCount {
			break
		}
	}
	if strings.TrimSpace(result.Situation) != "" {
		decision := f.FilterText(result.Situation)
		report.Reasons = append(report.Reasons, decision.Reasons...)
		if decision.Allowed {
			result.Situation = decision.Text
			if decision.Modified {
				report.Modified++
			}
		} else {
			result.Situation = ""
		}
	}

	if result.Meme != nil {
		meme := *result.Meme
		headline := f.FilterText(meme.Headline)
		caption := f.FilterText(meme.Caption)
		footer := Decision{Text: "", Allowed: true}
		if strings.TrimSpace(meme.Footer) != "" {
			footer = f.FilterText(meme.Footer)
		}
		if !headline.Allowed || !caption.Allowed || !footer.Allowed {
			result.Meme = nil
			report.Dropped++
			report.Reasons = append(report.Reasons, ReasonUnsafeMeme)
		} else {
			meme.Headline, meme.Caption, meme.Footer = headline.Text, caption.Text, footer.Text
			result.Meme = &meme
			if headline.Modified || caption.Modified || footer.Modified {
				report.Modified++
			}
		}
	}

	if len(filtered) < f.candidateCount {
		for _, fallback := range FallbacksForMode(mode, tone, language) {
			decision := f.FilterText(fallback.Text)
			if !decision.Allowed {
				continue
			}
			key := canonical(decision.Text)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			fallback.Text = decision.Text
			filtered = append(filtered, fallback)
			if len(filtered) == f.candidateCount {
				break
			}
		}
		report.UsedFallback = true
		report.Reasons = append(report.Reasons, ReasonFallback)
	}
	if mode == domain.ScenarioComment && tone != domain.ToneMeme {
		filtered = orderCommentReplies(filtered)
	}

	result.Replies = filtered
	report.Reasons = uniqueReasons(report.Reasons)
	return result, report
}

func Fallbacks(tone domain.Tone, language string) []domain.Reply {
	return FallbacksForMode(domain.ScenarioReply, tone, language)
}

func FallbacksForMode(mode domain.ScenarioMode, tone domain.Tone, language string) []domain.Reply {
	language = strings.ToLower(strings.TrimSpace(language))
	toneAt := func(index int) domain.Tone {
		if tone != domain.ToneMix {
			return tone
		}
		return [...]domain.Tone{domain.ToneSmart, domain.TonePlayful, domain.ToneBoundary}[index]
	}
	if mode == domain.ScenarioComment {
		commentToneAt := func(index int) domain.Tone {
			if tone == domain.ToneMeme {
				return domain.ToneMeme
			}
			return [...]domain.Tone{domain.ToneSmart, domain.TonePlayful, domain.ToneBoundary}[index]
		}
		if strings.HasPrefix(language, "kk") || strings.HasPrefix(language, "kz") {
			return []domain.Reply{
				{Tone: commentToneAt(0), Text: "Бір сөйлем — ал жалғасына пікірлердің өзі дайын тұр."},
				{Tone: commentToneAt(1), Text: "Оқиға аяқталды, бірақ сұрақтар енді басталды."},
				{Tone: commentToneAt(2), Text: "Бұл ой бұрылысты ескертусіз жасаған екен."},
			}
		}
		if strings.HasPrefix(language, "en") {
			return []domain.Reply{
				{Tone: commentToneAt(0), Text: "One sentence, and the comments already have a sequel ready."},
				{Tone: commentToneAt(1), Text: "The post ended, but the questions just clocked in."},
				{Tone: commentToneAt(2), Text: "That thought took the scenic route without warning anyone."},
			}
		}
		return []domain.Reply{
			{Tone: commentToneAt(0), Text: "Одна фраза — а у комментариев уже готово продолжение."},
			{Tone: commentToneAt(1), Text: "Пост закончился, но вопросы только вышли на смену."},
			{Tone: commentToneAt(2), Text: "Эта мысль свернула в сюжет без предупреждения."},
		}
	}
	if strings.HasPrefix(language, "kk") || strings.HasPrefix(language, "kz") {
		return []domain.Reply{
			{Tone: toneAt(0), Text: "Күшті бастама. Енді нақты ойыңды айтасың ба?"},
			{Tone: toneAt(1), Text: "Әзілді түсіндім. Енді құрметпен сөйлесейік."},
			{Tone: toneAt(2), Text: "Бұл тонмен әңгімені жалғастырмаймын."},
		}
	}
	if strings.HasPrefix(language, "en") {
		return []domain.Reply{
			{Tone: toneAt(0), Text: "Strong entrance. Are the actual arguments coming next?"},
			{Tone: toneAt(1), Text: "I got the joke. Now let's try respect."},
			{Tone: toneAt(2), Text: "I won't continue this conversation in that tone."},
		}
	}

	switch tone {
	case domain.ToneSmart:
		return []domain.Reply{
			{Tone: tone, Text: "Давай без шума — по сути есть что сказать?"},
			{Tone: tone, Text: "Форма яркая. Теперь можно и к аргументам."},
			{Tone: tone, Text: "Услышал. Вернёмся к фактам?"},
		}
	case domain.ToneBoundary:
		return []domain.Reply{
			{Tone: tone, Text: "Со мной так разговаривать не нужно."},
			{Tone: tone, Text: "Продолжим, когда тон станет уважительным."},
			{Tone: tone, Text: "Границу обозначил. Дальше решать тебе."},
		}
	case domain.ToneSharp:
		return []domain.Reply{
			{Tone: tone, Text: "Сбавь тон. Так разговор не продолжится."},
			{Tone: tone, Text: "Громкость есть. Смысла пока не заметил."},
			{Tone: tone, Text: "Попробуй ещё раз, но уже с уважением."},
		}
	default:
		return []domain.Reply{
			{Tone: toneAt(0), Text: "Сильный заход. Аргументы во второй серии будут? 😏"},
			{Tone: toneAt(1), Text: "Шутку оценил. Теперь можно и по существу."},
			{Tone: toneAt(2), Text: "Звучит громко. А мысль где-то рядом?"},
		}
	}
}

func orderCommentReplies(replies []domain.Reply) []domain.Reply {
	ordered := make([]domain.Reply, 0, len(replies))
	used := make([]bool, len(replies))
	for _, expected := range []domain.Tone{domain.ToneSmart, domain.TonePlayful, domain.ToneBoundary} {
		for index, reply := range replies {
			if !used[index] && reply.Tone == expected {
				ordered = append(ordered, reply)
				used[index] = true
				break
			}
		}
	}
	for index, reply := range replies {
		if !used[index] {
			ordered = append(ordered, reply)
		}
	}
	return ordered
}

// EscapeTelegramHTML must be applied only at the transport boundary. Keeping
// it separate ensures copied replies contain the original plain text.
func EscapeTelegramHTML(text string) string {
	return html.EscapeString(text)
}

func normalizePlainText(value string) (string, bool) {
	value = norm.NFKC.String(value)
	var b strings.Builder
	b.Grow(len(value))
	removed := false
	for _, r := range value {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			removed = true
		default:
			b.WriteRune(r)
		}
	}
	normalized := strings.TrimSpace(spacePattern.ReplaceAllString(b.String(), " "))
	return normalized, removed
}

type matchForms struct {
	words   string
	compact string
	tokens  []string
}

func newMatchForms(value string) matchForms {
	value = strings.ToLower(norm.NFKC.String(value))
	var words strings.Builder
	words.Grow(len(value))
	separator := true
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) {
			continue
		}
		r = confusableSkeleton(r)
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			words.WriteRune(r)
			separator = false
			continue
		}
		if !separator {
			words.WriteByte(' ')
			separator = true
		}
	}
	normalizedWords := strings.Join(strings.Fields(words.String()), " ")
	tokens := strings.Fields(normalizedWords)
	return matchForms{
		words:   normalizedWords,
		compact: strings.Join(tokens, ""),
		tokens:  tokens,
	}
}

func (forms matchForms) contains(phrase string) bool {
	wanted := newMatchForms(phrase)
	if wanted.words == "" {
		return false
	}
	if containsPhrase(forms.words, wanted.words) {
		return true
	}
	if utf8.RuneCountInString(wanted.compact) >= 5 && strings.Contains(forms.compact, wanted.compact) {
		return true
	}
	return containsLetterSpaced(forms.tokens, []rune(wanted.compact))
}

func containsLetterSpaced(tokens []string, wanted []rune) bool {
	if len(wanted) == 0 || len(tokens) < len(wanted) {
		return false
	}
	for start := 0; start+len(wanted) <= len(tokens); start++ {
		matched := true
		for offset, expected := range wanted {
			token := []rune(tokens[start+offset])
			if len(token) != 1 || token[0] != expected {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// confusableSkeleton intentionally covers the highest-frequency homoglyphs in
// Russian/Kazakh/English attacks. It is for matching only; user-visible text is
// never transliterated through this function.
func confusableSkeleton(r rune) rune {
	switch r {
	case 'а', 'α':
		return 'a'
	case 'б', 'в', 'β':
		return 'b'
	case 'е', 'ё', 'ε':
		return 'e'
	case 'і', 'ι':
		return 'i'
	case 'к', 'κ':
		return 'k'
	case 'м', 'μ':
		return 'm'
	case 'н', 'η':
		return 'h'
	case 'о', 'ο', '0':
		return 'o'
	case 'р', 'ρ':
		return 'p'
	case 'с':
		return 'c'
	case 'т', 'τ':
		return 't'
	case 'у', 'γ':
		return 'y'
	case 'х', 'χ':
		return 'x'
	case 'ѕ':
		return 's'
	default:
		return r
	}
}

func matchTokenSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		forms := newMatchForms(value)
		if len(forms.tokens) == 1 {
			result[forms.tokens[0]] = struct{}{}
		}
	}
	return result
}

func matchesHateTemplate(tokens []string) bool {
	groupIndex, harmIndex, intentIndex := -1, -1, -1
	for index, token := range tokens {
		if _, ok := hateGroupTokens[token]; ok {
			groupIndex = index
		}
		if _, ok := hateHarmTokens[token]; ok {
			harmIndex = index
		}
		if _, ok := hateIntentTokens[token]; ok {
			intentIndex = index
		}
	}
	if groupIndex < 0 || harmIndex < 0 || intentIndex < 0 {
		return false
	}
	return indexDistance(groupIndex, harmIndex) <= 8 && indexDistance(intentIndex, harmIndex) <= 5
}

func indexDistance(left, right int) int {
	if left > right {
		return left - right
	}
	return right - left
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func canonical(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func redactPhoneCandidates(value string) string {
	return phonePattern.ReplaceAllStringFunc(value, func(candidate string) string {
		digits := 0
		for _, r := range candidate {
			if unicode.IsDigit(r) {
				digits++
			}
		}
		if digits < 10 || digits > 15 {
			return candidate
		}
		return "[номер скрыт]"
	})
}

func containsPhrase(value, phrase string) bool {
	start := 0
	for {
		index := strings.Index(value[start:], phrase)
		if index < 0 {
			return false
		}
		index += start
		beforeOK := index == 0 || !isWordRune(runeBefore(value, index))
		afterIndex := index + len(phrase)
		afterOK := afterIndex == len(value) || !isWordRune(runeAt(value, afterIndex))
		if beforeOK && afterOK {
			return true
		}
		start = index + len(phrase)
	}
}

func runeBefore(value string, index int) rune {
	r, _ := utf8.DecodeLastRuneInString(value[:index])
	return r
}

func runeAt(value string, index int) rune {
	r, _ := utf8.DecodeRuneInString(value[index:])
	return r
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func uniqueReasons(reasons []Reason) []Reason {
	if len(reasons) < 2 {
		return reasons
	}
	seen := make(map[Reason]struct{}, len(reasons))
	result := make([]Reason, 0, len(reasons))
	for _, reason := range reasons {
		if _, ok := seen[reason]; ok {
			continue
		}
		seen[reason] = struct{}{}
		result = append(result, reason)
	}
	return result
}
