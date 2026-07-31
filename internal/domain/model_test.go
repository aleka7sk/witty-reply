package domain

import (
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
