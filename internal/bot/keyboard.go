package bot

import (
	"fmt"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

func consentKeyboard(codec *session.CallbackCodec, userID int64, lang language) (*telegram.InlineKeyboardMarkup, error) {
	data, err := encodeCallback(codec, session.ActionConsent, userID, userID, 0, -1)
	if err != nil {
		return nil, err
	}
	label := "✅ Согласен"
	if lang == langKK {
		label = "✅ Келісемін"
	} else if lang == langEN {
		label = "✅ I agree"
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{telegram.CallbackButton(label, data)}}}, nil
}

func resultKeyboard(codec *session.CallbackCodec, userID, generationID int64, revision uint32, replies []domain.Reply, mode domain.ScenarioMode, lang language) (*telegram.InlineKeyboardMarkup, error) {
	if len(replies) != 3 {
		return nil, fmt.Errorf("result keyboard requires exactly three replies")
	}
	keyboard := make([][]telegram.InlineKeyboardButton, 0, 5)
	copyRow := make([]telegram.InlineKeyboardButton, 0, len(replies))
	styleRow := make([]telegram.InlineKeyboardButton, 0, len(replies))
	for index, reply := range replies {
		copyRow = append(copyRow, telegram.CopyButton(fmt.Sprintf("📋 %d", index+1), reply.Text))
		data, err := encodeCallback(codec, session.ActionSaveStyle, userID, generationID, revision, int8(index))
		if err != nil {
			return nil, err
		}
		styleRow = append(styleRow, telegram.CallbackButton(fmt.Sprintf("⭐ %d", index+1), data))
	}
	keyboard = append(keyboard, copyRow)

	feedbackRow := make([]telegram.InlineKeyboardButton, 0, 2)
	for _, item := range []struct {
		label  string
		action session.Action
	}{{"👍", session.ActionFeedbackUp}, {"👎", session.ActionFeedbackDown}} {
		data, err := encodeCallback(codec, item.action, userID, generationID, revision, -1)
		if err != nil {
			return nil, err
		}
		feedbackRow = append(feedbackRow, telegram.CallbackButton(item.label, data))
	}
	keyboard = append(keyboard, feedbackRow)

	refinementRows := [][]struct {
		label  string
		action session.Action
	}{
		{{"😂 Смешнее", session.ActionFunnier}, {"🔥 Жёстче", session.ActionSharper}, {"🌿 Мягче", session.ActionSofter}},
		{{"✂️ Короче", session.ActionShorter}, {"🔄 Ещё", session.ActionMore}, {"🖼 Мем", session.ActionMeme}},
	}
	if mode == domain.ScenarioComment {
		refinementRows = [][]struct {
			label  string
			action session.Action
		}{
			{{"😂 Ещё смешнее", session.ActionFunnier}, {"🧠 Тоньше", session.ActionCommentSubtler}},
			{{"😈 Наглее", session.ActionCommentBolder}, {"🤪 Абсурднее", session.ActionCommentAbsurd}},
			{{"✂️ Короче", session.ActionShorter}, {"🎭 Другой заход", session.ActionCommentDifferentAngle}},
			{{"🔄 Ещё три", session.ActionMore}},
		}
	}
	if lang == langKK {
		refinementRows = [][]struct {
			label  string
			action session.Action
		}{
			{{"😂 Күлкілі", session.ActionFunnier}, {"🔥 Өткір", session.ActionSharper}, {"🌿 Жұмсақ", session.ActionSofter}},
			{{"✂️ Қысқа", session.ActionShorter}, {"🔄 Тағы", session.ActionMore}, {"🖼 Мем", session.ActionMeme}},
		}
		if mode == domain.ScenarioComment {
			refinementRows = [][]struct {
				label  string
				action session.Action
			}{
				{{"😂 Күлкілі", session.ActionFunnier}, {"🧠 Нәзік", session.ActionCommentSubtler}},
				{{"😈 Батыл", session.ActionCommentBolder}, {"🤪 Абсурд", session.ActionCommentAbsurd}},
				{{"✂️ Қысқа", session.ActionShorter}, {"🎭 Басқа тәсіл", session.ActionCommentDifferentAngle}},
				{{"🔄 Тағы үшеу", session.ActionMore}},
			}
		}
	} else if lang == langEN {
		refinementRows = [][]struct {
			label  string
			action session.Action
		}{
			{{"😂 Funnier", session.ActionFunnier}, {"🔥 Sharper", session.ActionSharper}, {"🌿 Softer", session.ActionSofter}},
			{{"✂️ Shorter", session.ActionShorter}, {"🔄 More", session.ActionMore}, {"🖼 Meme", session.ActionMeme}},
		}
		if mode == domain.ScenarioComment {
			refinementRows = [][]struct {
				label  string
				action session.Action
			}{
				{{"😂 Funnier", session.ActionFunnier}, {"🧠 Subtler", session.ActionCommentSubtler}},
				{{"😈 Bolder", session.ActionCommentBolder}, {"🤪 More absurd", session.ActionCommentAbsurd}},
				{{"✂️ Shorter", session.ActionShorter}, {"🎭 New angle", session.ActionCommentDifferentAngle}},
				{{"🔄 Three more", session.ActionMore}},
			}
		}
	}
	for _, row := range refinementRows {
		buttons := make([]telegram.InlineKeyboardButton, 0, len(row))
		for _, item := range row {
			data, err := encodeCallback(codec, item.action, userID, generationID, revision, -1)
			if err != nil {
				return nil, err
			}
			buttons = append(buttons, telegram.CallbackButton(item.label, data))
		}
		keyboard = append(keyboard, buttons)
	}
	if mode != domain.ScenarioComment {
		keyboard = append(keyboard, styleRow)
	}
	switchAction := session.ActionModeComment
	switchLabel := "🔥 Нет, нужен коммент под постом"
	if mode == domain.ScenarioComment {
		switchAction = session.ActionModeReply
		switchLabel = "↩️ Нет, это ответ человеку"
	}
	if lang == langKK {
		if mode == domain.ScenarioComment {
			switchLabel = "↩️ Жоқ, бұл адамға жауап"
		} else {
			switchLabel = "🔥 Жоқ, постқа пікір керек"
		}
	} else if lang == langEN {
		if mode == domain.ScenarioComment {
			switchLabel = "↩️ No, reply to the person"
		} else {
			switchLabel = "🔥 No, comment under the post"
		}
	}
	switchData, err := encodeCallback(codec, switchAction, userID, generationID, revision, -1)
	if err != nil {
		return nil, err
	}
	keyboard = append(keyboard, []telegram.InlineKeyboardButton{telegram.CallbackButton(switchLabel, switchData)})
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: keyboard}, nil
}

func modeChoiceKeyboard(codec *session.CallbackCodec, userID, sourceID int64, revision uint32, lang language) (*telegram.InlineKeyboardMarkup, error) {
	replyLabel, commentLabel := "↩️ Ответить человеку", "🔥 Залететь в комментарии"
	if lang == langKK {
		replyLabel, commentLabel = "↩️ Адамға жауап беру", "🔥 Пікір жазу"
	} else if lang == langEN {
		replyLabel, commentLabel = "↩️ Reply to the person", "🔥 Comment under the post"
	}
	replyData, err := encodeCallback(codec, session.ActionModeReply, userID, sourceID, revision, -1)
	if err != nil {
		return nil, err
	}
	commentData, err := encodeCallback(codec, session.ActionModeComment, userID, sourceID, revision, -1)
	if err != nil {
		return nil, err
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{telegram.CallbackButton(replyLabel, replyData)},
		{telegram.CallbackButton(commentLabel, commentData)},
	}}, nil
}

func styleKeyboard(codec *session.CallbackCodec, userID int64, lang language) (*telegram.InlineKeyboardMarkup, error) {
	type item struct {
		label  string
		action session.Action
	}
	items := []item{{"🧠 Умно", session.ActionToneSmart}, {"😏 С подколом", session.ActionTonePlayful}, {"🧊 Уверенно", session.ActionToneBoundary}}
	resetLabel := "♻️ Сбросить стиль"
	if lang == langKK {
		items = []item{{"🧠 Ақылды", session.ActionToneSmart}, {"😏 Әзілмен", session.ActionTonePlayful}, {"🧊 Нық", session.ActionToneBoundary}}
		resetLabel = "♻️ Стильді өшіру"
	} else if lang == langEN {
		items = []item{{"🧠 Smart", session.ActionToneSmart}, {"😏 Playful", session.ActionTonePlayful}, {"🧊 Firm", session.ActionToneBoundary}}
		resetLabel = "♻️ Reset style"
	}
	row := make([]telegram.InlineKeyboardButton, 0, len(items))
	for _, current := range items {
		data, err := encodeCallback(codec, current.action, userID, userID, 0, -1)
		if err != nil {
			return nil, err
		}
		row = append(row, telegram.CallbackButton(current.label, data))
	}
	reset, err := encodeCallback(codec, session.ActionResetStyle, userID, userID, 0, -1)
	if err != nil {
		return nil, err
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{row, {telegram.CallbackButton(resetLabel, reset)}}}, nil
}

func deleteKeyboard(codec *session.CallbackCodec, userID int64, lang language) (*telegram.InlineKeyboardMarkup, error) {
	confirm, err := encodeCallback(codec, session.ActionConfirmDelete, userID, userID, 0, -1)
	if err != nil {
		return nil, err
	}
	cancel, err := encodeCallback(codec, session.ActionCancelDelete, userID, userID, 0, -1)
	if err != nil {
		return nil, err
	}
	labels := []string{"Удалить", "Отмена"}
	if lang == langKK {
		labels = []string{"Жою", "Бас тарту"}
	} else if lang == langEN {
		labels = []string{"Delete", "Cancel"}
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{telegram.CallbackButton("🗑 "+labels[0], confirm), telegram.CallbackButton(labels[1], cancel)}}}, nil
}

func encodeCallback(codec *session.CallbackCodec, action session.Action, userID, interactionID int64, revision uint32, candidate int8) (string, error) {
	return codec.Encode(session.CallbackPayload{
		Action: action, UserID: userID, InteractionID: interactionID, Revision: revision, Candidate: candidate, IssuedAt: time.Now(),
	})
}
