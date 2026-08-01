package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// MaxThreadMaterialRunes bounds the operator-supplied factual brief used to
// prepare one Belcanto Threads post. The material is durable until the normal
// content-retention cleanup so a process restart never turns a real school
// observation into an invented replacement.
const MaxThreadMaterialRunes = 6_000

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

// ThreadObjective is the operator-selected job of a Belcanto post. Legacy is
// a migration-only value for publication drafts created before durable briefs
// existed; it is never offered as a new Telegram choice.
type ThreadObjective string

const (
	ThreadObjectiveReach     ThreadObjective = "reach"
	ThreadObjectiveReplies   ThreadObjective = "replies"
	ThreadObjectiveTrust     ThreadObjective = "trust"
	ThreadObjectiveTrial     ThreadObjective = "trial"
	ThreadObjectiveCommunity ThreadObjective = "community"
	ThreadObjectiveLegacy    ThreadObjective = "legacy"
)

func ParseThreadObjective(value string) (ThreadObjective, bool) {
	objective := ThreadObjective(strings.ToLower(strings.TrimSpace(value)))
	return objective, objective.Valid()
}

func (o ThreadObjective) Valid() bool {
	switch o {
	case ThreadObjectiveReach, ThreadObjectiveReplies, ThreadObjectiveTrust,
		ThreadObjectiveTrial, ThreadObjectiveCommunity, ThreadObjectiveLegacy:
		return true
	default:
		return false
	}
}

func (o ThreadObjective) Selectable() bool {
	return o.Valid() && o != ThreadObjectiveLegacy
}

type ThreadMaterialKind string

const (
	ThreadMaterialNone ThreadMaterialKind = "none"
	ThreadMaterialText ThreadMaterialKind = "text"
)

func ParseThreadMaterialKind(value string) (ThreadMaterialKind, bool) {
	kind := ThreadMaterialKind(strings.ToLower(strings.TrimSpace(value)))
	return kind, kind.Valid()
}

func (k ThreadMaterialKind) Valid() bool {
	return k == ThreadMaterialNone || k == ThreadMaterialText
}

type ThreadBriefState string

const (
	ThreadBriefAwaitingGoal     ThreadBriefState = "awaiting_goal"
	ThreadBriefAwaitingMaterial ThreadBriefState = "awaiting_material"
	ThreadBriefMaterialReady    ThreadBriefState = "material_ready"
	ThreadBriefCandidatesReady  ThreadBriefState = "candidates_ready"
	ThreadBriefDraftReady       ThreadBriefState = "draft_ready"
	ThreadBriefCancelled        ThreadBriefState = "cancelled"
)

func (s ThreadBriefState) Valid() bool {
	switch s {
	case ThreadBriefAwaitingGoal, ThreadBriefAwaitingMaterial,
		ThreadBriefMaterialReady, ThreadBriefCandidatesReady,
		ThreadBriefDraftReady, ThreadBriefCancelled:
		return true
	default:
		return false
	}
}

// ThreadBrief is the durable pre-publication intake for one Belcanto post.
// It deliberately remains separate from ThreadDraft: a draft is always an
// exact, publication-ready preview, while a brief may still be waiting for an
// operator choice or factual material.
type ThreadBrief struct {
	ID               int64
	TelegramID       int64
	StartUpdateID    int64
	Voice            ThreadVoice
	Objective        ThreadObjective
	MaterialKind     ThreadMaterialKind
	MaterialText     string
	MaterialUpdateID int64
	State            ThreadBriefState
	Revision         uint32
	Current          bool
	ErrorCode        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (b ThreadBrief) ValidateForStart() error {
	if b.TelegramID <= 0 {
		return errors.New("thread brief owner is required")
	}
	if b.StartUpdateID <= 0 {
		return errors.New("thread brief start update is required")
	}
	if !b.Voice.Valid() {
		return errors.New("thread brief voice is invalid")
	}
	if b.Objective != "" || b.MaterialKind != "" || strings.TrimSpace(b.MaterialText) != "" || b.MaterialUpdateID != 0 {
		return errors.New("new thread brief cannot contain completed choices")
	}
	if b.State != "" && b.State != ThreadBriefAwaitingGoal {
		return errors.New("new thread brief must await a goal")
	}
	if b.Revision > 1 {
		return errors.New("new thread brief revision is invalid")
	}
	if strings.TrimSpace(b.ErrorCode) != "" {
		return errors.New("new thread brief cannot contain an error")
	}
	return nil
}

// Validate checks the persisted state machine, including the relationship
// between its state and any operator-supplied material.
func (b ThreadBrief) Validate() error {
	if b.ID <= 0 {
		return errors.New("thread brief id is required")
	}
	if b.TelegramID <= 0 || b.StartUpdateID <= 0 || !b.Voice.Valid() || b.Revision == 0 || !b.State.Valid() {
		return errors.New("thread brief identity or lifecycle is invalid")
	}
	if !validThreadBriefErrorCode(b.ErrorCode) {
		return errors.New("thread brief error code is invalid")
	}
	switch b.State {
	case ThreadBriefAwaitingGoal:
		if b.Objective != "" || b.MaterialKind != "" || b.MaterialText != "" || b.MaterialUpdateID != 0 {
			return errors.New("thread brief awaiting a goal contains later choices")
		}
	case ThreadBriefAwaitingMaterial:
		if !b.Objective.Selectable() || b.MaterialKind != "" || b.MaterialText != "" || b.MaterialUpdateID != 0 {
			return errors.New("thread brief awaiting material is invalid")
		}
	case ThreadBriefMaterialReady, ThreadBriefCandidatesReady, ThreadBriefDraftReady:
		if !b.Objective.Selectable() || !validThreadBriefMaterial(b.MaterialKind, b.MaterialText, b.MaterialUpdateID) {
			return errors.New("thread brief material is invalid")
		}
	case ThreadBriefCancelled:
		if b.Current || !validCancelledThreadBriefChoices(b) {
			return errors.New("cancelled thread brief is invalid")
		}
	}
	return nil
}

func validThreadBriefMaterial(kind ThreadMaterialKind, text string, updateID int64) bool {
	if updateID <= 0 || !kind.Valid() || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') || utf8.RuneCountInString(text) > MaxThreadMaterialRunes {
		return false
	}
	for _, character := range text {
		if (unicode.IsControl(character) && character != '\n' && character != '\t') || unicode.In(character, unicode.Cf) {
			return false
		}
	}
	switch kind {
	case ThreadMaterialNone:
		return text == ""
	case ThreadMaterialText:
		return strings.TrimSpace(text) != ""
	default:
		return false
	}
}

func validCancelledThreadBriefChoices(brief ThreadBrief) bool {
	if brief.Objective == "" {
		return brief.MaterialKind == "" && brief.MaterialText == "" && brief.MaterialUpdateID == 0
	}
	if !brief.Objective.Selectable() {
		return false
	}
	if brief.MaterialKind == "" {
		return brief.MaterialText == "" && brief.MaterialUpdateID == 0
	}
	return validThreadBriefMaterial(brief.MaterialKind, brief.MaterialText, brief.MaterialUpdateID)
}

func validThreadBriefErrorCode(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, '\x00') && utf8.RuneCountInString(value) <= 160
}

// ThreadFinalistSetState is the durable lifecycle of one exact five-post
// editorial choice. A ready set is the only state in which a finalist may be
// selected; selected and cancelled sets are immutable audit records.
type ThreadFinalistSetState string

const (
	ThreadFinalistSetReady     ThreadFinalistSetState = "ready"
	ThreadFinalistSetSelected  ThreadFinalistSetState = "selected"
	ThreadFinalistSetCancelled ThreadFinalistSetState = "cancelled"
)

func (s ThreadFinalistSetState) Valid() bool {
	return s == ThreadFinalistSetReady || s == ThreadFinalistSetSelected || s == ThreadFinalistSetCancelled
}

const threadFinalistCount = 5

const (
	ThreadFinalistVisualTextOnly      = "text_only"
	ThreadFinalistVisualLicensedPhoto = "licensed_photo"
	ThreadFinalistVisualBelcantoPhoto = "belcanto_photo"
)

// ThreadFinalist is an immutable WYSIWYG candidate. Evidence and reviewer
// prose deliberately do not cross this boundary; the durable record contains
// only the bounded publication text and metadata required to make a choice.
type ThreadFinalist struct {
	Position       int
	ReviewerID     string
	Goal           string
	Objective      ThreadObjective
	ScenarioID     string
	Mechanism      string
	MaterialBasis  string
	Text           string
	Recommended    bool
	Selectable     bool
	VisualMode     string
	PhotoSuggested bool
	PhotoQuery     string
}

// ThreadFinalistSet stores one complete generation before any publishable
// ThreadDraft exists. SourceRevision fences the source brief or base draft;
// TargetDraftRevision is the revision assigned to the draft only after an
// operator atomically selects a finalist.
type ThreadFinalistSet struct {
	ID                  int64
	TelegramID          int64
	BriefID             int64
	BaseDraftID         int64
	GenerationID        string
	GenerationUpdateID  int64
	Voice               ThreadVoice
	Objective           ThreadObjective
	Provider            string
	Model               string
	SourceRevision      uint32
	TargetDraftRevision uint32
	PreserveMediaMode   ThreadMediaMode
	PreserveMediaID     int64
	State               ThreadFinalistSetState
	Revision            uint32
	Current             bool
	SelectedPosition    int
	SelectionUpdateID   int64
	SelectedDraftID     int64
	Candidates          []ThreadFinalist
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// ValidateForCreate checks caller-supplied generation output before a store
// supplies the identity and durable lifecycle fields.
func (s ThreadFinalistSet) ValidateForCreate() error {
	if s.ID != 0 || s.TelegramID <= 0 || s.BriefID < 0 || s.BaseDraftID < 0 || (s.BriefID == 0 && s.BaseDraftID == 0) {
		return errors.New("thread finalist set identity is invalid")
	}
	if !validThreadGenerationID(s.GenerationID) || s.GenerationUpdateID <= 0 {
		return errors.New("thread finalist set generation is invalid")
	}
	if !s.Voice.Valid() || !s.Objective.Selectable() {
		return errors.New("thread finalist set editorial metadata is invalid")
	}
	if !validBoundedThreadValue(s.Provider, 100) || !validBoundedThreadValue(s.Model, 160) {
		return errors.New("thread finalist set provider metadata is invalid")
	}
	if s.SourceRevision == 0 || s.SourceRevision == ^uint32(0) || s.TargetDraftRevision == 0 {
		return errors.New("thread finalist set revisions are invalid")
	}
	if s.BaseDraftID == 0 {
		if s.BriefID <= 0 || s.TargetDraftRevision != 1 || s.PreserveMediaMode != "" || s.PreserveMediaID != 0 {
			return errors.New("initial thread finalist set source is invalid")
		}
	} else if s.TargetDraftRevision != s.SourceRevision+1 {
		return errors.New("refinement thread finalist set revisions are invalid")
	}
	if !validThreadFinalistPreservedMedia(s.PreserveMediaMode, s.PreserveMediaID) {
		return errors.New("thread finalist set preserved media is invalid")
	}
	if s.State != "" && s.State != ThreadFinalistSetReady {
		return errors.New("new thread finalist set must be ready")
	}
	if s.Revision > 1 || s.SelectedPosition != -1 || s.SelectionUpdateID != 0 || s.SelectedDraftID != 0 {
		return errors.New("new thread finalist set contains selection state")
	}
	return validateThreadFinalists(s.Candidates, s.Objective)
}

// Validate checks a fully persisted finalist set, including optimistic
// lifecycle fencing and the exact five-candidate invariant.
func (s ThreadFinalistSet) Validate() error {
	if s.ID <= 0 || s.TelegramID <= 0 || s.BriefID < 0 || s.BaseDraftID < 0 || (s.BriefID == 0 && s.BaseDraftID == 0) ||
		!validThreadGenerationID(s.GenerationID) || s.GenerationUpdateID <= 0 || !s.Voice.Valid() ||
		!s.Objective.Selectable() || !validBoundedThreadValue(s.Provider, 100) || !validBoundedThreadValue(s.Model, 160) ||
		s.SourceRevision == 0 || s.SourceRevision == ^uint32(0) || s.TargetDraftRevision == 0 ||
		!validThreadFinalistPreservedMedia(s.PreserveMediaMode, s.PreserveMediaID) ||
		!s.State.Valid() || s.Revision == 0 {
		return errors.New("thread finalist set persisted metadata is invalid")
	}
	if err := validateThreadFinalists(s.Candidates, s.Objective); err != nil {
		return err
	}
	if s.BaseDraftID == 0 {
		if s.BriefID <= 0 || s.TargetDraftRevision != 1 || s.PreserveMediaMode != "" || s.PreserveMediaID != 0 {
			return errors.New("persisted initial thread finalist set source is invalid")
		}
	} else if s.TargetDraftRevision != s.SourceRevision+1 {
		return errors.New("persisted refinement thread finalist set revisions are invalid")
	}
	switch s.State {
	case ThreadFinalistSetReady:
		if !s.Current || s.SelectedPosition != -1 || s.SelectionUpdateID != 0 || s.SelectedDraftID != 0 {
			return errors.New("ready thread finalist set has selection state")
		}
	case ThreadFinalistSetSelected:
		if s.Current || s.SelectedPosition < 0 || s.SelectedPosition >= threadFinalistCount ||
			s.SelectionUpdateID <= 0 || s.SelectedDraftID <= 0 || !s.Candidates[s.SelectedPosition].Selectable {
			return errors.New("selected thread finalist set is invalid")
		}
	case ThreadFinalistSetCancelled:
		if s.Current || s.SelectedPosition != -1 || s.SelectionUpdateID != 0 || s.SelectedDraftID != 0 {
			return errors.New("cancelled thread finalist set has selection state")
		}
	}
	return nil
}

func validateThreadFinalists(candidates []ThreadFinalist, objective ThreadObjective) error {
	if len(candidates) != threadFinalistCount {
		return fmt.Errorf("thread finalist set must contain exactly %d candidates", threadFinalistCount)
	}
	positions := make([]bool, threadFinalistCount)
	reviewerIDs := make(map[string]struct{}, threadFinalistCount)
	scenarios := make(map[string]struct{}, threadFinalistCount)
	mechanisms := make(map[string]struct{}, threadFinalistCount)
	recommended := 0
	for index, candidate := range candidates {
		if candidate.Position != index || candidate.Position < 0 || candidate.Position >= threadFinalistCount || positions[candidate.Position] {
			return errors.New("thread finalist positions are invalid")
		}
		positions[candidate.Position] = true
		if !validBoundedThreadValue(candidate.ReviewerID, 16) {
			return errors.New("thread finalist reviewer ID is invalid")
		}
		if _, duplicate := reviewerIDs[candidate.ReviewerID]; duplicate {
			return errors.New("thread finalist reviewer IDs must be unique")
		}
		reviewerIDs[candidate.ReviewerID] = struct{}{}
		if !validBoundedThreadValue(candidate.Goal, 120) || candidate.Objective != objective || !candidate.Objective.Selectable() ||
			!ValidThreadScenarioID(candidate.ScenarioID) || !validBoundedThreadValue(candidate.Mechanism, 120) {
			return errors.New("thread finalist editorial metadata is invalid")
		}
		if _, duplicate := scenarios[candidate.ScenarioID]; duplicate {
			return errors.New("thread finalist scenarios must be unique")
		}
		scenarios[candidate.ScenarioID] = struct{}{}
		mechanisms[candidate.Mechanism] = struct{}{}
		if candidate.MaterialBasis != "none" && candidate.MaterialBasis != "material" {
			return errors.New("thread finalist material basis is invalid")
		}
		if !validThreadPostText(candidate.Text) {
			return errors.New("thread finalist text is invalid")
		}
		if candidate.PhotoQuery != "" && !validThreadPhotoSearchQuery(candidate.PhotoQuery) {
			return errors.New("thread finalist photo query is invalid")
		}
		switch candidate.VisualMode {
		case ThreadFinalistVisualTextOnly:
			if candidate.PhotoSuggested {
				return errors.New("text-only thread finalist cannot suggest a photo")
			}
		case ThreadFinalistVisualLicensedPhoto, ThreadFinalistVisualBelcantoPhoto:
			if !candidate.PhotoSuggested {
				return errors.New("visual thread finalist must suggest a photo")
			}
		default:
			return errors.New("thread finalist visual mode is invalid")
		}
		if candidate.PhotoSuggested && candidate.PhotoQuery == "" {
			return errors.New("thread finalist suggested photo requires a query")
		}
		if candidate.Recommended {
			recommended++
			if !candidate.Selectable {
				return errors.New("recommended thread finalist must be selectable")
			}
		}
	}
	if recommended != 1 {
		return errors.New("thread finalist set must contain exactly one recommendation")
	}
	if len(mechanisms) < 4 {
		return errors.New("thread finalist set must contain at least four mechanisms")
	}
	return nil
}

func validThreadFinalistPreservedMedia(mode ThreadMediaMode, id int64) bool {
	if mode == "" {
		return id == 0
	}
	if mode == ThreadMediaImagePending {
		return id >= 0
	}
	return mode == ThreadMediaImage && id > 0
}

func validThreadGenerationID(value string) bool {
	return validBoundedThreadValue(value, 64) && !strings.ContainsAny(value, "\r\n\t")
}

func validBoundedThreadValue(value string, limit int) bool {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') ||
		strings.TrimSpace(value) != value || value == "" || utf8.RuneCountInString(value) > limit {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return false
		}
	}
	return true
}

func validThreadPostText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, '\x00') &&
		strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= MaxThreadPostRunes
}

// ValidThreadScenarioID accepts stable machine identifiers. The current
// scenario registry belongs to the editorial package; domain validation keeps
// persisted historical IDs readable as that registry evolves.
func ValidThreadScenarioID(value string) bool {
	if len(value) < 3 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' {
			continue
		}
		return false
	}
	return true
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
	ID             int64
	TelegramID     int64
	SourceKind     ThreadMediaSource
	SourceUpdateID int64
	// AttachUpdateID is the durable Telegram callback operation that manually
	// selected a licensed asset. It is separate from SourceUpdateID, which is
	// reserved for the original bytes of an operator-uploaded Telegram photo.
	AttachUpdateID  int64
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
		if m.AttachUpdateID != 0 {
			return errors.New("telegram media cannot contain a licensed attach operation")
		}
		if m.SourceAssetID != "" || m.SourcePageURL != "" || m.SourceAuthor != "" || m.SourceAuthorURL != "" || m.SourceQuery != "" {
			return errors.New("telegram media cannot contain licensed source provenance")
		}
	case ThreadMediaSourcePexels:
		if m.SourceUpdateID != 0 {
			return errors.New("licensed media cannot contain a Telegram update")
		}
		if m.AttachUpdateID < 0 {
			return errors.New("licensed media attach operation is invalid")
		}
		if !validThreadMediaField(m.SourceAssetID, 80) || !validThreadMediaField(m.SourceAuthor, 160) || !validThreadMediaField(m.SourceQuery, 120) {
			return errors.New("licensed media provenance is incomplete")
		}
		if m.AttachUpdateID > 0 && !validThreadPhotoSearchQuery(m.SourceQuery) {
			return errors.New("manual licensed media search query is invalid")
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
	ID         int64
	TelegramID int64
	Voice      ThreadVoice
	Goal       string
	// BriefID, Objective, ScenarioID, and GenerationID connect an exact
	// preview to the durable editorial decision that produced it. Zero/empty
	// values remain accepted only for pre-v4 legacy callers and are normalized
	// by stores to explicit legacy metadata.
	BriefID            int64
	FinalistSetID      int64
	Objective          ThreadObjective
	ScenarioID         string
	GenerationID       string
	GenerationUpdateID int64
	// PhotoQuery is the editor's anonymous, safe Pexels fallback for this exact
	// text. It is retained even when text-only is the recommended format so the
	// operator can override that recommendation without regenerating the post.
	PhotoQuery  string
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
	if d.BriefID < 0 || d.FinalistSetID < 0 || d.GenerationUpdateID < 0 {
		return errors.New("thread draft editorial reference is invalid")
	}
	if d.Objective != "" && !d.Objective.Valid() {
		return errors.New("thread draft objective is invalid")
	}
	if d.ScenarioID != "" && !ValidThreadScenarioID(d.ScenarioID) {
		return errors.New("thread draft scenario is invalid")
	}
	if !utf8.ValidString(d.GenerationID) || strings.ContainsAny(d.GenerationID, "\x00\r\n\t") || utf8.RuneCountInString(d.GenerationID) > 64 {
		return errors.New("thread draft generation id is invalid")
	}
	if d.BriefID > 0 || d.FinalistSetID > 0 {
		if !d.Objective.Selectable() || d.ScenarioID == "" || d.ScenarioID == "legacy_unspecified" ||
			strings.TrimSpace(d.GenerationID) == "" || d.GenerationUpdateID <= 0 {
			return errors.New("thread draft editorial metadata is incomplete")
		}
	}
	if !utf8.ValidString(d.Goal) || strings.ContainsRune(d.Goal, '\x00') || strings.TrimSpace(d.Goal) == "" || utf8.RuneCountInString(d.Goal) > 120 {
		return errors.New("thread draft goal must contain 1 to 120 characters")
	}
	if d.PhotoQuery != "" && !validThreadPhotoSearchQuery(d.PhotoQuery) {
		return errors.New("thread draft photo query is invalid")
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

func validThreadPhotoSearchQuery(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) < 3 || utf8.RuneCountInString(value) > 100 {
		return false
	}
	letters := 0
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z':
			letters++
		case character == ' ', character == '-':
		default:
			return false
		}
	}
	return letters >= 3 && strings.TrimSpace(value) == value
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
