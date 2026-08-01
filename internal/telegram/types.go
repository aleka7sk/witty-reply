package telegram

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// Telegram's cloud Bot API currently limits downloads to 20 MiB.
	MaxCloudDownloadBytes int64 = 20 << 20
	// Telegram accepts photo uploads of up to 10 MiB through multipart/form-data.
	MaxCloudPhotoUploadBytes int64 = 10 << 20
)

type UpdateType string

const (
	UpdateMessage               UpdateType = "message"
	UpdateEditedMessage         UpdateType = "edited_message"
	UpdateChannelPost           UpdateType = "channel_post"
	UpdateEditedChannelPost     UpdateType = "edited_channel_post"
	UpdateInlineQuery           UpdateType = "inline_query"
	UpdateChosenInlineResult    UpdateType = "chosen_inline_result"
	UpdateCallbackQuery         UpdateType = "callback_query"
	UpdatePreCheckoutQuery      UpdateType = "pre_checkout_query"
	UpdatePurchasedPaidMedia    UpdateType = "purchased_paid_media"
	UpdateMyChatMember          UpdateType = "my_chat_member"
	UpdateChatMember            UpdateType = "chat_member"
	UpdateChatJoinRequest       UpdateType = "chat_join_request"
	UpdateBusinessConnection    UpdateType = "business_connection"
	UpdateBusinessMessage       UpdateType = "business_message"
	UpdateEditedBusinessMessage UpdateType = "edited_business_message"
)

// ProductAllowedUpdates returns the narrow update set used by the core reply
// experience. Callers can append inline-mode or payment updates when enabled.
func ProductAllowedUpdates() []UpdateType {
	return []UpdateType{UpdateMessage, UpdateCallbackQuery, UpdateMyChatMember}
}

type Update struct {
	UpdateID           int64               `json:"update_id"`
	Message            *Message            `json:"message,omitempty"`
	EditedMessage      *Message            `json:"edited_message,omitempty"`
	InlineQuery        *InlineQuery        `json:"inline_query,omitempty"`
	ChosenInlineResult *ChosenInlineResult `json:"chosen_inline_result,omitempty"`
	CallbackQuery      *CallbackQuery      `json:"callback_query,omitempty"`
	MyChatMember       *ChatMemberUpdated  `json:"my_chat_member,omitempty"`
}

type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

type Message struct {
	MessageID       int64           `json:"message_id"`
	From            *User           `json:"from,omitempty"`
	Date            int64           `json:"date"`
	Chat            Chat            `json:"chat"`
	ForwardOrigin   *MessageOrigin  `json:"forward_origin,omitempty"`
	Text            string          `json:"text,omitempty"`
	Entities        []MessageEntity `json:"entities,omitempty"`
	Photo           []PhotoSize     `json:"photo,omitempty"`
	Document        *Document       `json:"document,omitempty"`
	Voice           *Voice          `json:"voice,omitempty"`
	Caption         string          `json:"caption,omitempty"`
	CaptionEntities []MessageEntity `json:"caption_entities,omitempty"`
	MediaGroupID    string          `json:"media_group_id,omitempty"`
	ReplyToMessage  *Message        `json:"reply_to_message,omitempty"`
}

type MessageEntity struct {
	Type          string `json:"type"`
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	URL           string `json:"url,omitempty"`
	User          *User  `json:"user,omitempty"`
	Language      string `json:"language,omitempty"`
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
}

// LargestPhoto returns the photo size with the largest known area. Telegram
// normally orders photo sizes, but choosing explicitly avoids depending on it.
func (m Message) LargestPhoto() (PhotoSize, bool) {
	if len(m.Photo) == 0 {
		return PhotoSize{}, false
	}
	best := m.Photo[0]
	for _, photo := range m.Photo[1:] {
		if int64(photo.Width)*int64(photo.Height) > int64(best.Width)*int64(best.Height) {
			best = photo
		}
	}
	return best, true
}

type MessageOrigin struct {
	Type            string `json:"type"`
	Date            int64  `json:"date"`
	SenderUser      *User  `json:"sender_user,omitempty"`
	SenderUserName  string `json:"sender_user_name,omitempty"`
	SenderChat      *Chat  `json:"sender_chat,omitempty"`
	Chat            *Chat  `json:"chat,omitempty"`
	MessageID       int64  `json:"message_id,omitempty"`
	AuthorSignature string `json:"author_signature,omitempty"`
}

type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Document struct {
	FileID       string     `json:"file_id"`
	FileUniqueID string     `json:"file_unique_id"`
	Thumbnail    *PhotoSize `json:"thumbnail,omitempty"`
	FileName     string     `json:"file_name,omitempty"`
	MIMEType     string     `json:"mime_type,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
}

type Voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	MIMEType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type CallbackQuery struct {
	ID              string   `json:"id"`
	From            User     `json:"from"`
	Message         *Message `json:"message,omitempty"`
	InlineMessageID string   `json:"inline_message_id,omitempty"`
	ChatInstance    string   `json:"chat_instance"`
	Data            string   `json:"data,omitempty"`
}

type InlineQuery struct {
	ID       string `json:"id"`
	From     User   `json:"from"`
	Query    string `json:"query"`
	Offset   string `json:"offset"`
	ChatType string `json:"chat_type,omitempty"`
}

type ChosenInlineResult struct {
	ResultID        string `json:"result_id"`
	From            User   `json:"from"`
	InlineMessageID string `json:"inline_message_id,omitempty"`
	Query           string `json:"query"`
}

type ChatMemberUpdated struct {
	Chat          Chat       `json:"chat"`
	From          User       `json:"from"`
	Date          int64      `json:"date"`
	OldChatMember ChatMember `json:"old_chat_member"`
	NewChatMember ChatMember `json:"new_chat_member"`
}

type ChatMember struct {
	Status string `json:"status"`
	User   User   `json:"user"`
}

type GetUpdatesParams struct {
	Offset         int64        `json:"offset,omitempty"`
	Limit          int          `json:"limit,omitempty"`
	TimeoutSeconds int          `json:"timeout,omitempty"`
	AllowedUpdates []UpdateType `json:"allowed_updates,omitempty"`
}

func (p GetUpdatesParams) validate() error {
	if p.Limit < 0 || p.Limit > 100 {
		return validation("limit", "must be between 1 and 100 when set")
	}
	if p.TimeoutSeconds < 0 {
		return validation("timeout", "must not be negative")
	}
	return nil
}

type SetWebhookParams struct {
	URL                string       `json:"url"`
	IPAddress          string       `json:"ip_address,omitempty"`
	MaxConnections     int          `json:"max_connections,omitempty"`
	AllowedUpdates     []UpdateType `json:"allowed_updates,omitempty"`
	DropPendingUpdates bool         `json:"drop_pending_updates,omitempty"`
	SecretToken        string       `json:"secret_token,omitempty"`
}

func (p SetWebhookParams) validate() error {
	if strings.TrimSpace(p.URL) == "" {
		return validation("url", "is required")
	}
	if p.MaxConnections < 0 || p.MaxConnections > 100 {
		return validation("max_connections", "must be between 1 and 100 when set")
	}
	if err := validateWebhookSecret(p.SecretToken, true); err != nil {
		return err
	}
	return nil
}

type DeleteWebhookParams struct {
	DropPendingUpdates bool `json:"drop_pending_updates,omitempty"`
}

type WebhookInfo struct {
	URL                          string       `json:"url"`
	HasCustomCertificate         bool         `json:"has_custom_certificate"`
	PendingUpdateCount           int          `json:"pending_update_count"`
	IPAddress                    string       `json:"ip_address,omitempty"`
	LastErrorDate                int64        `json:"last_error_date,omitempty"`
	LastErrorMessage             string       `json:"last_error_message,omitempty"`
	LastSynchronizationErrorDate int64        `json:"last_synchronization_error_date,omitempty"`
	MaxConnections               int          `json:"max_connections,omitempty"`
	AllowedUpdates               []UpdateType `json:"allowed_updates,omitempty"`
}

type SendMessageParams struct {
	ChatID              int64                 `json:"chat_id"`
	Text                string                `json:"text"`
	ParseMode           string                `json:"parse_mode,omitempty"`
	DisableNotification bool                  `json:"disable_notification,omitempty"`
	ProtectContent      bool                  `json:"protect_content,omitempty"`
	ReplyMarkup         *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

func (p SendMessageParams) validate() error {
	if p.ChatID == 0 {
		return validation("chat_id", "is required")
	}
	if strings.TrimSpace(p.Text) == "" {
		return validation("text", "is required")
	}
	if !utf8.ValidString(p.Text) || utf8.RuneCountInString(p.Text) > 4096 {
		return validation("text", "must be valid UTF-8 and no longer than 4096 characters")
	}
	if p.ReplyMarkup != nil {
		return p.ReplyMarkup.Validate()
	}
	return nil
}

// InputFile is either a Telegram file_id/HTTP URL reference or a byte upload.
// Exactly one of Reference and Data must be populated.
type InputFile struct {
	Reference string
	Filename  string
	Data      []byte
}

func FileReference(reference string) InputFile {
	return InputFile{Reference: strings.TrimSpace(reference)}
}

func FileUpload(filename string, data []byte) InputFile {
	return InputFile{Filename: filename, Data: data}
}

func (f InputFile) validate() error {
	hasReference := strings.TrimSpace(f.Reference) != ""
	hasData := len(f.Data) != 0
	if hasReference == hasData {
		return validation("photo", "must contain exactly one reference or byte upload")
	}
	return nil
}

type SendPhotoParams struct {
	ChatID              int64
	Photo               InputFile
	Caption             string
	ParseMode           string
	DisableNotification bool
	ProtectContent      bool
	ReplyMarkup         *InlineKeyboardMarkup
}

func (p SendPhotoParams) validate(maxUploadBytes int64) error {
	if p.ChatID == 0 {
		return validation("chat_id", "is required")
	}
	if err := p.Photo.validate(); err != nil {
		return err
	}
	if int64(len(p.Photo.Data)) > maxUploadBytes {
		return &FileTooLargeError{Size: int64(len(p.Photo.Data)), Limit: maxUploadBytes}
	}
	if !utf8.ValidString(p.Caption) || utf8.RuneCountInString(p.Caption) > 1024 {
		return validation("caption", "must be valid UTF-8 and no longer than 1024 characters")
	}
	if p.ReplyMarkup != nil {
		return p.ReplyMarkup.Validate()
	}
	return nil
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

func (m InlineKeyboardMarkup) Validate() error {
	if len(m.InlineKeyboard) == 0 {
		return validation("reply_markup.inline_keyboard", "must not be empty")
	}
	for rowIndex, row := range m.InlineKeyboard {
		if len(row) == 0 {
			return validation(fmt.Sprintf("reply_markup.inline_keyboard[%d]", rowIndex), "must not be empty")
		}
		for columnIndex, button := range row {
			if err := button.validate(); err != nil {
				return fmt.Errorf("button [%d][%d]: %w", rowIndex, columnIndex, err)
			}
		}
	}
	return nil
}

type InlineKeyboardButton struct {
	Text                         string          `json:"text"`
	URL                          string          `json:"url,omitempty"`
	CallbackData                 string          `json:"callback_data,omitempty"`
	CopyText                     *CopyTextButton `json:"copy_text,omitempty"`
	SwitchInlineQuery            string          `json:"switch_inline_query,omitempty"`
	SwitchInlineQueryCurrentChat string          `json:"switch_inline_query_current_chat,omitempty"`
}

func CallbackButton(label, data string) InlineKeyboardButton {
	return InlineKeyboardButton{Text: label, CallbackData: data}
}

func CopyButton(label, text string) InlineKeyboardButton {
	return InlineKeyboardButton{Text: label, CopyText: &CopyTextButton{Text: text}}
}

func (b InlineKeyboardButton) validate() error {
	if strings.TrimSpace(b.Text) == "" || !utf8.ValidString(b.Text) || utf8.RuneCountInString(b.Text) > 64 {
		return validation("text", "must be valid UTF-8 and contain 1 to 64 characters")
	}
	actions := 0
	if b.URL != "" {
		actions++
	}
	if b.CallbackData != "" {
		actions++
		if !utf8.ValidString(b.CallbackData) || len([]byte(b.CallbackData)) > 64 {
			return validation("callback_data", "must be valid UTF-8 and not exceed 64 bytes")
		}
	}
	if b.CopyText != nil {
		actions++
		if err := b.CopyText.validate(); err != nil {
			return err
		}
	}
	if b.SwitchInlineQuery != "" {
		actions++
	}
	if b.SwitchInlineQueryCurrentChat != "" {
		actions++
	}
	if actions != 1 {
		return validation("action", "exactly one button action is required")
	}
	return nil
}

type CopyTextButton struct {
	Text string `json:"text"`
}

func (b CopyTextButton) validate() error {
	if !utf8.ValidString(b.Text) {
		return validation("copy_text.text", "must be valid UTF-8")
	}
	count := utf8.RuneCountInString(b.Text)
	if count < 1 || count > 256 {
		return validation("copy_text.text", "must contain 1 to 256 characters")
	}
	return nil
}

type ChatAction string

const (
	ChatActionTyping          ChatAction = "typing"
	ChatActionUploadPhoto     ChatAction = "upload_photo"
	ChatActionRecordVideo     ChatAction = "record_video"
	ChatActionUploadVideo     ChatAction = "upload_video"
	ChatActionRecordVoice     ChatAction = "record_voice"
	ChatActionUploadVoice     ChatAction = "upload_voice"
	ChatActionUploadDocument  ChatAction = "upload_document"
	ChatActionChooseSticker   ChatAction = "choose_sticker"
	ChatActionFindLocation    ChatAction = "find_location"
	ChatActionRecordVideoNote ChatAction = "record_video_note"
	ChatActionUploadVideoNote ChatAction = "upload_video_note"
)

var validChatActions = map[ChatAction]struct{}{
	ChatActionTyping: {}, ChatActionUploadPhoto: {}, ChatActionRecordVideo: {},
	ChatActionUploadVideo: {}, ChatActionRecordVoice: {}, ChatActionUploadVoice: {},
	ChatActionUploadDocument: {}, ChatActionChooseSticker: {}, ChatActionFindLocation: {},
	ChatActionRecordVideoNote: {}, ChatActionUploadVideoNote: {},
}

type SendChatActionParams struct {
	ChatID int64      `json:"chat_id"`
	Action ChatAction `json:"action"`
}

func (p SendChatActionParams) validate() error {
	if p.ChatID == 0 {
		return validation("chat_id", "is required")
	}
	if _, ok := validChatActions[p.Action]; !ok {
		return validation("action", "is unsupported")
	}
	return nil
}

type AnswerCallbackQueryParams struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
	URL             string `json:"url,omitempty"`
	CacheTime       int    `json:"cache_time,omitempty"`
}

func (p AnswerCallbackQueryParams) validate() error {
	if strings.TrimSpace(p.CallbackQueryID) == "" {
		return validation("callback_query_id", "is required")
	}
	if !utf8.ValidString(p.Text) || utf8.RuneCountInString(p.Text) > 200 {
		return validation("text", "must be valid UTF-8 and no longer than 200 characters")
	}
	if p.CacheTime < 0 {
		return validation("cache_time", "must not be negative")
	}
	return nil
}

type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

type DownloadedFile struct {
	File        File
	Data        []byte
	ContentType string
}

func validateWebhookSecret(secret string, optional bool) error {
	if secret == "" && optional {
		return nil
	}
	if len(secret) < 1 || len(secret) > 256 {
		return validation("secret_token", "must contain 1 to 256 characters")
	}
	for _, char := range secret {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return validation("secret_token", "contains unsupported characters")
	}
	return nil
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("telegram: invalid %s: %s", e.Field, e.Message)
}

func validation(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}
