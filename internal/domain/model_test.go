package domain

import (
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
