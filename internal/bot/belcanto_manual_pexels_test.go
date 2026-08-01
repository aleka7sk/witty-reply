package bot

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/photos"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

type rotatingLicensedPhotoSource struct {
	mu         sync.Mutex
	assets     []photos.Asset
	err        error
	calls      int
	queries    []string
	exclusions [][]string
}

type synchronizedLicensedPhotoSource struct {
	assets  []photos.Asset
	arrived atomic.Int32
	release chan struct{}
}

func (source *synchronizedLicensedPhotoSource) Find(_ context.Context, _ string) (photos.Asset, error) {
	index := int(source.arrived.Add(1) - 1)
	if index == len(source.assets)-1 {
		close(source.release)
	}
	<-source.release
	if index < 0 || index >= len(source.assets) {
		return photos.Asset{}, photos.ErrNotFound
	}
	return source.assets[index], nil
}

func (source *rotatingLicensedPhotoSource) Find(_ context.Context, query string) (photos.Asset, error) {
	return source.find(query, nil)
}

func (source *rotatingLicensedPhotoSource) FindAlternative(
	_ context.Context,
	query string,
	excludedAssetIDs ...string,
) (photos.Asset, error) {
	return source.find(query, excludedAssetIDs)
}

func (source *rotatingLicensedPhotoSource) find(query string, excluded []string) (photos.Asset, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	source.queries = append(source.queries, query)
	source.exclusions = append(source.exclusions, append([]string(nil), excluded...))
	if source.err != nil {
		return photos.Asset{}, source.err
	}
	for _, asset := range source.assets {
		if !slices.Contains(excluded, asset.AssetID) {
			return asset, nil
		}
	}
	return photos.Asset{}, photos.ErrNotFound
}

func (source *rotatingLicensedPhotoSource) snapshot() (int, []string, [][]string) {
	source.mu.Lock()
	defer source.mu.Unlock()
	exclusions := make([][]string, len(source.exclusions))
	for index := range source.exclusions {
		exclusions[index] = append([]string(nil), source.exclusions[index]...)
	}
	return source.calls, append([]string(nil), source.queries...), exclusions
}

func manualPexelsAsset(t *testing.T, id, author string, fill color.Color) photos.Asset {
	t.Helper()
	return photos.Asset{
		Provider: "pexels", AssetID: id,
		PageURL: "https://www.pexels.com/photo/object-" + id + "/",
		Author:  author, AuthorURL: "https://www.pexels.com/@author-" + id,
		Query: fallbackLicensedPhotoQuery, Alt: "Empty music studio object",
		Data: threadTestImage(t, 800, 1_000, fill),
	}
}

func TestThreadTextDraftCanManuallySelectAndReplacePexelsPhoto(t *testing.T) {
	service, telegramClient, memory, _, provider, observedStore := newBelcantoMediaService(t, &recordingImagePublisher{})
	source := &rotatingLicensedPhotoSource{assets: []photos.Asset{
		manualPexelsAsset(t, "101", "First Lens", color.RGBA{R: 30, G: 70, B: 120, A: 255}),
		manualPexelsAsset(t, "202", "Second Lens", color.RGBA{R: 130, G: 50, B: 80, A: 255}),
	}}
	service.threadPhotoSource = source
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "full"
	ctx := context.Background()

	if err := service.HandleUpdate(ctx, textUpdate(100, "/threads")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	selectPexels := threadButtonCallback(t, messages[0].ReplyMarkup, "📷 Подобрать фото в Pexels")
	payload, err := service.callbacks.DecodeForUser(selectPexels, 42)
	if err != nil {
		t.Fatal(err)
	}
	before, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if before.MediaMode != domain.ThreadMediaText || before.PhotoQuery != fallbackLicensedPhotoQuery {
		t.Fatalf("initial draft = %+v", before)
	}

	if err := service.HandleUpdate(ctx, threadCallbackUpdate(101, "manual-pexels", selectPexels)); err != nil {
		t.Fatal(err)
	}
	first, err := memory.GetThreadDraft(ctx, before.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	firstMedia, err := memory.GetThreadMedia(ctx, first.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if first.MediaMode != domain.ThreadMediaImage || first.Revision != before.Revision+1 ||
		firstMedia.SourceAssetID != "101" || firstMedia.AttachUpdateID != 101 {
		t.Fatalf("first manual Pexels preview draft=%+v media=%+v", first, firstMedia)
	}
	photosSent := snapshotThreadPhotos(telegramClient)
	if len(photosSent) != 1 || !strings.Contains(photosSent[0].Caption, before.Text) ||
		!strings.Contains(photosSent[0].Caption, "First Lens · Pexels") {
		t.Fatalf("first photo preview = %+v", photosSent)
	}
	otherPexels := threadButtonCallback(t, photosSent[0].ReplyMarkup, "🔄 Другое фото из Pexels")
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(102, "other-pexels", otherPexels)); err != nil {
		t.Fatal(err)
	}
	second, err := memory.GetThreadDraft(ctx, before.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	secondMedia, err := memory.GetThreadMedia(ctx, second.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != first.Revision+1 || second.MediaID == first.MediaID ||
		secondMedia.SourceAssetID != "202" || secondMedia.AttachUpdateID != 102 {
		t.Fatalf("replacement draft=%+v media=%+v", second, secondMedia)
	}
	if _, err := memory.GetThreadMedia(ctx, first.MediaID, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old licensed media retained: %v", err)
	}
	calls, queries, exclusions := source.snapshot()
	if calls != 2 || len(queries) != 2 || queries[0] != fallbackLicensedPhotoQuery ||
		len(exclusions[1]) != 1 || exclusions[1][0] != "101" {
		t.Fatalf("photo source calls=%d queries=%v exclusions=%v", calls, queries, exclusions)
	}
	if provider.threadCalls.Load() != 1 || provider.ordinaryCalls.Load() != 0 || observedStore.consumeCalls.Load() != 0 {
		t.Fatalf("manual media crossed generation/quota boundary ordinary=%d threads=%d quota=%d",
			provider.ordinaryCalls.Load(), provider.threadCalls.Load(), observedStore.consumeCalls.Load())
	}
	if strings.Count(logs.String(), `"event":"belcanto_threads_media_changed"`) != 2 ||
		!strings.Contains(logs.String(), `"photo_asset_id":"202"`) {
		t.Fatalf("manual media audit = %q", logs.String())
	}
}

func TestManualPexelsCallbackRetryRedeliversCommittedPhotoWithoutResearch(t *testing.T) {
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, &recordingImagePublisher{})
	source := &rotatingLicensedPhotoSource{assets: []photos.Asset{
		manualPexelsAsset(t, "303", "Retry Lens", color.RGBA{R: 20, G: 110, B: 70, A: 255}),
	}}
	service.threadPhotoSource = source
	flaky := &failOnceThreadTelegram{base: telegramClient}
	service.telegram = flaky
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(200, "/threads")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	selectPexels := threadButtonCallback(t, messages[0].ReplyMarkup, "📷 Подобрать фото в Pexels")
	payload, err := service.callbacks.DecodeForUser(selectPexels, 42)
	if err != nil {
		t.Fatal(err)
	}
	flaky.failPhoto()
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(201, "pexels-preview-fails", selectPexels)); err == nil {
		t.Fatal("manual Pexels preview unexpectedly succeeded")
	}
	committed, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != payload.Revision+1 || committed.MediaMode != domain.ThreadMediaImage {
		t.Fatalf("licensed media transaction was not committed: %+v", committed)
	}
	if calls, _, _ := source.snapshot(); calls != 1 {
		t.Fatalf("photo searches after failed preview = %d", calls)
	}

	service.threadPhotoSource = nil
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(201, "pexels-preview-retry", selectPexels)); err != nil {
		t.Fatal(err)
	}
	after, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != committed.Revision || after.MediaID != committed.MediaID {
		t.Fatalf("retry changed committed licensed media: before=%+v after=%+v", committed, after)
	}
	if calls, _, _ := source.snapshot(); calls != 1 {
		t.Fatalf("retry researched Pexels again: calls=%d", calls)
	}
	photosSent := snapshotThreadPhotos(telegramClient)
	if len(photosSent) != 1 || !strings.Contains(photosSent[0].Caption, "Retry Lens · Pexels") {
		t.Fatalf("retried preview = %+v", photosSent)
	}
}

func TestManualPexelsFailureLeavesExistingDraftUnchanged(t *testing.T) {
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, &recordingImagePublisher{})
	source := &rotatingLicensedPhotoSource{err: photos.ErrNotFound}
	service.threadPhotoSource = source
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(300, "/threads")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	selectPexels := threadButtonCallback(t, messages[0].ReplyMarkup, "📷 Подобрать фото в Pexels")
	payload, err := service.callbacks.DecodeForUser(selectPexels, 42)
	if err != nil {
		t.Fatal(err)
	}
	before, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(301, "pexels-not-found", selectPexels)); err != nil {
		t.Fatal(err)
	}
	after, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.MediaMode != before.MediaMode || after.MediaID != before.MediaID {
		t.Fatalf("failed search changed draft: before=%+v after=%+v", before, after)
	}
	messages = telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "Текстовый черновик сохранён без изменений") {
		t.Fatalf("failure message = %+v", messages[len(messages)-1])
	}
	if len(snapshotThreadPhotos(telegramClient)) != 0 {
		t.Fatal("failed search sent a photo")
	}
}

func TestManualPexelsChoiceSurvivesSmallTextRefinement(t *testing.T) {
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, &recordingImagePublisher{})
	source := &rotatingLicensedPhotoSource{assets: []photos.Asset{
		manualPexelsAsset(t, "404", "Chosen Lens", color.RGBA{R: 70, G: 90, B: 130, A: 255}),
	}}
	service.threadPhotoSource = source
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(350, "/threads")); err != nil {
		t.Fatal(err)
	}
	selectPexels := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "📷 Подобрать фото в Pexels")
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(351, "select-manual-pexels", selectPexels)); err != nil {
		t.Fatal(err)
	}
	selected, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	photosSent := snapshotThreadPhotos(telegramClient)
	shorter := threadButtonCallback(t, photosSent[len(photosSent)-1].ReplyMarkup, "✂️ Короче")
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(352, "refine-with-manual-pexels", shorter)); err != nil {
		t.Fatal(err)
	}
	refined, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if refined.ID == selected.ID || refined.MediaID != selected.MediaID || refined.MediaMode != domain.ThreadMediaImage {
		t.Fatalf("manual Pexels choice was not preserved: selected=%+v refined=%+v", selected, refined)
	}
	if calls, _, _ := source.snapshot(); calls != 1 {
		t.Fatalf("refinement unexpectedly researched Pexels: calls=%d", calls)
	}
}

func TestPexelsButtonIsHiddenAndOldSignedActionFailsClosedWhenDisabled(t *testing.T) {
	service, telegramClient, memory, codec, _, _ := newBelcantoMediaService(t, &recordingImagePublisher{})
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(400, "/threads")); err != nil {
		t.Fatal(err)
	}
	messages := telegramClient.snapshotMessages()
	for _, row := range messages[0].ReplyMarkup.InlineKeyboard {
		for _, button := range row {
			if strings.Contains(button.Text, "Pexels") {
				t.Fatalf("disabled Pexels button is visible: %+v", button)
			}
		}
	}
	draft, err := memory.GetCurrentThreadDraft(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	oldSignedAction, err := codec.Encode(session.CallbackPayload{
		Action: session.ActionThreadUsePexels, UserID: 42,
		InteractionID: draft.ID, Revision: draft.Revision, Candidate: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.HandleUpdate(ctx, threadCallbackUpdate(401, "disabled-pexels", oldSignedAction)); err != nil {
		t.Fatal(err)
	}
	after, err := memory.GetThreadDraft(ctx, draft.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != draft.Revision || after.MediaMode != draft.MediaMode || after.MediaID != draft.MediaID {
		t.Fatalf("disabled action mutated draft: before=%+v after=%+v", draft, after)
	}
	messages = telegramClient.snapshotMessages()
	if !strings.Contains(messages[len(messages)-1].Text, "Pexels сейчас не подключён") {
		t.Fatalf("disabled Pexels response = %+v", messages[len(messages)-1])
	}
}

func TestPexelsKeyboardDistinguishesOwnAndLicensedPhotos(t *testing.T) {
	codec, err := session.NewCallbackCodec([]byte("0123456789abcdef0123456789abcdef"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	draft := domain.ThreadDraft{ID: 1, TelegramID: 42, Revision: 1, MediaMode: domain.ThreadMediaImage, MediaID: 2}
	for _, test := range []struct {
		name           string
		pexelsSelected bool
		want           string
		reject         string
	}{
		{name: "own photo", want: "📷 Подобрать фото в Pexels", reject: "🔄 Другое фото из Pexels"},
		{name: "licensed photo", pexelsSelected: true, want: "🔄 Другое фото из Pexels", reject: "📷 Подобрать фото в Pexels"},
	} {
		t.Run(test.name, func(t *testing.T) {
			keyboard, keyboardErr := threadDraftKeyboard(codec, 42, draft, true, test.pexelsSelected)
			if keyboardErr != nil {
				t.Fatal(keyboardErr)
			}
			if !threadKeyboardHasLabel(keyboard, test.want) || threadKeyboardHasLabel(keyboard, test.reject) {
				t.Fatalf("keyboard = %+v", keyboard)
			}
		})
	}
}

func TestManualPexelsErrorsStaySafeAndActionable(t *testing.T) {
	for _, test := range []struct {
		err      error
		code     string
		contains string
	}{
		{err: photos.ErrAuthentication, code: "pexels_authentication", contains: "не принял API-ключ"},
		{err: photos.ErrRateLimited, code: "pexels_rate_limited", contains: "Лимит Pexels"},
		{err: photos.ErrUnavailable, code: "pexels_unavailable", contains: "временно недоступен"},
		{err: photos.ErrNotFound, code: "pexels_not_found", contains: "Не удалось найти"},
	} {
		if code := safeErrorCode(test.err); code != test.code {
			t.Fatalf("safeErrorCode(%v)=%q", test.err, code)
		}
		if message := threadLicensedPhotoErrorText(test.err, false); !strings.Contains(message, test.contains) {
			t.Fatalf("message for %v = %q", test.err, message)
		}
	}
}

func TestConcurrentManualPexelsReplayAuditsTheCommittedAsset(t *testing.T) {
	service, telegramClient, memory, _, _, _ := newBelcantoMediaService(t, &recordingImagePublisher{})
	source := &synchronizedLicensedPhotoSource{
		assets: []photos.Asset{
			manualPexelsAsset(t, "501", "First Concurrent Lens", color.RGBA{R: 10, G: 70, B: 120, A: 255}),
			manualPexelsAsset(t, "502", "Second Concurrent Lens", color.RGBA{R: 130, G: 40, B: 70, A: 255}),
		},
		release: make(chan struct{}),
	}
	service.threadPhotoSource = source
	service.config.BelcantoReviewLogMode = "off"
	ctx := context.Background()
	if err := service.HandleUpdate(ctx, textUpdate(500, "/threads")); err != nil {
		t.Fatal(err)
	}
	selectPexels := threadButtonCallback(t, telegramClient.snapshotMessages()[0].ReplyMarkup, "📷 Подобрать фото в Pexels")
	payload, err := service.callbacks.DecodeForUser(selectPexels, 42)
	if err != nil {
		t.Fatal(err)
	}
	user, err := memory.GetUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.config.BelcantoReviewLogMode = "full"
	var handlerErrs [2]error
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range handlerErrs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			handlerErrs[index] = service.selectThreadDraftLicensedPhoto(ctx, 501, 42, user, payload)
		}(index)
	}
	close(start)
	wait.Wait()
	if handlerErrs[0] != nil || handlerErrs[1] != nil {
		t.Fatalf("concurrent handlers = %v", handlerErrs)
	}
	draft, err := memory.GetThreadDraft(ctx, payload.InteractionID, 42)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := memory.GetThreadMedia(ctx, draft.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	otherAssetID := "501"
	if committed.SourceAssetID == otherAssetID {
		otherAssetID = "502"
	}
	if strings.Count(logs.String(), `"event":"belcanto_threads_media_changed"`) != 2 ||
		strings.Count(logs.String(), `"photo_asset_id":"`+committed.SourceAssetID+`"`) != 2 ||
		strings.Contains(logs.String(), `"photo_asset_id":"`+otherAssetID+`"`) {
		t.Fatalf("audit does not match committed asset %q: %s", committed.SourceAssetID, logs.String())
	}
}

func threadKeyboardHasLabel(keyboard *telegram.InlineKeyboardMarkup, label string) bool {
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			if button.Text == label {
				return true
			}
		}
	}
	return false
}
