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
