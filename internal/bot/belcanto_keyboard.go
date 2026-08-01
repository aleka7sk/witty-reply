package bot

import (
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

func threadBriefObjectiveKeyboard(
	codec *session.CallbackCodec,
	userID int64,
	brief domain.ThreadBrief,
) (*telegram.InlineKeyboardMarkup, error) {
	button := func(label string, action session.Action) (telegram.InlineKeyboardButton, error) {
		data, err := encodeCallback(codec, action, userID, brief.ID, brief.Revision, -1)
		if err != nil {
			return telegram.InlineKeyboardButton{}, err
		}
		return telegram.CallbackButton(label, data), nil
	}
	definitions := [][]struct {
		label  string
		action session.Action
	}{
		{{"👀 Охват", session.ActionThreadObjectiveReach}, {"💬 Ответы", session.ActionThreadObjectiveReplies}},
		{{"🤝 Доверие", session.ActionThreadObjectiveTrust}, {"🎟 Пробное занятие", session.ActionThreadObjectiveTrial}},
		{{"🎶 Сообщество", session.ActionThreadObjectiveCommunity}},
		{{"🗑 Отменить", session.ActionThreadBriefCancel}},
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, len(definitions))
	for _, definitionRow := range definitions {
		row := make([]telegram.InlineKeyboardButton, 0, len(definitionRow))
		for _, definition := range definitionRow {
			item, err := button(definition.label, definition.action)
			if err != nil {
				return nil, err
			}
			row = append(row, item)
		}
		rows = append(rows, row)
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func threadBriefMaterialKeyboard(
	codec *session.CallbackCodec,
	userID int64,
	brief domain.ThreadBrief,
) (*telegram.InlineKeyboardMarkup, error) {
	button := func(label string, action session.Action) (telegram.InlineKeyboardButton, error) {
		data, err := encodeCallback(codec, action, userID, brief.ID, brief.Revision, -1)
		if err != nil {
			return telegram.InlineKeyboardButton{}, err
		}
		return telegram.CallbackButton(label, data), nil
	}
	changeObjective, err := button("↩️ Сменить цель", session.ActionThreadBriefChangeObjective)
	if err != nil {
		return nil, err
	}
	cancel, err := button("🗑 Отменить", session.ActionThreadBriefCancel)
	if err != nil {
		return nil, err
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, 2)
	// A conversion post needs verified terms or a verified next step. Offering
	// an evergreen shortcut here would only move the operator into a generator
	// error (and tempt the model to invent an offer), so trial fails early in UX.
	if brief.Objective != domain.ThreadObjectiveTrial {
		withoutMaterial, buttonErr := button("✨ Без материала дня", session.ActionThreadMaterialNone)
		if buttonErr != nil {
			return nil, buttonErr
		}
		rows = append(rows, []telegram.InlineKeyboardButton{withoutMaterial})
	}
	rows = append(rows, []telegram.InlineKeyboardButton{changeObjective, cancel})
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func threadBriefRetryKeyboard(
	codec *session.CallbackCodec,
	userID int64,
	brief domain.ThreadBrief,
) (*telegram.InlineKeyboardMarkup, error) {
	retryData, err := encodeCallback(codec, session.ActionThreadBriefRetry, userID, brief.ID, brief.Revision, -1)
	if err != nil {
		return nil, err
	}
	cancelData, err := encodeCallback(codec, session.ActionThreadBriefCancel, userID, brief.ID, brief.Revision, -1)
	if err != nil {
		return nil, err
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{telegram.CallbackButton("🔄 Повторить генерацию", retryData)},
		{telegram.CallbackButton("🗑 Отменить", cancelData)},
	}}, nil
}

func threadDraftKeyboard(
	codec *session.CallbackCodec,
	userID int64,
	draft domain.ThreadDraft,
	pexelsEnabled bool,
	pexelsSelected bool,
) (*telegram.InlineKeyboardMarkup, error) {
	button := func(label string, action session.Action) (telegram.InlineKeyboardButton, error) {
		data, err := encodeCallback(codec, action, userID, draft.ID, draft.Revision, -1)
		if err != nil {
			return telegram.InlineKeyboardButton{}, err
		}
		return telegram.CallbackButton(label, data), nil
	}
	rows := make([][]telegram.InlineKeyboardButton, 0, 8)
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
	if draft.MediaMode == domain.ThreadMediaImagePending {
		if pexelsEnabled {
			label := "📷 Подобрать фото в Pexels"
			if pexelsSelected {
				label = "🔄 Другое фото из Pexels"
			}
			if err := appendRow(struct {
				label  string
				action session.Action
			}{label, session.ActionThreadUsePexels}); err != nil {
				return nil, err
			}
		}
		if draft.MediaID > 0 {
			if err := appendRow(struct {
				label  string
				action session.Action
			}{"↩️ Оставить прежнее фото", session.ActionThreadKeepImage}); err != nil {
				return nil, err
			}
		}
		if err := appendRow(struct {
			label  string
			action session.Action
		}{"📝 Оставить только текст", session.ActionThreadUseText}); err != nil {
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
	publishLabel := "✅ Опубликовать"
	if draft.MediaMode == domain.ThreadMediaImage {
		publishLabel = "✅ Права есть — опубликовать"
	}
	if err := appendRow(struct {
		label  string
		action session.Action
	}{publishLabel, session.ActionThreadPublish}); err != nil {
		return nil, err
	}
	if draft.MediaMode == domain.ThreadMediaImage {
		if pexelsEnabled {
			label := "📷 Подобрать фото в Pexels"
			if pexelsSelected {
				label = "🔄 Другое фото из Pexels"
			}
			if err := appendRow(struct {
				label  string
				action session.Action
			}{label, session.ActionThreadUsePexels}); err != nil {
				return nil, err
			}
		}
		if err := appendRow(
			struct {
				label  string
				action session.Action
			}{"🖼 Загрузить своё фото", session.ActionThreadUseImage},
			struct {
				label  string
				action session.Action
			}{"📝 Только текст", session.ActionThreadUseText},
		); err != nil {
			return nil, err
		}
	} else {
		if pexelsEnabled {
			if err := appendRow(struct {
				label  string
				action session.Action
			}{"📷 Подобрать фото в Pexels", session.ActionThreadUsePexels}); err != nil {
				return nil, err
			}
		}
		if err := appendRow(struct {
			label  string
			action session.Action
		}{"🖼 Загрузить своё фото", session.ActionThreadUseImage}); err != nil {
			return nil, err
		}
	}
	if err := appendRow(
		struct {
			label  string
			action session.Action
		}{"🔄 Другой сценарий", session.ActionThreadDifferentAngle},
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
