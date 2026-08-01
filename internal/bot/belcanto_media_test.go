package bot

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
	threadspub "github.com/aleka7sk/witty-reply/internal/threads"
)

const threadTestMediaBaseURL = "https://media.example.test/threads/"

var (
	errThreadTestDownload = errors.New("test transient Telegram download failure")
	errThreadTestAttach   = errors.New("test transient thread media attach failure")
)

type mediaRouteProvider struct {
	threadText    string
	ordinaryCalls atomic.Int64
	threadCalls   atomic.Int64
}

func (p *mediaRouteProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	p.ordinaryCalls.Add(1)
	return ai.NewFake().Generate(ctx, request)
}

func (p *mediaRouteProvider) GenerateThreadPost(_ context.Context, request ai.ThreadPostRequest) (ai.ThreadPostResult, error) {
	p.threadCalls.Add(1)
	return validTestThreadResultForRequest(request, p.threadText, "media-test", "media-test"), nil
}

type quotaObservingStore struct {
	store.Store
	consumeCalls atomic.Int64
}

func (s *quotaObservingStore) ConsumeQuota(
	ctx context.Context,
	telegramID, reservationID int64,
	category domain.QuotaCategory,
	limit int,
	now time.Time,
) (domain.QuotaDecision, error) {
	s.consumeCalls.Add(1)
	return s.Store.ConsumeQuota(ctx, telegramID, reservationID, category, limit, now)
}

type failOnceThreadAttachStore struct {
	store.Store

	mu       sync.Mutex
	failNext bool
	attempts int
}

func (s *failOnceThreadAttachStore) AttachThreadDraftMedia(
	ctx context.Context,
	draftID, telegramID int64,
	revision uint32,
	mediaValue domain.ThreadMedia,
) (domain.ThreadDraft, error) {
	s.mu.Lock()
	s.attempts++
	if s.failNext {
		s.failNext = false
		s.mu.Unlock()
		return domain.ThreadDraft{}, errThreadTestAttach
	}
	s.mu.Unlock()
	return s.Store.AttachThreadDraftMedia(ctx, draftID, telegramID, revision, mediaValue)
}

func (s *failOnceThreadAttachStore) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

type recordingImagePublisher struct {
	recordingThreadPublisher

	imageMu      sync.Mutex
	imageCalls   int
	imageText    string
	imageURL     string
	imageReplyTo string
}

func (p *recordingImagePublisher) CreateImage(_ context.Context, text, imageURL, replyToID string) (string, error) {
	p.imageMu.Lock()
	defer p.imageMu.Unlock()
	p.imageCalls++
	p.imageText = text
	p.imageURL = imageURL
	p.imageReplyTo = replyToID
	return "image-container-test", nil
}

func (p *recordingImagePublisher) imageSnapshot() (calls int, text, imageURL, replyToID string) {
	p.imageMu.Lock()
	defer p.imageMu.Unlock()
	return p.imageCalls, p.imageText, p.imageURL, p.imageReplyTo
}

type failOnceThreadTelegram struct {
	base *fakeTelegram

	mu               sync.Mutex
	failNextMessage  bool
	failNextPhoto    bool
	failNextDownload bool
	downloadAttempts int
}

func (f *failOnceThreadTelegram) SendMessage(ctx context.Context, params telegram.SendMessageParams) (telegram.Message, error) {
	f.mu.Lock()
	if f.failNextMessage {
		f.failNextMessage = false
		f.mu.Unlock()
		return telegram.Message{}, errors.New("test SendMessage failure")
	}
	f.mu.Unlock()
	return f.base.SendMessage(ctx, params)
}

func (f *failOnceThreadTelegram) SendPhoto(ctx context.Context, params telegram.SendPhotoParams) (telegram.Message, error) {
	f.mu.Lock()
	if f.failNextPhoto {
		f.failNextPhoto = false
		f.mu.Unlock()
		return telegram.Message{}, errors.New("test SendPhoto failure")
	}
	f.mu.Unlock()
	return f.base.SendPhoto(ctx, params)
}

func (f *failOnceThreadTelegram) SendChatAction(ctx context.Context, params telegram.SendChatActionParams) error {
	return f.base.SendChatAction(ctx, params)
}

func (f *failOnceThreadTelegram) AnswerCallbackQuery(ctx context.Context, params telegram.AnswerCallbackQueryParams) error {
	return f.base.AnswerCallbackQuery(ctx, params)
}

func (f *failOnceThreadTelegram) DownloadFileLimit(ctx context.Context, fileID string, limit int64) (telegram.DownloadedFile, error) {
	f.mu.Lock()
	f.downloadAttempts++
	if f.failNextDownload {
		f.failNextDownload = false
		f.mu.Unlock()
		return telegram.DownloadedFile{}, errThreadTestDownload
	}
	f.mu.Unlock()
	return f.base.DownloadFileLimit(ctx, fileID, limit)
}

func (f *failOnceThreadTelegram) failMessage() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextMessage = true
}

func (f *failOnceThreadTelegram) failPhoto() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextPhoto = true
}

func (f *failOnceThreadTelegram) failDownload() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNextDownload = true
}

func (f *failOnceThreadTelegram) downloadAttemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloadAttempts
}

func newBelcantoMediaService(
	t *testing.T,
	publisher threadspub.Publisher,
) (*Service, *fakeTelegram, *store.Memory, *session.CallbackCodec, *mediaRouteProvider, *quotaObservingStore) {
	t.Helper()
	provider := &mediaRouteProvider{threadText: "Иногда голосу нужен не совет, а комната, где его наконец услышат."}
	service, telegramClient, memory, _, codec := newTestService(t, provider)
	service.threadPublisher = publisher
	service.threadMediaURL = func(deliveryKey string) (string, error) {
		return threadTestMediaBaseURL + deliveryKey + ".jpg", nil
	}
	service.belcantoOperators[42] = struct{}{}
	ctx := context.Background()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	observedStore := &quotaObservingStore{Store: memory}
	service.store = observedStore
	return service, telegramClient, memory, codec, provider, observedStore
}

func threadTestImage(t *testing.T, width, height int, fill color.Color) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func threadPhotoUpdate(updateID int64, fileID string, data []byte, mediaGroupID string) telegram.Update {
	user := testUser()
	return telegram.Update{UpdateID: updateID, Message: &telegram.Message{
		MessageID: updateID,
		From:      &user,
		Chat:      telegram.Chat{ID: user.ID, Type: "private"},
		Photo: []telegram.PhotoSize{{
			FileID: fileID, Width: 1_800, Height: 900, FileSize: int64(len(data)),
		}},
		MediaGroupID: mediaGroupID,
	}}
}

func setThreadTestFile(client *fakeTelegram, fileID string, data []byte) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.files[fileID] = append([]byte(nil), data...)
}

func snapshotThreadPhotos(client *fakeTelegram) []telegram.SendPhotoParams {
	client.mu.Lock()
	defer client.mu.Unlock()
	result := make([]telegram.SendPhotoParams, len(client.photos))
	copy(result, client.photos)
	for index := range result {
		result[index].Photo.Data = append([]byte(nil), result[index].Photo.Data...)
	}
	return result
}

func preparePendingThreadImage(
	t *testing.T,
	service *Service,
	telegramClient *fakeTelegram,
	commandUpdateID, callbackUpdateID int64,
) (initialPublish string, pending domain.ThreadDraft) {
	t.Helper()
	ctx := context.Background()
	generateThreadDraftForTest(t, service, service.store, commandUpdateID)
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 {
		t.Fatalf("Threads text preview messages = %+v", messages)
	}
	initialPublish = threadPublishCallback(t, messages)
	useImage := threadButtonCallback(t, messages[0].ReplyMarkup, "🖼 Загрузить своё фото")
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(callbackUpdateID, "use-image", useImage)); err != nil {
		t.Fatal(err)
	}
	payload, err := service.callbacks.DecodeForUser(useImage, 42)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = service.store.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	return initialPublish, pending
}

func attachThreadImage(
	t *testing.T,
	service *Service,
	telegramClient *fakeTelegram,
	updateID int64,
	fileID string,
	data []byte,
) domain.ThreadDraft {
	t.Helper()
	setThreadTestFile(telegramClient, fileID, data)
	if err := service.HandleUpdate(context.Background(), threadPhotoUpdate(updateID, fileID, data, "")); err != nil {
		t.Fatal(err)
	}
	draft, err := service.store.GetCurrentThreadDraft(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func TestThreadTextPreviewOffersSignedImageChoiceAndPersistsPendingState(t *testing.T) {
	service, telegramClient, memory, codec, _, observedStore := newBelcantoMediaService(t, &recordingImagePublisher{})
	ctx := context.Background()

	generateThreadDraftForTest(t, service, service.store, 1)
	messages := telegramClient.snapshotMessages()
	if len(messages) != 1 || !strings.Contains(messages[0].Text, "Формат: 📝 только текст") {
		t.Fatalf("text preview = %+v", messages)
	}
	useImage := threadButtonCallback(t, messages[0].ReplyMarkup, "🖼 Загрузить своё фото")
	payload, err := codec.DecodeForUser(useImage, 42)
	if err != nil {
		t.Fatalf("add-image callback is not valid and owner-bound: %v", err)
	}
	if payload.Action != session.ActionThreadUseImage {
		t.Fatalf("add-image action = %d", payload.Action)
	}
	before, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if before.MediaMode != domain.ThreadMediaText || before.MediaID != 0 || before.MediaRightsConfirmedAt != nil {
		t.Fatalf("initial text draft = %+v", before)
	}

	if err := service.HandleUpdate(ctx, threadCallbackUpdate(2, "choose-image", useImage)); err != nil {
		t.Fatal(err)
	}
	after, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.MediaMode != domain.ThreadMediaImagePending || after.MediaID != 0 ||
		after.Revision != before.Revision+1 || after.State != domain.ThreadDraftReady ||
		after.MediaRightsConfirmedAt != nil {
		t.Fatalf("pending image draft = %+v", after)
	}
	messages = telegramClient.snapshotMessages()
	last := messages[len(messages)-1]
	if !strings.Contains(last.Text, "Пришли одно своё реальное фото Belcanto") || last.ReplyMarkup == nil {
		t.Fatalf("image prompt = %+v", last)
	}
	if got := observedStore.consumeCalls.Load(); got != 0 {
		t.Fatalf("format selection consumed ordinary quota %d times", got)
	}
}

func TestThreadPhotoUploadNormalizesPreviewWithoutOrdinaryAIOrQuota(t *testing.T) {
	publisher := &recordingImagePublisher{}
	service, telegramClient, memory, _, provider, observedStore := newBelcantoMediaService(t, publisher)
	_, pending := preparePendingThreadImage(t, service, telegramClient, 10, 11)
	input := threadTestImage(t, 1_800, 900, color.RGBA{R: 184, G: 58, B: 82, A: 255})

	draft := attachThreadImage(t, service, telegramClient, 12, "first-photo", input)
	if draft.ID != pending.ID || draft.MediaMode != domain.ThreadMediaImage || draft.MediaID <= 0 ||
		draft.Revision != pending.Revision+1 || draft.MediaRightsConfirmedAt != nil {
		t.Fatalf("attached image draft = %+v", draft)
	}
	mediaValue, err := memory.GetThreadMedia(context.Background(), draft.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if mediaValue.MediaType != "image/jpeg" || mediaValue.Width != 1_440 || mediaValue.Height != 720 ||
		bytes.Equal(mediaValue.Data, input) || len(mediaValue.Digest) != 64 || mediaValue.SourceUpdateID != 12 {
		t.Fatalf("normalized media = type:%q dimensions:%dx%d bytes:%d digest:%q update:%d",
			mediaValue.MediaType, mediaValue.Width, mediaValue.Height, len(mediaValue.Data), mediaValue.Digest, mediaValue.SourceUpdateID)
	}
	photos := snapshotThreadPhotos(telegramClient)
	if len(photos) != 1 {
		t.Fatalf("photo previews = %+v", photos)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(photos[0].Photo.Data))
	if err != nil {
		t.Fatalf("preview is not normalized JPEG: %v", err)
	}
	if config.Width != mediaValue.Width || config.Height != mediaValue.Height ||
		!bytes.Equal(photos[0].Photo.Data, mediaValue.Data) || !photos[0].ProtectContent ||
		!strings.Contains(photos[0].Caption, draft.Text) || !strings.Contains(photos[0].Caption, "Формат: 🖼 фото + текст") {
		t.Fatalf("normalized preview = dimensions:%dx%d protect:%v caption:%q",
			config.Width, config.Height, photos[0].ProtectContent, photos[0].Caption)
	}
	if label := threadButtonCallback(t, photos[0].ReplyMarkup, "✅ Права есть — опубликовать"); label == "" {
		t.Fatal("image publish callback is empty")
	}
	if got := telegramClient.downloads.Load(); got != 1 {
		t.Fatalf("Telegram downloads = %d", got)
	}
	if got := provider.threadCalls.Load(); got != 1 {
		t.Fatalf("Threads generation calls = %d", got)
	}
	if got := provider.ordinaryCalls.Load(); got != 0 {
		t.Fatalf("photo upload reached ordinary AI %d times", got)
	}
	if got := observedStore.consumeCalls.Load(); got != 0 {
		t.Fatalf("photo upload consumed ordinary quota %d times", got)
	}
}

func TestThreadImageSelectionMakesOldTextPublishCallbackStale(t *testing.T) {
	publisher := &recordingImagePublisher{}
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, publisher)
	oldPublish, _ := preparePendingThreadImage(t, service, telegramClient, 20, 21)
	draft := attachThreadImage(t, service, telegramClient, 22, "stale-photo", threadTestImage(t, 900, 600, color.RGBA{B: 255, A: 255}))

	if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(23, "stale-text-publish", oldPublish)); err != nil {
		t.Fatal(err)
	}
	imageCalls, _, _, _ := publisher.imageSnapshot()
	textCalls, statusCalls, publishCalls, _ := publisher.snapshot()
	if imageCalls != 0 || textCalls != 0 || statusCalls != 0 || publishCalls != 0 {
		t.Fatalf("stale text callback reached publisher image:%d text:%d status:%d publish:%d",
			imageCalls, textCalls, statusCalls, publishCalls)
	}
	after, err := memory.GetThreadDraft(context.Background(), draft.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.ThreadDraftReady || after.MediaRightsConfirmedAt != nil {
		t.Fatalf("stale callback mutated draft = %+v", after)
	}
	messages := telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "устарела") {
		t.Fatalf("stale response = %q", messages[len(messages)-1].Text)
	}
}

func TestThreadImagePublishUsesExactPreviewAndURLOnceAndConfirmsRightsOnlyOnPublish(t *testing.T) {
	publisher := &recordingImagePublisher{recordingThreadPublisher: recordingThreadPublisher{
		publication: threadspub.Publication{ID: "image-post", Permalink: "https://www.threads.net/@belcanto/post/image"},
	}}
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, publisher)
	_, _ = preparePendingThreadImage(t, service, telegramClient, 30, 31)
	draftBefore := attachThreadImage(t, service, telegramClient, 32, "publish-photo", threadTestImage(t, 1_000, 500, color.White))
	if draftBefore.MediaRightsConfirmedAt != nil || draftBefore.PublishStartedAt != nil {
		t.Fatalf("rights or publish marker set before approval = %+v", draftBefore)
	}
	mediaValue, err := memory.GetThreadMedia(context.Background(), draftBefore.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	photos := snapshotThreadPhotos(telegramClient)
	publishData := threadButtonCallback(t, photos[len(photos)-1].ReplyMarkup, "✅ Права есть — опубликовать")

	if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(33, "publish-image", publishData)); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(34, "publish-image-replay", publishData)); err != nil {
		t.Fatal(err)
	}
	imageCalls, imageText, imageURL, replyToID := publisher.imageSnapshot()
	textCalls, _, publishCalls, _ := publisher.snapshot()
	wantURL := threadTestMediaBaseURL + mediaValue.DeliveryKey + ".jpg"
	if imageCalls != 1 || textCalls != 0 || publishCalls != 1 || imageText != draftBefore.Text ||
		imageURL != wantURL || replyToID != "" {
		t.Fatalf("publisher image calls=%d textCalls=%d publishCalls=%d text=%q url=%q reply=%q",
			imageCalls, textCalls, publishCalls, imageText, imageURL, replyToID)
	}
	draftAfter, err := memory.GetThreadDraft(context.Background(), draftBefore.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if draftAfter.State != domain.ThreadDraftPublished || draftAfter.MediaRightsConfirmedAt == nil ||
		draftAfter.PublishStartedAt == nil || draftAfter.PublishedAt == nil {
		t.Fatalf("published image draft = %+v", draftAfter)
	}
}

func TestThreadImageCanSwitchBackToTextWithoutPublishingOrRetainingOrphan(t *testing.T) {
	publisher := &recordingImagePublisher{}
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, publisher)
	_, _ = preparePendingThreadImage(t, service, telegramClient, 40, 41)
	imageDraft := attachThreadImage(t, service, telegramClient, 42, "text-switch-photo", threadTestImage(t, 800, 400, color.Black))
	photos := snapshotThreadPhotos(telegramClient)
	useText := threadButtonCallback(t, photos[len(photos)-1].ReplyMarkup, "📝 Только текст")

	if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(43, "use-text", useText)); err != nil {
		t.Fatal(err)
	}
	textDraft, err := memory.GetThreadDraft(context.Background(), imageDraft.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if textDraft.MediaMode != domain.ThreadMediaText || textDraft.MediaID != 0 ||
		textDraft.Revision != imageDraft.Revision+1 || textDraft.MediaRightsConfirmedAt != nil ||
		textDraft.State != domain.ThreadDraftReady {
		t.Fatalf("text-only draft = %+v", textDraft)
	}
	if _, err := memory.GetThreadMedia(context.Background(), imageDraft.MediaID, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphaned media still retained: %v", err)
	}
	messages := telegramClient.snapshotMessages()
	last := messages[len(messages)-1]
	if !strings.Contains(last.Text, "Формат: 📝 только текст") {
		t.Fatalf("text-only preview = %q", last.Text)
	}
	if callback := threadButtonCallback(t, last.ReplyMarkup, "✅ Опубликовать"); callback == "" {
		t.Fatal("text-only publish callback is empty")
	}
	imageCalls, _, _, _ := publisher.imageSnapshot()
	textCalls, statusCalls, publishCalls, _ := publisher.snapshot()
	if imageCalls != 0 || textCalls != 0 || statusCalls != 0 || publishCalls != 0 {
		t.Fatalf("format switch reached publisher image:%d text:%d status:%d publish:%d",
			imageCalls, textCalls, statusCalls, publishCalls)
	}
}

func TestThreadImageReplacementKeepsOldUntilNewUploadThenRemovesIt(t *testing.T) {
	service, telegramClient, memory, _, provider, observedStore := newBelcantoMediaService(t, &recordingImagePublisher{})
	_, _ = preparePendingThreadImage(t, service, telegramClient, 50, 51)
	firstDraft := attachThreadImage(t, service, telegramClient, 52, "replacement-first", threadTestImage(t, 900, 450, color.RGBA{R: 255, A: 255}))
	firstMedia, err := memory.GetThreadMedia(context.Background(), firstDraft.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	photos := snapshotThreadPhotos(telegramClient)
	replaceData := threadButtonCallback(t, photos[len(photos)-1].ReplyMarkup, "🖼 Загрузить своё фото")

	if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(53, "replace-photo", replaceData)); err != nil {
		t.Fatal(err)
	}
	pending, err := memory.GetThreadDraft(context.Background(), firstDraft.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if pending.MediaMode != domain.ThreadMediaImagePending || pending.MediaID != firstDraft.MediaID ||
		pending.Revision != firstDraft.Revision+1 || pending.MediaRightsConfirmedAt != nil {
		t.Fatalf("replacement pending draft = %+v", pending)
	}
	if _, err := memory.GetThreadMedia(context.Background(), firstDraft.MediaID, 42); err != nil {
		t.Fatalf("old media removed before replacement succeeded: %v", err)
	}
	messages := telegramClient.snapshotMessages()
	last := messages[len(messages)-1]
	if !strings.Contains(last.Text, "Прежнее останется сохранено") {
		t.Fatalf("replacement prompt = %q", last.Text)
	}
	if callback := threadButtonCallback(t, last.ReplyMarkup, "↩️ Оставить прежнее фото"); callback == "" {
		t.Fatal("keep-old-image callback is empty")
	}

	secondDraft := attachThreadImage(t, service, telegramClient, 54, "replacement-second", threadTestImage(t, 1_200, 600, color.RGBA{G: 255, A: 255}))
	if secondDraft.MediaMode != domain.ThreadMediaImage || secondDraft.MediaID == firstDraft.MediaID ||
		secondDraft.Revision != pending.Revision+1 || secondDraft.MediaRightsConfirmedAt != nil {
		t.Fatalf("replacement image draft = %+v", secondDraft)
	}
	secondMedia, err := memory.GetThreadMedia(context.Background(), secondDraft.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if secondMedia.Digest == firstMedia.Digest || secondMedia.SourceUpdateID != 54 {
		t.Fatalf("replacement media = %+v", secondMedia)
	}
	if _, err := memory.GetThreadMedia(context.Background(), firstDraft.MediaID, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replaced media still retained: %v", err)
	}
	if got := len(snapshotThreadPhotos(telegramClient)); got != 2 {
		t.Fatalf("photo preview count after replacement = %d", got)
	}
	if provider.ordinaryCalls.Load() != 0 || provider.threadCalls.Load() != 1 || observedStore.consumeCalls.Load() != 0 {
		t.Fatalf("replacement crossed generation/quota boundary ordinary=%d threads=%d quota=%d",
			provider.ordinaryCalls.Load(), provider.threadCalls.Load(), observedStore.consumeCalls.Load())
	}
}

func TestThreadImageUploadReplayIsIdempotentAndDoesNotRedownload(t *testing.T) {
	service, telegramClient, memory, _, provider, observedStore := newBelcantoMediaService(t, &recordingImagePublisher{})
	_, _ = preparePendingThreadImage(t, service, telegramClient, 60, 61)
	data := threadTestImage(t, 960, 480, color.Gray{Y: 96})
	update := threadPhotoUpdate(62, "replayed-photo", data, "")
	setThreadTestFile(telegramClient, "replayed-photo", data)

	if err := service.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	before, err := memory.GetCurrentThreadDraft(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	after, err := memory.GetCurrentThreadDraft(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.Revision != before.Revision || after.MediaID != before.MediaID || after.MediaMode != before.MediaMode {
		t.Fatalf("replayed upload mutated draft: before=%+v after=%+v", before, after)
	}
	if got := telegramClient.downloads.Load(); got != 1 {
		t.Fatalf("replayed upload downloads = %d", got)
	}
	photos := snapshotThreadPhotos(telegramClient)
	if len(photos) != 2 || !bytes.Equal(photos[0].Photo.Data, photos[1].Photo.Data) || photos[0].Caption != photos[1].Caption {
		t.Fatalf("replayed previews = %+v", photos)
	}
	if provider.ordinaryCalls.Load() != 0 || provider.threadCalls.Load() != 1 || observedStore.consumeCalls.Load() != 0 {
		t.Fatalf("replay crossed generation/quota boundary ordinary=%d threads=%d quota=%d",
			provider.ordinaryCalls.Load(), provider.threadCalls.Load(), observedStore.consumeCalls.Load())
	}
}

func TestThreadImageDownloadFailurePropagatesAndSameUpdateRetrySucceeds(t *testing.T) {
	service, telegramClient, memory, _, provider, observedStore := newBelcantoMediaService(t, &recordingImagePublisher{})
	flakyTelegram := &failOnceThreadTelegram{base: telegramClient}
	service.telegram = flakyTelegram
	_, pending := preparePendingThreadImage(t, service, telegramClient, 63, 64)
	data := threadTestImage(t, 960, 480, color.RGBA{R: 30, G: 120, B: 180, A: 255})
	update := threadPhotoUpdate(65, "transient-download", data, "")
	setThreadTestFile(telegramClient, "transient-download", data)

	flakyTelegram.failDownload()
	err := service.HandleUpdate(context.Background(), update)
	if !errors.Is(err, errThreadTestDownload) {
		t.Fatalf("transient download error = %v", err)
	}
	afterFailure, getErr := memory.GetThreadDraft(context.Background(), pending.ID, 42)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if afterFailure.MediaMode != domain.ThreadMediaImagePending || afterFailure.MediaID != pending.MediaID ||
		afterFailure.Revision != pending.Revision || afterFailure.MediaRightsConfirmedAt != nil {
		t.Fatalf("download failure mutated pending draft: before=%+v after=%+v", pending, afterFailure)
	}
	if got := len(snapshotThreadPhotos(telegramClient)); got != 0 {
		t.Fatalf("download failure sent %d photo previews", got)
	}
	if got := len(telegramClient.snapshotMessages()); got != 2 {
		t.Fatalf("download failure was acknowledged as user-visible rejection: messages=%d", got)
	}
	if got := telegramClient.downloads.Load(); got != 0 {
		t.Fatalf("base Telegram downloads after injected failure = %d", got)
	}

	if err := service.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	afterRetry, getErr := memory.GetThreadDraft(context.Background(), pending.ID, 42)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if afterRetry.MediaMode != domain.ThreadMediaImage || afterRetry.MediaID <= 0 ||
		afterRetry.Revision != pending.Revision+1 || afterRetry.MediaRightsConfirmedAt != nil {
		t.Fatalf("retried download did not attach image = %+v", afterRetry)
	}
	mediaValue, getErr := memory.GetThreadMedia(context.Background(), afterRetry.MediaID, 42)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if mediaValue.SourceUpdateID != update.UpdateID {
		t.Fatalf("retried media source update = %d", mediaValue.SourceUpdateID)
	}
	if got := flakyTelegram.downloadAttemptCount(); got != 2 {
		t.Fatalf("download attempts = %d", got)
	}
	if got := telegramClient.downloads.Load(); got != 1 {
		t.Fatalf("successful base Telegram downloads = %d", got)
	}
	if got := len(snapshotThreadPhotos(telegramClient)); got != 1 {
		t.Fatalf("photo previews after successful retry = %d", got)
	}
	if provider.ordinaryCalls.Load() != 0 || provider.threadCalls.Load() != 1 || observedStore.consumeCalls.Load() != 0 {
		t.Fatalf("download retry crossed generation/quota boundary ordinary=%d threads=%d quota=%d",
			provider.ordinaryCalls.Load(), provider.threadCalls.Load(), observedStore.consumeCalls.Load())
	}
}

func TestThreadImageAttachFailurePropagatesAndSameUpdateRetrySucceeds(t *testing.T) {
	service, telegramClient, memory, _, provider, observedStore := newBelcantoMediaService(t, &recordingImagePublisher{})
	_, pending := preparePendingThreadImage(t, service, telegramClient, 66, 67)
	failingStore := &failOnceThreadAttachStore{Store: service.store, failNext: true}
	service.store = failingStore
	data := threadTestImage(t, 1_080, 540, color.RGBA{R: 210, G: 150, B: 40, A: 255})
	update := threadPhotoUpdate(68, "transient-attach", data, "")
	setThreadTestFile(telegramClient, "transient-attach", data)

	err := service.HandleUpdate(context.Background(), update)
	if !errors.Is(err, errThreadTestAttach) {
		t.Fatalf("transient attach error = %v", err)
	}
	afterFailure, getErr := memory.GetThreadDraft(context.Background(), pending.ID, 42)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if afterFailure.MediaMode != domain.ThreadMediaImagePending || afterFailure.MediaID != pending.MediaID ||
		afterFailure.Revision != pending.Revision || afterFailure.MediaRightsConfirmedAt != nil {
		t.Fatalf("attach failure mutated pending draft: before=%+v after=%+v", pending, afterFailure)
	}
	if got := len(snapshotThreadPhotos(telegramClient)); got != 0 {
		t.Fatalf("attach failure sent %d photo previews", got)
	}
	if got := len(telegramClient.snapshotMessages()); got != 2 {
		t.Fatalf("attach failure was acknowledged as user-visible rejection: messages=%d", got)
	}
	if got := failingStore.attemptCount(); got != 1 {
		t.Fatalf("attach attempts after failure = %d", got)
	}

	if err := service.HandleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	afterRetry, getErr := memory.GetThreadDraft(context.Background(), pending.ID, 42)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if afterRetry.MediaMode != domain.ThreadMediaImage || afterRetry.MediaID <= 0 ||
		afterRetry.Revision != pending.Revision+1 || afterRetry.MediaRightsConfirmedAt != nil {
		t.Fatalf("retried attach did not persist image = %+v", afterRetry)
	}
	mediaValue, getErr := memory.GetThreadMedia(context.Background(), afterRetry.MediaID, 42)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if mediaValue.SourceUpdateID != update.UpdateID {
		t.Fatalf("retried media source update = %d", mediaValue.SourceUpdateID)
	}
	if got := failingStore.attemptCount(); got != 2 {
		t.Fatalf("attach attempts after retry = %d", got)
	}
	if got := telegramClient.downloads.Load(); got != 2 {
		t.Fatalf("downloads across attach retry = %d", got)
	}
	if got := len(snapshotThreadPhotos(telegramClient)); got != 1 {
		t.Fatalf("photo previews after successful attach retry = %d", got)
	}
	if provider.ordinaryCalls.Load() != 0 || provider.threadCalls.Load() != 1 || observedStore.consumeCalls.Load() != 0 {
		t.Fatalf("attach retry crossed generation/quota boundary ordinary=%d threads=%d quota=%d",
			provider.ordinaryCalls.Load(), provider.threadCalls.Load(), observedStore.consumeCalls.Load())
	}
}

func TestThreadImageUploadRejectsAlbumsAndInvalidImagesWithoutLosingPendingDraft(t *testing.T) {
	tests := []struct {
		name          string
		data          []byte
		mediaGroupID  string
		wantText      string
		wantDownloads int64
	}{
		{
			name: "album", data: threadTestImage(t, 800, 400, color.RGBA{G: 255, B: 255, A: 255}), mediaGroupID: "album-1",
			wantText: "нужен один кадр, не альбом", wantDownloads: 0,
		},
		{
			name: "invalid", data: []byte("this is not an image"),
			wantText: "Не получилось подготовить это фото", wantDownloads: 1,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, telegramClient, memory, _, provider, observedStore := newBelcantoMediaService(t, &recordingImagePublisher{})
			_, pending := preparePendingThreadImage(t, service, telegramClient, int64(70+index*10), int64(71+index*10))
			fileID := "rejected-" + test.name
			setThreadTestFile(telegramClient, fileID, test.data)

			if err := service.HandleUpdate(context.Background(), threadPhotoUpdate(int64(72+index*10), fileID, test.data, test.mediaGroupID)); err != nil {
				t.Fatal(err)
			}
			after, err := memory.GetThreadDraft(context.Background(), pending.ID, 42)
			if err != nil {
				t.Fatal(err)
			}
			if after.MediaMode != domain.ThreadMediaImagePending || after.MediaID != pending.MediaID ||
				after.Revision != pending.Revision || after.MediaRightsConfirmedAt != nil {
				t.Fatalf("rejected upload mutated pending draft: before=%+v after=%+v", pending, after)
			}
			if got := len(snapshotThreadPhotos(telegramClient)); got != 0 {
				t.Fatalf("rejected upload sent %d photo previews", got)
			}
			messages := telegramClient.snapshotMessages()
			if !strings.Contains(messages[len(messages)-1].Text, test.wantText) {
				t.Fatalf("rejection response = %q", messages[len(messages)-1].Text)
			}
			if got := telegramClient.downloads.Load(); got != test.wantDownloads {
				t.Fatalf("downloads = %d, want %d", got, test.wantDownloads)
			}
			if provider.ordinaryCalls.Load() != 0 || provider.threadCalls.Load() != 1 || observedStore.consumeCalls.Load() != 0 {
				t.Fatalf("rejection crossed generation/quota boundary ordinary=%d threads=%d quota=%d",
					provider.ordinaryCalls.Load(), provider.threadCalls.Load(), observedStore.consumeCalls.Load())
			}
		})
	}
}

func TestThreadMediaModeCallbackRetryRedeliversCommittedTransition(t *testing.T) {
	t.Run("pending prompt after SendMessage failure", func(t *testing.T) {
		service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, &recordingImagePublisher{})
		flaky := &failOnceThreadTelegram{base: telegramClient}
		service.telegram = flaky
		ctx := context.Background()
		generateThreadDraftForTest(t, service, service.store, 80)
		messages := telegramClient.snapshotMessages()
		useImage := threadButtonCallback(t, messages[0].ReplyMarkup, "🖼 Загрузить своё фото")
		payload, err := service.callbacks.DecodeForUser(useImage, 42)
		if err != nil {
			t.Fatal(err)
		}

		flaky.failMessage()
		if err := service.HandleUpdate(ctx, threadCallbackUpdate(81, "use-image-fails", useImage)); err == nil {
			t.Fatal("first add-image callback unexpectedly delivered its prompt")
		}
		committed, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
		if err != nil {
			t.Fatal(err)
		}
		if committed.MediaMode != domain.ThreadMediaImagePending || committed.Revision != payload.Revision+1 {
			t.Fatalf("callback transition was not committed before delivery failure: %+v", committed)
		}
		if got := len(telegramClient.snapshotMessages()); got != 1 {
			t.Fatalf("failed prompt was recorded as delivered: messages=%d", got)
		}

		if err := service.HandleUpdate(ctx, threadCallbackUpdate(81, "use-image-retry", useImage)); err != nil {
			t.Fatal(err)
		}
		after, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
		if err != nil {
			t.Fatal(err)
		}
		if after.Revision != committed.Revision || after.MediaMode != committed.MediaMode || after.MediaID != committed.MediaID {
			t.Fatalf("callback retry advanced committed transition again: before=%+v after=%+v", committed, after)
		}
		messages = telegramClient.snapshotMessages()
		if len(messages) != 2 || !strings.Contains(messages[len(messages)-1].Text, "Пришли одно своё реальное фото Belcanto") {
			t.Fatalf("retried image prompt = %+v", messages)
		}
	})

	t.Run("image preview after SendPhoto failure", func(t *testing.T) {
		service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, &recordingImagePublisher{})
		flaky := &failOnceThreadTelegram{base: telegramClient}
		service.telegram = flaky
		_, _ = preparePendingThreadImage(t, service, telegramClient, 82, 83)
		imageDraft := attachThreadImage(t, service, telegramClient, 84, "retry-preview-photo", threadTestImage(t, 900, 450, color.RGBA{R: 90, G: 40, B: 180, A: 255}))
		photos := snapshotThreadPhotos(telegramClient)
		replaceImage := threadButtonCallback(t, photos[len(photos)-1].ReplyMarkup, "🖼 Загрузить своё фото")
		if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(85, "replace-image", replaceImage)); err != nil {
			t.Fatal(err)
		}
		messages := telegramClient.snapshotMessages()
		keepImage := threadButtonCallback(t, messages[len(messages)-1].ReplyMarkup, "↩️ Оставить прежнее фото")
		payload, err := service.callbacks.DecodeForUser(keepImage, 42)
		if err != nil {
			t.Fatal(err)
		}

		flaky.failPhoto()
		if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(86, "keep-image-fails", keepImage)); err == nil {
			t.Fatal("first keep-image callback unexpectedly delivered its preview")
		}
		committed, err := memory.GetThreadDraft(context.Background(), imageDraft.ID, 42)
		if err != nil {
			t.Fatal(err)
		}
		if committed.MediaMode != domain.ThreadMediaImage || committed.MediaID != imageDraft.MediaID ||
			committed.Revision != payload.Revision+1 {
			t.Fatalf("keep-image transition was not committed before delivery failure: %+v", committed)
		}
		if got := len(snapshotThreadPhotos(telegramClient)); got != 1 {
			t.Fatalf("failed image preview was recorded as delivered: photos=%d", got)
		}

		if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(86, "keep-image-retry", keepImage)); err != nil {
			t.Fatal(err)
		}
		after, err := memory.GetThreadDraft(context.Background(), imageDraft.ID, 42)
		if err != nil {
			t.Fatal(err)
		}
		if after.Revision != committed.Revision || after.MediaMode != committed.MediaMode || after.MediaID != committed.MediaID {
			t.Fatalf("keep-image retry advanced committed transition again: before=%+v after=%+v", committed, after)
		}
		photos = snapshotThreadPhotos(telegramClient)
		if len(photos) != 2 || photos[len(photos)-1].Caption != threadDraftText(after) ||
			!bytes.Equal(photos[0].Photo.Data, photos[1].Photo.Data) {
			t.Fatalf("retried image previews = %+v", photos)
		}
	})
}

func TestThreadDisabledPublisherDoesNotClaimImageOrConfirmRights(t *testing.T) {
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, threadspub.NewDisabled())
	_, _ = preparePendingThreadImage(t, service, telegramClient, 90, 91)
	draftBefore := attachThreadImage(t, service, telegramClient, 92, "disabled-photo", threadTestImage(t, 800, 400, color.RGBA{R: 255, B: 255, A: 255}))
	photos := snapshotThreadPhotos(telegramClient)
	publishData := threadButtonCallback(t, photos[len(photos)-1].ReplyMarkup, "✅ Права есть — опубликовать")

	if err := service.HandleUpdate(context.Background(), threadCallbackUpdate(93, "disabled-image-publish", publishData)); err != nil {
		t.Fatal(err)
	}
	draftAfter, err := memory.GetThreadDraft(context.Background(), draftBefore.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if draftAfter.State != domain.ThreadDraftReady || draftAfter.ClaimToken != "" || draftAfter.ClaimExpiresAt != nil ||
		draftAfter.ContainerID != "" || draftAfter.PublishStartedAt != nil || draftAfter.MediaRightsConfirmedAt != nil {
		t.Fatalf("disabled publisher claimed image draft = %+v", draftAfter)
	}
	messages := telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "Threads пока не подключён") {
		t.Fatalf("disabled publisher response = %q", messages[len(messages)-1].Text)
	}
}

var (
	_ ai.Provider               = (*mediaRouteProvider)(nil)
	_ ai.ThreadPostGenerator    = (*mediaRouteProvider)(nil)
	_ store.Store               = (*quotaObservingStore)(nil)
	_ store.Store               = (*failOnceThreadAttachStore)(nil)
	_ TelegramClient            = (*failOnceThreadTelegram)(nil)
	_ threadspub.Publisher      = (*recordingImagePublisher)(nil)
	_ threadspub.ImagePublisher = (*recordingImagePublisher)(nil)
)
