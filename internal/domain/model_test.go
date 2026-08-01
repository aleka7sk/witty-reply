package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestInputValidate(t *testing.T) {
	tests := []struct {
		name    string
		input   Input
		wantErr bool
	}{
		{name: "text", input: Input{Kind: InputText, Text: "Привет"}},
		{name: "empty text", input: Input{Kind: InputText, Text: "   "}, wantErr: true},
		{name: "image", input: Input{Kind: InputImage, Image: []byte{1}, MediaType: "image/png"}},
		{name: "empty image", input: Input{Kind: InputImage}, wantErr: true},
		{name: "unsupported", input: Input{Kind: "video", Text: "x"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.input.Validate(100, 100)
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestParseTone(t *testing.T) {
	tone, ok := ParseTone(" SHARP ")
	if !ok || tone != ToneSharp {
		t.Fatalf("ParseTone() = %q, %v", tone, ok)
	}
	if _, ok := ParseTone("cruel"); ok {
		t.Fatal("unknown tone was accepted")
	}
}

func TestParseScenarioMode(t *testing.T) {
	mode, ok := ParseScenarioMode(" COMMENT ")
	if !ok || mode != ScenarioComment || !mode.Concrete() {
		t.Fatalf("ParseScenarioMode() = %q, %v", mode, ok)
	}
	if ScenarioAuto.Concrete() {
		t.Fatal("auto mode must not be concrete")
	}
	if _, ok := ParseScenarioMode("ambiguous"); ok {
		t.Fatal("unknown scenario mode was accepted")
	}
}

func TestThreadDraftValidateForCreate(t *testing.T) {
	valid := ThreadDraft{
		TelegramID: 42,
		Voice:      ThreadVoiceBelcanto,
		Goal:       "обсуждение",
		Text:       "Взрослость — это когда любимую песню уже не стесняешься хотя бы включать.",
		Provider:   "fake",
		Model:      "deterministic",
		Revision:   1,
	}
	if err := valid.ValidateForCreate(); err != nil {
		t.Fatalf("valid draft: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ThreadDraft)
	}{
		{name: "owner", mutate: func(d *ThreadDraft) { d.TelegramID = 0 }},
		{name: "voice", mutate: func(d *ThreadDraft) { d.Voice = "invented" }},
		{name: "goal", mutate: func(d *ThreadDraft) { d.Goal = "  " }},
		{name: "photo query punctuation", mutate: func(d *ThreadDraft) { d.PhotoQuery = "piano, room" }},
		{name: "photo query non ASCII", mutate: func(d *ThreadDraft) { d.PhotoQuery = "пустая студия" }},
		{name: "text", mutate: func(d *ThreadDraft) { d.Text = strings.Repeat("я", MaxThreadPostRunes+1) }},
		{name: "provider", mutate: func(d *ThreadDraft) { d.Provider = "" }},
		{name: "model", mutate: func(d *ThreadDraft) { d.Model = "" }},
		{name: "revision", mutate: func(d *ThreadDraft) { d.Revision = 0 }},
		{name: "media mode", mutate: func(d *ThreadDraft) { d.MediaMode = "video" }},
		{name: "image without media", mutate: func(d *ThreadDraft) { d.MediaMode = ThreadMediaImage }},
		{name: "text with media", mutate: func(d *ThreadDraft) { d.MediaMode = ThreadMediaText; d.MediaID = 1 }},
		{name: "state", mutate: func(d *ThreadDraft) { d.State = ThreadDraftPublished }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := valid
			test.mutate(&draft)
			if err := draft.ValidateForCreate(); err == nil {
				t.Fatal("invalid draft was accepted")
			}
		})
	}
}

func TestThreadMediaValidateForStore(t *testing.T) {
	data := []byte("normalized-jpeg")
	digest := sha256.Sum256(data)
	valid := ThreadMedia{
		TelegramID: 42, SourceUpdateID: 100, Data: data, MediaType: "image/jpeg",
		Width: 1_000, Height: 800, Digest: hex.EncodeToString(digest[:]),
		DeliveryKey: "0123456789abcdef0123456789abcdef",
	}
	if err := valid.ValidateForStore(); err != nil {
		t.Fatalf("valid media: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ThreadMedia)
	}{
		{name: "source", mutate: func(value *ThreadMedia) { value.SourceUpdateID = 0 }},
		{name: "size", mutate: func(value *ThreadMedia) { value.Data = make([]byte, MaxThreadMediaBytes+1) }},
		{name: "type", mutate: func(value *ThreadMedia) { value.MediaType = "image/png" }},
		{name: "width", mutate: func(value *ThreadMedia) { value.Width = 200 }},
		{name: "ratio", mutate: func(value *ThreadMedia) { value.Width, value.Height = 1_440, 100 }},
		{name: "digest", mutate: func(value *ThreadMedia) { value.Digest = strings.Repeat("0", 64) }},
		{name: "delivery", mutate: func(value *ThreadMedia) { value.DeliveryKey = "short" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := valid
			test.mutate(&value)
			if err := value.ValidateForStore(); err == nil {
				t.Fatal("invalid media was accepted")
			}
		})
	}
}

func TestThreadMediaValidateForStoreAcceptsPexelsProvenance(t *testing.T) {
	data := []byte("normalized-licensed-jpeg")
	digest := sha256.Sum256(data)
	valid := ThreadMedia{
		TelegramID: 42, SourceKind: ThreadMediaSourcePexels,
		SourceAssetID: "12345", SourcePageURL: "https://www.pexels.com/photo/microphone-12345/",
		SourceAuthor: "Lens Author", SourceAuthorURL: "https://www.pexels.com/@lens-author",
		SourceQuery: "vintage microphone close up",
		Data:        data, MediaType: "image/jpeg", Width: 800, Height: 1_000,
		Digest: hex.EncodeToString(digest[:]), DeliveryKey: "abcdef0123456789abcdef0123456789",
	}
	if err := valid.ValidateForStore(); err != nil {
		t.Fatalf("valid Pexels media: %v", err)
	}
	legacy := valid
	legacy.SourceQuery = strings.Repeat("a", 110)
	if err := legacy.ValidateForStore(); err != nil {
		t.Fatalf("legacy automatic Pexels query must remain readable after upgrade: %v", err)
	}
	manual := valid
	manual.AttachUpdateID = 77
	if err := manual.ValidateForStore(); err != nil {
		t.Fatalf("valid manual Pexels media: %v", err)
	}
	manual.SourceQuery = strings.Repeat("a", 101)
	if err := manual.ValidateForStore(); err == nil {
		t.Fatal("oversized manual Pexels query was accepted")
	}
	for name, mutate := range map[string]func(*ThreadMedia){
		"Telegram update": func(value *ThreadMedia) { value.SourceUpdateID = 9 },
		"missing author":  func(value *ThreadMedia) { value.SourceAuthor = "" },
		"author control":  func(value *ThreadMedia) { value.SourceAuthor = "Lens\nInjected" },
		"author bidi":     func(value *ThreadMedia) { value.SourceAuthor = "Lens\u202eAuthor" },
		"wrong host":      func(value *ThreadMedia) { value.SourcePageURL = "https://pexels.example/photo" },
		"credentials":     func(value *ThreadMedia) { value.SourceAuthorURL = "https://user:pass@www.pexels.com/@lens" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.ValidateForStore(); err == nil {
				t.Fatal("invalid Pexels provenance was accepted")
			}
		})
	}
}

func TestThreadVoiceAndDraftStateValidity(t *testing.T) {
	if !ThreadVoiceBelcanto.Valid() || !ThreadVoiceAlisher.Valid() || ThreadVoice("other").Valid() {
		t.Fatal("thread voice validity contract is wrong")
	}
	for _, state := range []ThreadDraftState{
		ThreadDraftReady, ThreadDraftPublishing, ThreadDraftPublished,
		ThreadDraftFailed, ThreadDraftUnknown, ThreadDraftCancelled,
	} {
		if !state.Valid() {
			t.Fatalf("state %q is not valid", state)
		}
	}
	if ThreadDraftState("other").Valid() {
		t.Fatal("unknown draft state was accepted")
	}
}

func TestThreadEditorialMetadataValidation(t *testing.T) {
	for _, objective := range []ThreadObjective{
		ThreadObjectiveReach, ThreadObjectiveReplies, ThreadObjectiveTrust,
		ThreadObjectiveTrial, ThreadObjectiveCommunity,
	} {
		if !objective.Valid() || !objective.Selectable() {
			t.Fatalf("objective %q is not selectable", objective)
		}
		parsed, ok := ParseThreadObjective(" " + strings.ToUpper(string(objective)) + " ")
		if !ok || parsed != objective {
			t.Fatalf("ParseThreadObjective(%q) = %q, %v", objective, parsed, ok)
		}
	}
	if !ThreadObjectiveLegacy.Valid() || ThreadObjectiveLegacy.Selectable() {
		t.Fatal("legacy objective must be valid but not selectable")
	}
	if _, ok := ParseThreadObjective("sales"); ok {
		t.Fatal("unknown objective was accepted")
	}
	for _, kind := range []ThreadMaterialKind{ThreadMaterialNone, ThreadMaterialText} {
		parsed, ok := ParseThreadMaterialKind(" " + strings.ToUpper(string(kind)) + " ")
		if !ok || parsed != kind || !kind.Valid() {
			t.Fatalf("material kind %q is invalid", kind)
		}
	}
	for _, scenarioID := range []string{"karaoke_archetype", "teacher_one_move", "scenario2"} {
		if !ValidThreadScenarioID(scenarioID) {
			t.Fatalf("scenario ID %q was rejected", scenarioID)
		}
	}
	for _, scenarioID := range []string{"", "A_bad", "has-dash", "кириллица", strings.Repeat("a", 65)} {
		if ValidThreadScenarioID(scenarioID) {
			t.Fatalf("invalid scenario ID %q was accepted", scenarioID)
		}
	}
}

func TestThreadBriefValidationFollowsDurableStateMachine(t *testing.T) {
	start := ThreadBrief{TelegramID: 42, StartUpdateID: 100, Voice: ThreadVoiceBelcanto}
	if err := start.ValidateForStart(); err != nil {
		t.Fatalf("ValidateForStart(): %v", err)
	}
	base := ThreadBrief{
		ID: 1, TelegramID: 42, StartUpdateID: 100, Voice: ThreadVoiceBelcanto,
		State: ThreadBriefAwaitingGoal, Revision: 1, Current: true,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("awaiting goal: %v", err)
	}
	awaitingMaterial := base
	awaitingMaterial.Objective = ThreadObjectiveReplies
	awaitingMaterial.State = ThreadBriefAwaitingMaterial
	awaitingMaterial.Revision = 2
	if err := awaitingMaterial.Validate(); err != nil {
		t.Fatalf("awaiting material: %v", err)
	}
	withText := awaitingMaterial
	withText.MaterialKind = ThreadMaterialText
	withText.MaterialText = "После работы ученица сначала боялась услышать запись своего голоса."
	withText.MaterialUpdateID = 101
	withText.State = ThreadBriefMaterialReady
	withText.Revision = 3
	if err := withText.Validate(); err != nil {
		t.Fatalf("material ready: %v", err)
	}
	withoutMaterial := awaitingMaterial
	withoutMaterial.MaterialKind = ThreadMaterialNone
	withoutMaterial.MaterialUpdateID = 102
	withoutMaterial.State = ThreadBriefMaterialReady
	withoutMaterial.Revision = 3
	if err := withoutMaterial.Validate(); err != nil {
		t.Fatalf("no material: %v", err)
	}
	cancelled := withText
	cancelled.State = ThreadBriefCancelled
	cancelled.Current = false
	cancelled.Revision++
	if err := cancelled.Validate(); err != nil {
		t.Fatalf("cancelled: %v", err)
	}

	invalid := []ThreadBrief{
		func() ThreadBrief { value := base; value.Objective = ThreadObjectiveReach; return value }(),
		func() ThreadBrief { value := awaitingMaterial; value.MaterialKind = ThreadMaterialText; return value }(),
		func() ThreadBrief { value := withText; value.MaterialUpdateID = 0; return value }(),
		func() ThreadBrief { value := withoutMaterial; value.MaterialText = "invented"; return value }(),
		func() ThreadBrief { value := cancelled; value.Current = true; return value }(),
	}
	for index, value := range invalid {
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid brief %d was accepted: %+v", index, value)
		}
	}
}

func TestThreadDraftRequiresCompleteMetadataWhenLinkedToBrief(t *testing.T) {
	draft := ThreadDraft{
		TelegramID: 42, Voice: ThreadVoiceBelcanto, Goal: "discussion",
		BriefID: 9, Objective: ThreadObjectiveReplies, ScenarioID: "karaoke_archetype",
		GenerationID: "generation-1", GenerationUpdateID: 101,
		Text:     "Кто вы в караоке: тот, кто выбирает песню, или тот, кто внезапно забирает второй микрофон?",
		Provider: "fake", Model: "deterministic", Revision: 1,
	}
	if err := draft.ValidateForCreate(); err != nil {
		t.Fatalf("linked draft: %v", err)
	}
	for name, mutate := range map[string]func(*ThreadDraft){
		"objective":         func(value *ThreadDraft) { value.Objective = ThreadObjectiveLegacy },
		"scenario":          func(value *ThreadDraft) { value.ScenarioID = "" },
		"generation":        func(value *ThreadDraft) { value.GenerationID = "" },
		"generation update": func(value *ThreadDraft) { value.GenerationUpdateID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := draft
			mutate(&candidate)
			if err := candidate.ValidateForCreate(); err == nil {
				t.Fatal("incomplete linked draft was accepted")
			}
		})
	}
}

func TestThreadFinalistSetValidationRequiresCanonicalFiveAndOneSafeRecommendation(t *testing.T) {
	valid := ThreadFinalistSet{
		TelegramID: 42, BriefID: 9, GenerationID: "generation-finalists-1", GenerationUpdateID: 101,
		Voice: ThreadVoiceBelcanto, Objective: ThreadObjectiveReplies,
		Provider: "fake", Model: "deterministic", SourceRevision: 3, TargetDraftRevision: 1,
		SelectedPosition: -1,
		Candidates: []ThreadFinalist{
			{Position: 0, ReviewerID: "A", Goal: "ответы", Objective: ThreadObjectiveReplies, ScenarioID: "karaoke_choice", Mechanism: "question", MaterialBasis: "material", Text: "Какую песню вы первой выберете в караоке?", Recommended: true, Selectable: true, VisualMode: ThreadFinalistVisualLicensedPhoto, PhotoSuggested: true, PhotoQuery: "vintage microphone close up"},
			{Position: 1, ReviewerID: "B", Goal: "ответы", Objective: ThreadObjectiveReplies, ScenarioID: "song_memory", Mechanism: "memory", MaterialBasis: "material", Text: "Какую песню вы помните не по словам, а по голосу близкого человека?", Selectable: true, VisualMode: ThreadFinalistVisualTextOnly},
			{Position: 2, ReviewerID: "C", Goal: "ответы", Objective: ThreadObjectiveReplies, ScenarioID: "astana_playlist", Mechanism: "local", MaterialBasis: "material", Text: "Какая песня лучше всего звучит во время вечерней поездки по Астане?", Selectable: true, VisualMode: ThreadFinalistVisualTextOnly},
			{Position: 3, ReviewerID: "D", Goal: "ответы", Objective: ThreadObjectiveReplies, ScenarioID: "small_group", Mechanism: "trust", MaterialBasis: "material", Text: "Что спокойнее для первого занятия: один на один или маленькая группа?", Selectable: true, VisualMode: ThreadFinalistVisualTextOnly},
			{Position: 4, ReviewerID: "E", Goal: "ответы", Objective: ThreadObjectiveReplies, ScenarioID: "community_week", Mechanism: "community", MaterialBasis: "material", Text: "К чему вы бы присоединились сначала: караоке, йога или актёрское занятие?", Selectable: false, VisualMode: ThreadFinalistVisualTextOnly},
		},
	}
	if err := valid.ValidateForCreate(); err != nil {
		t.Fatalf("valid finalist set: %v", err)
	}
	for name, mutate := range map[string]func(*ThreadFinalistSet){
		"four candidates": func(value *ThreadFinalistSet) { value.Candidates = value.Candidates[:4] },
		"noncanonical": func(value *ThreadFinalistSet) {
			value.Candidates[0], value.Candidates[1] = value.Candidates[1], value.Candidates[0]
		},
		"two recommendations":   func(value *ThreadFinalistSet) { value.Candidates[1].Recommended = true },
		"unsafe recommendation": func(value *ThreadFinalistSet) { value.Candidates[0].Selectable = false },
		"three mechanisms": func(value *ThreadFinalistSet) {
			value.Candidates[3].Mechanism = "question"
			value.Candidates[4].Mechanism = "memory"
		},
		"visual mismatch":  func(value *ThreadFinalistSet) { value.Candidates[0].PhotoSuggested = false },
		"initial revision": func(value *ThreadFinalistSet) { value.TargetDraftRevision = 2 },
		"source overflow":  func(value *ThreadFinalistSet) { value.SourceRevision = ^uint32(0) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Candidates = append([]ThreadFinalist(nil), valid.Candidates...)
			mutate(&candidate)
			if err := candidate.ValidateForCreate(); err == nil {
				t.Fatal("invalid finalist set was accepted")
			}
		})
	}
	refinement := valid
	refinement.BriefID = 0
	refinement.BaseDraftID = 17
	refinement.SourceRevision = 4
	refinement.TargetDraftRevision = 5
	refinement.PreserveMediaMode = ThreadMediaImagePending
	if err := refinement.ValidateForCreate(); err != nil {
		t.Fatalf("pending-media refinement: %v", err)
	}
}
