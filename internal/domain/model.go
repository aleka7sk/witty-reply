package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type InputKind string

const (
	InputText  InputKind = "text"
	InputImage InputKind = "image"
	InputVoice InputKind = "voice"
)

// ScenarioMode describes whose voice the generated text must use. Auto is a
// request-only value: provider results must always resolve it to Reply or
// Comment so the Telegram UI can make the model's interpretation visible.
type ScenarioMode string

const (
	ScenarioAuto    ScenarioMode = "auto"
	ScenarioReply   ScenarioMode = "reply"
	ScenarioComment ScenarioMode = "comment"
)

var ValidScenarioModes = map[ScenarioMode]struct{}{
	ScenarioAuto: {}, ScenarioReply: {}, ScenarioComment: {},
}

func ParseScenarioMode(value string) (ScenarioMode, bool) {
	mode := ScenarioMode(strings.ToLower(strings.TrimSpace(value)))
	_, ok := ValidScenarioModes[mode]
	return mode, ok
}

func (m ScenarioMode) Concrete() bool {
	return m == ScenarioReply || m == ScenarioComment
}

// ModeConfidence deliberately uses a two-value contract. The model should ask
// for clarification only when choosing the wrong scenario would materially
// change the voice and the source does not contain a dominant signal.
type ModeConfidence string

const (
	ModeConfidenceHigh ModeConfidence = "high"
	ModeConfidenceLow  ModeConfidence = "low"
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
	Input           Input
	Tone            Tone
	Mode            ScenarioMode
	Transform       string
	Language        string
	Relationship    string
	SourceHint      string
	StyleExamples   []string
	PreviousReplies []string
	VariantSeed     uint32
}

type GenerationResult struct {
	Mode           ScenarioMode   `json:"mode"`
	ModeConfidence ModeConfidence `json:"mode_confidence"`
	Situation      string         `json:"situation"`
	Replies        []Reply        `json:"replies"`
	Meme           *Meme          `json:"meme,omitempty"`
	Provider       string         `json:"-"`
	Model          string         `json:"-"`
	Usage          Usage          `json:"-"`
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

// MaxThreadPostRunes is the public Threads limit for a text post. The exact
// preview stored in ThreadDraft is the text sent to Threads after approval.
const MaxThreadPostRunes = 500

// MaxThreadMediaBytes is the durable output bound for a normalized image used
// by a first-party Threads post. The original Telegram upload may be larger;
// only the metadata-free JPEG delivered to Meta must fit this eight MiB limit.
const MaxThreadMediaBytes = 8 << 20

type ThreadVoice string

const (
	ThreadVoiceBelcanto ThreadVoice = "belcanto"
	ThreadVoiceAlisher  ThreadVoice = "alisher"
)

func (v ThreadVoice) Valid() bool {
	return v == ThreadVoiceBelcanto || v == ThreadVoiceAlisher
}

// ThreadMediaMode describes the operator-visible format of one exact preview.
// ImagePending is durable so an image upload can be routed correctly after a
// process restart; it is never publishable.
type ThreadMediaMode string

const (
	ThreadMediaText         ThreadMediaMode = "text"
	ThreadMediaImagePending ThreadMediaMode = "image_pending"
	ThreadMediaImage        ThreadMediaMode = "image"
)

func (m ThreadMediaMode) Valid() bool {
	return m == ThreadMediaText || m == ThreadMediaImagePending || m == ThreadMediaImage
}

type ThreadMediaSource string

const (
	ThreadMediaSourceTelegram ThreadMediaSource = "telegram_upload"
	ThreadMediaSourcePexels   ThreadMediaSource = "pexels"
)

func (s ThreadMediaSource) Valid() bool {
	return s == ThreadMediaSourceTelegram || s == ThreadMediaSourcePexels
}

// ThreadMedia is a normalized, metadata-free image attached to one or more
// retained draft revisions. Data is never logged and is removed with the
// owning user or by normal content-retention cleanup.
type ThreadMedia struct {
	ID              int64
	TelegramID      int64
	SourceKind      ThreadMediaSource
	SourceUpdateID  int64
	SourceAssetID   string
	SourcePageURL   string
	SourceAuthor    string
	SourceAuthorURL string
	SourceQuery     string
	Data            []byte
	MediaType       string
	Width           int
	Height          int
	Digest          string
	DeliveryKey     string
	CreatedAt       time.Time
}

func (m ThreadMedia) ValidateForStore() error {
	if m.TelegramID <= 0 {
		return errors.New("thread media owner is required")
	}
	sourceKind := m.SourceKind
	if sourceKind == "" && m.SourceUpdateID > 0 {
		sourceKind = ThreadMediaSourceTelegram
	}
	if !sourceKind.Valid() {
		return errors.New("thread media source kind is invalid")
	}
	switch sourceKind {
	case ThreadMediaSourceTelegram:
		if m.SourceUpdateID <= 0 {
			return errors.New("thread media source update is required")
		}
		if m.SourceAssetID != "" || m.SourcePageURL != "" || m.SourceAuthor != "" || m.SourceAuthorURL != "" || m.SourceQuery != "" {
			return errors.New("telegram media cannot contain licensed source provenance")
		}
	case ThreadMediaSourcePexels:
		if m.SourceUpdateID != 0 {
			return errors.New("licensed media cannot contain a Telegram update")
		}
		if !validThreadMediaField(m.SourceAssetID, 80) || !validThreadMediaField(m.SourceAuthor, 160) || !validThreadMediaField(m.SourceQuery, 120) {
			return errors.New("licensed media provenance is incomplete")
		}
		if !validThreadMediaHTTPSURL(m.SourcePageURL, "www.pexels.com") || !validThreadMediaHTTPSURL(m.SourceAuthorURL, "www.pexels.com") {
			return errors.New("licensed media provenance URL is invalid")
		}
	}
	if len(m.Data) == 0 || len(m.Data) > MaxThreadMediaBytes {
		return errors.New("thread media must contain 1 byte to 8 MiB")
	}
	if m.MediaType != "image/jpeg" {
		return errors.New("thread media must be a normalized JPEG")
	}
	if m.Width < 1 || m.Height < 1 || m.Width > 8_000 || m.Height > 8_000 || int64(m.Width)*int64(m.Height) > 12_000_000 {
		return errors.New("thread media dimensions are invalid")
	}
	longSide, shortSide := max(m.Width, m.Height), min(m.Width, m.Height)
	if m.Width < 320 || m.Width > 1_440 || longSide > shortSide*10 {
		return errors.New("thread media does not satisfy Threads geometry limits")
	}
	if len(m.Digest) != 64 {
		return errors.New("thread media digest is invalid")
	}
	for _, character := range m.Digest {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return errors.New("thread media digest is invalid")
		}
	}
	digest := sha256.Sum256(m.Data)
	if m.Digest != hex.EncodeToString(digest[:]) {
		return errors.New("thread media digest does not match content")
	}
	if len(m.DeliveryKey) != 32 {
		return errors.New("thread media delivery key is invalid")
	}
	for _, character := range m.DeliveryKey {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return errors.New("thread media delivery key is invalid")
		}
	}
	return nil
}

func (m ThreadMedia) EffectiveSourceKind() ThreadMediaSource {
	if m.SourceKind == "" && m.SourceUpdateID > 0 {
		return ThreadMediaSourceTelegram
	}
	return m.SourceKind
}

func validThreadMediaField(value string, maxRunes int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return false
		}
	}
	return true
}

func validThreadMediaHTTPSURL(value, expectedHost string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 2048 {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), expectedHost) || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	return parsed.Port() == "" || parsed.Port() == "443"
}

type ThreadDraftState string

const (
	ThreadDraftReady      ThreadDraftState = "draft"
	ThreadDraftPublishing ThreadDraftState = "publishing"
	ThreadDraftPublished  ThreadDraftState = "published"
	ThreadDraftFailed     ThreadDraftState = "failed"
	ThreadDraftUnknown    ThreadDraftState = "unknown"
	ThreadDraftCancelled  ThreadDraftState = "cancelled"
)

func (s ThreadDraftState) Valid() bool {
	switch s {
	case ThreadDraftReady, ThreadDraftPublishing, ThreadDraftPublished,
		ThreadDraftFailed, ThreadDraftUnknown, ThreadDraftCancelled:
		return true
	default:
		return false
	}
}

// ThreadDraft is the durable, single-preview publication aggregate for the
// Belcanto Threads copilot. Current is an ownership-scoped optimistic pointer:
// creating a replacement makes every older signed keyboard stale without
// deleting its audit and idempotency record.
type ThreadDraft struct {
	ID          int64
	TelegramID  int64
	Voice       ThreadVoice
	Goal        string
	Text        string
	Provider    string
	Model       string
	Revision    uint32
	MediaMode   ThreadMediaMode
	MediaID     int64
	State       ThreadDraftState
	Current     bool
	ContainerID string
	PostID      string
	Permalink   string
	ErrorCode   string
	// ClaimToken fences a single publisher worker. ClaimExpiresAt makes a
	// pre-publish crash recoverable, while PublishStartedAt is the durable
	// boundary after which an automatic retry could create a duplicate.
	ClaimToken       string
	ClaimExpiresAt   *time.Time
	PublishStartedAt *time.Time
	// MediaRightsConfirmedAt is set atomically by the image publish button.
	// It records the operator's explicit confirmation that Belcanto may publish
	// the image and that every identifiable person has consented.
	MediaRightsConfirmedAt *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
	PublishedAt            *time.Time
}

// ValidateForCreate enforces the durable preview boundary without rewriting
// any user-visible text. CreateThreadDraft supplies lifecycle fields itself.
func (d ThreadDraft) ValidateForCreate() error {
	if d.TelegramID <= 0 {
		return errors.New("thread draft owner is required")
	}
	if !d.Voice.Valid() {
		return errors.New("thread draft voice is invalid")
	}
	if !utf8.ValidString(d.Goal) || strings.ContainsRune(d.Goal, '\x00') || strings.TrimSpace(d.Goal) == "" || utf8.RuneCountInString(d.Goal) > 120 {
		return errors.New("thread draft goal must contain 1 to 120 characters")
	}
	if !utf8.ValidString(d.Text) || strings.ContainsRune(d.Text, '\x00') || strings.TrimSpace(d.Text) == "" || utf8.RuneCountInString(d.Text) > MaxThreadPostRunes {
		return errors.New("thread draft text must contain 1 to 500 characters")
	}
	if !utf8.ValidString(d.Provider) || strings.ContainsRune(d.Provider, '\x00') || strings.TrimSpace(d.Provider) == "" || utf8.RuneCountInString(d.Provider) > 100 {
		return errors.New("thread draft provider must contain 1 to 100 characters")
	}
	if !utf8.ValidString(d.Model) || strings.ContainsRune(d.Model, '\x00') || strings.TrimSpace(d.Model) == "" || utf8.RuneCountInString(d.Model) > 160 {
		return errors.New("thread draft model must contain 1 to 160 characters")
	}
	if d.Revision == 0 {
		return errors.New("thread draft revision must be positive")
	}
	mediaMode := d.MediaMode
	if mediaMode == "" {
		mediaMode = ThreadMediaText
	}
	if !mediaMode.Valid() {
		return errors.New("thread draft media mode is invalid")
	}
	if mediaMode == ThreadMediaText && d.MediaID != 0 {
		return errors.New("text thread draft cannot reference media")
	}
	if mediaMode == ThreadMediaImage && d.MediaID <= 0 {
		return errors.New("image thread draft requires media")
	}
	if d.MediaID < 0 {
		return errors.New("thread draft media id is invalid")
	}
	if d.MediaRightsConfirmedAt != nil {
		return errors.New("new thread draft cannot pre-confirm media rights")
	}
	if d.State != "" && d.State != ThreadDraftReady {
		return errors.New("new thread draft state must be draft")
	}
	return nil
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
