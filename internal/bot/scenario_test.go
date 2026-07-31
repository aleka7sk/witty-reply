package bot

import (
	"testing"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

func TestExtractScenarioDirective(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		allowEmpty bool
		wantMode   domain.ScenarioMode
		wantText   string
	}{
		{name: "comment", input: "Коммент: Многие пациентки не чувствуют шевелений", wantMode: domain.ScenarioComment, wantText: "Многие пациентки не чувствуют шевелений"},
		{name: "reply", input: "ОТВЕТЬ: тебя никто не спрашивал", wantMode: domain.ScenarioReply, wantText: "тебя никто не спрашивал"},
		{name: "image caption", input: "придумай смешной коммент", allowEmpty: true, wantMode: domain.ScenarioComment},
		{name: "quoted phrase is data", input: "Он написал: придумай коммент: и ушёл", wantMode: domain.ScenarioAuto, wantText: "Он написал: придумай коммент: и ушёл"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode, text := extractScenarioDirective(test.input, test.allowEmpty)
			if mode != test.wantMode || text != test.wantText {
				t.Fatalf("extractScenarioDirective() = %q, %q; want %q, %q", mode, text, test.wantMode, test.wantText)
			}
		})
	}
}

func TestSourceHintDoesNotExposeForwardIdentity(t *testing.T) {
	message := telegram.Message{ForwardOrigin: &telegram.MessageOrigin{
		Type: "channel", SenderUserName: "private-name", Chat: &telegram.Chat{ID: -100123, Username: "private-channel"},
	}}
	if got := sourceHint(message, domain.InputText); got != "forwarded_channel" {
		t.Fatalf("sourceHint() = %q", got)
	}
}

func TestForwardedLeadingPrefixRemainsSourceData(t *testing.T) {
	service := &Service{config: Config{Limits: Limits{MaxTextRunes: 6000}}}
	message := telegram.Message{
		Text:          "Комментарий: это часть публикации",
		ForwardOrigin: &telegram.MessageOrigin{Type: "channel"},
	}
	raw, err := service.classifyInput(message)
	if err != nil {
		t.Fatal(err)
	}
	if raw.mode != domain.ScenarioAuto || raw.text != message.Text || raw.sourceHint != "forwarded_channel" {
		t.Fatalf("forwarded source was treated as a command: %+v", raw)
	}
}
