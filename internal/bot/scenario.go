package bot

import (
	"strings"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

type scenarioDirective struct {
	prefix string
	mode   domain.ScenarioMode
}

var scenarioDirectives = []scenarioDirective{
	{prefix: "придумай смешной комментарий:", mode: domain.ScenarioComment},
	{prefix: "придумай смешной коммент:", mode: domain.ScenarioComment},
	{prefix: "придумай комментарий:", mode: domain.ScenarioComment},
	{prefix: "придумай коммент:", mode: domain.ScenarioComment},
	{prefix: "напиши комментарий:", mode: domain.ScenarioComment},
	{prefix: "напиши коммент:", mode: domain.ScenarioComment},
	{prefix: "комментарий под постом:", mode: domain.ScenarioComment},
	{prefix: "комментарий:", mode: domain.ScenarioComment},
	{prefix: "коммент:", mode: domain.ScenarioComment},
	{prefix: "comment:", mode: domain.ScenarioComment},
	{prefix: "придумай ответ:", mode: domain.ScenarioReply},
	{prefix: "что ответить:", mode: domain.ScenarioReply},
	{prefix: "ответить:", mode: domain.ScenarioReply},
	{prefix: "ответь:", mode: domain.ScenarioReply},
	{prefix: "ответ:", mode: domain.ScenarioReply},
	{prefix: "reply:", mode: domain.ScenarioReply},
}

// extractScenarioDirective recognizes only explicit, leading control phrases.
// Restricting detection to prefixes avoids interpreting quoted phrases inside
// the source as bot commands. For image captions, a directive may consume the
// entire caption because the screenshot remains the source material.
func extractScenarioDirective(value string, allowEmpty bool) (domain.ScenarioMode, string) {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	for _, directive := range scenarioDirectives {
		if !strings.HasPrefix(lower, directive.prefix) {
			continue
		}
		remainder := strings.TrimSpace(trimmed[len(directive.prefix):])
		if remainder != "" || allowEmpty {
			return directive.mode, remainder
		}
	}
	if allowEmpty {
		exact := map[string]domain.ScenarioMode{
			"придумай смешной комментарий":  domain.ScenarioComment,
			"придумай смешной коммент":      domain.ScenarioComment,
			"придумай комментарий":          domain.ScenarioComment,
			"придумай коммент":              domain.ScenarioComment,
			"напиши комментарий":            domain.ScenarioComment,
			"напиши коммент":                domain.ScenarioComment,
			"ответь на последнее сообщение": domain.ScenarioReply,
			"что ответить":                  domain.ScenarioReply,
			"придумай ответ":                domain.ScenarioReply,
			"reply":                         domain.ScenarioReply,
			"comment":                       domain.ScenarioComment,
		}
		if mode, ok := exact[lower]; ok {
			return mode, ""
		}
	}
	return domain.ScenarioAuto, trimmed
}

func sourceHint(message telegram.Message, kind domain.InputKind) string {
	if message.ForwardOrigin != nil {
		switch message.ForwardOrigin.Type {
		case "channel":
			return "forwarded_channel"
		case "chat":
			return "forwarded_chat"
		case "user", "hidden_user":
			return "forwarded_user"
		}
	}
	switch kind {
	case domain.InputImage:
		return "screenshot"
	case domain.InputVoice:
		return "voice"
	default:
		return "plain"
	}
}
