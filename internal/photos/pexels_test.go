package photos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestPexelsFindUsesBoundedSearchAndSkipsPeople(t *testing.T) {
	imageData := testJPEG(t)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "pexels-key" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		query := request.URL.Query()
		if query.Get("query") != "vintage microphone close up" || query.Get("orientation") != "portrait" || query.Get("per_page") != "5" {
			t.Errorf("query = %v", query)
		}
		_, _ = io.WriteString(writer, `{
          "photos": [
            {"id": 11,"url":"https://www.pexels.com/photo/person-11/","photographer":"Face Author","photographer_url":"https://www.pexels.com/@face","alt":"Woman singing into a microphone","src":{"large2x":"https://images.pexels.com/photos/11.jpg"}},
            {"id": 22,"url":"https://www.pexels.com/photo/microphone-22/","photographer":"Lens Author","photographer_url":"https://www.pexels.com/@lens","alt":"Vintage microphone on an empty stage","src":{"large2x":"https://images.pexels.com/photos/22.jpg"}}
          ]
        }`)
	}))
	defer api.Close()
	baseTransport := http.DefaultTransport
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == pexelsImageHost {
			if request.URL.Path != "/photos/22.jpg" {
				t.Errorf("downloaded unsafe candidate path %q", request.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(bytes.NewReader(imageData)), ContentLength: int64(len(imageData)),
				Request: request,
			}, nil
		}
		return baseTransport.RoundTrip(request)
	})}
	provider, err := NewPexels(PexelsConfig{
		APIKey: "pexels-key", BaseURL: api.URL + "/v1", Timeout: time.Second, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := provider.Find(context.Background(), "vintage microphone close up")
	if err != nil {
		t.Fatal(err)
	}
	if asset.AssetID != "22" || asset.Author != "Lens Author" || asset.Provider != "pexels" || !bytes.Equal(asset.Data, imageData) {
		t.Fatalf("asset = %+v", asset)
	}
}

func TestPexelsFindRejectsUntrustedImageHostAndFallsBackToNotFound(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"photos":[{"id":22,"url":"https://www.pexels.com/photo/microphone-22/","photographer":"Lens","photographer_url":"https://www.pexels.com/@lens","alt":"Empty music room","src":{"large2x":"https://evil.example/photo.jpg"}}]}`)
	}))
	defer api.Close()
	provider, err := NewPexels(PexelsConfig{APIKey: "key", BaseURL: api.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Find(context.Background(), "empty music room")
	if err != ErrNotFound {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestPexelsFindRejectsInvalidQueryAndOversizedSearch(t *testing.T) {
	provider, err := NewPexels(PexelsConfig{APIKey: "key", BaseURL: "http://127.0.0.1:1", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Find(context.Background(), "микрофон"); err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("invalid query error = %v", err)
	}

	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.CopyN(writer, strings.NewReader(strings.Repeat("x", maxPexelsJSONBytes+1)), maxPexelsJSONBytes+1)
	}))
	defer api.Close()
	provider, err = NewPexels(PexelsConfig{APIKey: "key", BaseURL: api.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Find(context.Background(), "empty microphone stand"); err != ErrTooLarge {
		t.Fatalf("oversized error = %v, want ErrTooLarge", err)
	}
}

func TestPexelsFindCapsCandidatesDownloadsAndTotalTime(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"photos":[`)
	for index := 1; index <= 12; index++ {
		if index > 1 {
			body.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&body, `{"id":%d,"url":"https://www.pexels.com/photo/object-%d/","photographer":"Lens %d","photographer_url":"https://www.pexels.com/@lens-%d","alt":"Empty microphone stand","src":{"large2x":"https://images.pexels.com/photos/%d.jpg"}}`, index, index, index, index, index)
	}
	body.WriteString(`]}`)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, body.String())
	}))
	defer api.Close()
	baseTransport := http.DefaultTransport
	var downloads atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == pexelsImageHost {
			downloads.Add(1)
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("failed")), Request: request}, nil
		}
		return baseTransport.RoundTrip(request)
	})}
	provider, err := NewPexels(PexelsConfig{APIKey: "key", BaseURL: api.URL, Timeout: time.Second, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Find(context.Background(), "empty microphone stand"); err != ErrNotFound {
		t.Fatalf("error = %v", err)
	}
	if downloads.Load() != maxPexelsDownloads {
		t.Fatalf("download attempts = %d, want %d", downloads.Load(), maxPexelsDownloads)
	}

	slowAPI := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer slowAPI.Close()
	provider, err = NewPexels(PexelsConfig{APIKey: "key", BaseURL: slowAPI.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	provider.timeout = 25 * time.Millisecond
	started := time.Now()
	_, err = provider.Find(context.Background(), "empty microphone stand")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("bounded timeout error=%v elapsed=%s", err, time.Since(started))
	}
}

func TestPexelsAlwaysRejectsRedirectsAndUnsafeAttribution(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer api.Close()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return nil }}
	provider, err := NewPexels(PexelsConfig{APIKey: "key", BaseURL: api.URL, Timeout: time.Second, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Find(context.Background(), "empty microphone stand"); err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect error=%v target hits=%d", err, redirected.Load())
	}
	if safeAttributionText("Lens\nInjected", 160) || !safeAttributionText("Lens Author", 160) {
		t.Fatal("unsafe photographer attribution validation")
	}
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 400, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 400; x++ {
			value.Set(x, y, color.RGBA{R: 30, G: 40, B: 50, A: 255})
		}
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, value, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
