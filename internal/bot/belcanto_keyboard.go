package bot

import (
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

func threadDraftKeyboard(codec *session.CallbackCodec, userID int64, draft domain.ThreadDraft) (*telegram.InlineKeyboardMarkup, error) {
	button := func(label string, action session.Action) (telegram.InlineKeyboardButton, error) {
		data, err := encodeCallback(codec, action, userID, draft.ID, draft.Revision, -1)
		if err != nil {
			return telegram.InlineKeyboardButton{}, err
		}
		return telegram.CallbackButton(label, data), nil
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, 5)
	appendRow := func(items ...struct {
		label  string
		action session.Action
	}) error {
		row := make([]telegram.InlineKeyboardButton, 0, len(items))
		for _, item := range items {
			current, err := button(item.label, item.action)
			if err != nil {
				return err
			}
			row = append(row, current)
		}
		rows = append(rows, row)
		return nil
	}
	if err := appendRow(struct {
		label  string
		action session.Action
	}{"✅ Опубликовать", session.ActionThreadPublish}); err != nil {
		return nil, err
	}
	if err := appendRow(
		struct {
			label  string
			action session.Action
		}{"🔄 Другой пост", session.ActionThreadDifferentAngle},
		struct {
			label  string
			action session.Action
		}{"✂️ Короче", session.ActionThreadShorter},
	); err != nil {
		return nil, err
	}
	if err := appendRow(
		struct {
			label  string
			action session.Action
		}{"😂 Остроумнее", session.ActionThreadWittier},
		struct {
			label  string
			action session.Action
		}{"❤️ Теплее", session.ActionThreadWarmer},
	); err != nil {
		return nil, err
	}
	switchLabel, switchAction := "👤 Голос Алишера", session.ActionThreadNewAlisher
	if draft.Voice == domain.ThreadVoiceAlisher {
		switchLabel, switchAction = "🎼 Голос Belcanto", session.ActionThreadNewBelcanto
	}
	if err := appendRow(
		struct {
			label  string
			action session.Action
		}{"🚫 Без продажи", session.ActionThreadNoSell},
		struct {
			label  string
			action session.Action
		}{switchLabel, switchAction},
	); err != nil {
		return nil, err
	}
	if err := appendRow(struct {
		label  string
		action session.Action
	}{"🗑 Отменить", session.ActionThreadCancel}); err != nil {
		return nil, err
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}
