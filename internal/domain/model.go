package domain

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type InputKind string

const (
	InputText  InputKind = "text"
	InputImage InputKind = "image"
	InputVoice InputKind = "voice"
)

type Tone string

const (
	ToneMix      Tone = "mix"
	ToneSmart    Tone = "smart"
	TonePlayful  Tone = "playful"
	ToneSharp    Tone = "sharp"
	ToneBoundary Tone = "boundary"
	ToneMeme     Tone = "meme"
)

var ValidTones = map[Tone]struct{}{
	ToneMix: {}, ToneSmart: {}, TonePlayful: {}, ToneSharp: {}, ToneBoundary: {}, ToneMeme: {},
}

func ParseTone(value string) (Tone, bool) {
	tone := Tone(strings.ToLower(strings.TrimSpace(value)))
	_, ok := ValidTones[tone]
	return tone, ok
}

type Input struct {
	Kind      InputKind
	Text      string
	Image     []byte
	MediaType string
}

func (i Input) Validate(maxTextRunes, maxImageBytes int) error {
	switch i.Kind {
	case InputText, InputVoice:
		if strings.TrimSpace(i.Text) == "" {
			return errors.New("input text is empty")
		}
	case InputImage:
		if len(i.Image) == 0 {
			return errors.New("image is empty")
		}
		if maxImageBytes > 0 && len(i.Image) > maxImageBytes {
			return errors.New("image is too large")
		}
	default:
		return errors.New("unsupported input kind")
	}
	if maxTextRunes > 0 && utf8.RuneCountInString(i.Text) > maxTextRunes {
		return errors.New("input text is too long")
	}
	return nil
}

type User struct {
	TelegramID  int64
	Language    string
	DefaultTone Tone
	ConsentedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (u User) HasConsent() bool { return u.ConsentedAt != nil }

type Reply struct {
	Tone Tone   `json:"tone"`
	Text string `json:"text"`
	Note string `json:"note,omitempty"`
}

type Meme struct {
	Headline string `json:"headline"`
	Caption  string `json:"caption"`
	Footer   string `json:"footer,omitempty"`
	Mood     string `json:"mood,omitempty"`
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type GenerationRequest struct {
	Input         Input
	Tone          Tone
	Transform     string
	Language      string
	Relationship  string
	StyleExamples []string
}

type GenerationResult struct {
	Situation string  `json:"situation"`
	Replies   []Reply `json:"replies"`
	Meme      *Meme   `json:"meme,omitempty"`
	Provider  string  `json:"-"`
	Model     string  `json:"-"`
	Usage     Usage   `json:"-"`
}

type GenerationRecord struct {
	ID          int64
	TelegramID  int64
	InputKind   InputKind
	InputDigest string
	Tone        Tone
	Provider    string
	Model       string
	Result      GenerationResult
	CreatedAt   time.Time
}

type UserStats struct {
	UsedToday  int
	DailyLimit int
	Total      int64
	Plan       string
}

type QuotaCategory string

const (
	QuotaText       QuotaCategory = "text"
	QuotaMedia      QuotaCategory = "media"
	QuotaMeme       QuotaCategory = "meme"
	QuotaRefinement QuotaCategory = "refinement"
)

type QuotaDecision struct {
	Allowed  bool
	Used     int
	Limit    int
	ResetsAt time.Time
	Plan     string
}

type Feedback struct {
	GenerationID int64
	TelegramID   int64
	Candidate    int
	Rating       int
	Action       string
}

type UpdateJobStatus string

const (
	UpdateJobPending    UpdateJobStatus = "pending"
	UpdateJobProcessing UpdateJobStatus = "processing"
	UpdateJobCompleted  UpdateJobStatus = "completed"
	UpdateJobDead       UpdateJobStatus = "dead"
	UpdateJobSuperseded UpdateJobStatus = "superseded"
)

// UpdateJob is a durable, at-least-once Telegram delivery. Payload is retained
// only while the job can still be processed and is scrubbed on completion or
// after the retry budget is exhausted.
type UpdateJob struct {
	UpdateID     int64
	ActorID      int64
	Payload      []byte
	Supersedable bool
	Superseding  bool
	Status       UpdateJobStatus
	Attempts     int
	AvailableAt  time.Time
	LeaseUntil   time.Time
	LeaseToken   string
	EnqueuedAt   time.Time
}
