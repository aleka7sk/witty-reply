package threadmedia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/store"
)

const testDeliveryKey = "0123456789abcdef0123456789abcdef"

func TestGatewaySignedURLServesGETAndHEAD(t *testing.T) {
	const signingSecret = "gateway-signing-secret-that-must-stay-private"
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	image := []byte{0xff, 0xd8, 0xff, 0xdb, 0x00, 0x43, 0xff, 0xd9}
	lookupCount := 0
	reader := readerFunc(func(_ context.Context, deliveryKey string) (domain.ThreadMedia, error) {
		lookupCount++
		if deliveryKey != testDeliveryKey {
			t.Fatalf("delivery key = %q", deliveryKey)
		}
		return domain.ThreadMedia{Data: image, MediaType: "image/jpeg"}, nil
	})
	gateway := newTestGateway(t, now, signingSecret, reader)
	publicURL, err := gateway.PublicURL(testDeliveryKey)
	if err != nil {
		t.Fatalf("PublicURL() error = %v", err)
	}

	parsed, err := url.Parse(publicURL)
	if err != nil {
		t.Fatalf("Parse(PublicURL()) error = %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "media.example.test" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatalf("PublicURL() = %q", publicURL)
	}
	if !strings.HasPrefix(parsed.Path, PathPrefix+testDeliveryKey+"/") || !strings.HasSuffix(parsed.Path, ".jpg") {
		t.Fatalf("PublicURL() path = %q", parsed.Path)
	}
	if strings.Contains(publicURL, signingSecret) {
		t.Fatal("PublicURL() exposed the raw signing secret")
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			request := httptest.NewRequest(method, publicURL, nil)
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "image/jpeg" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(image)) {
				t.Errorf("Content-Length = %q", got)
			}
			if got := response.Header().Get("Cache-Control"); got != "private, max-age=60" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := response.Header().Get("Content-Disposition"); got != "inline" {
				t.Errorf("Content-Disposition = %q", got)
			}
			if got := response.Header().Get("X-Robots-Tag"); got != "noindex, noimageindex" {
				t.Errorf("X-Robots-Tag = %q", got)
			}
			if method == http.MethodGet && response.Body.String() != string(image) {
				t.Errorf("GET body = %x, want %x", response.Body.Bytes(), image)
			}
			if method == http.MethodHead && response.Body.Len() != 0 {
				t.Errorf("HEAD body = %x, want empty", response.Body.Bytes())
			}
		})
	}
	if lookupCount != 2 {
		t.Fatalf("media lookup count = %d, want 2", lookupCount)
	}
}

func TestGatewayRejectsTamperedExpiredAndWrongMethodRequests(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	lookupCount := 0
	gateway := newTestGateway(t, now, "another-private-signing-secret", readerFunc(func(context.Context, string) (domain.ThreadMedia, error) {
		lookupCount++
		return domain.ThreadMedia{Data: []byte("jpeg"), MediaType: "image/jpeg"}, nil
	}))
	publicURL, err := gateway.PublicURL(testDeliveryKey)
	if err != nil {
		t.Fatalf("PublicURL() error = %v", err)
	}

	tests := []struct {
		name       string
		method     string
		requestURL string
		prepare    func()
		wantStatus int
		wantAllow  string
	}{
		{
			name:       "tampered signature",
			method:     http.MethodGet,
			requestURL: tamperSignature(t, publicURL),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "tampered delivery key",
			method:     http.MethodGet,
			requestURL: strings.Replace(publicURL, testDeliveryKey, "1123456789abcdef0123456789abcdef", 1),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "tampered expiry",
			method:     http.MethodGet,
			requestURL: tamperExpiry(t, publicURL),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "expired",
			method:     http.MethodGet,
			requestURL: publicURL,
			prepare: func() {
				gateway.now = func() time.Time { return now.Add(DefaultURLTTL + time.Second) }
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "wrong method",
			method:     http.MethodPost,
			requestURL: publicURL,
			prepare: func() {
				gateway.now = func() time.Time { return now }
			},
			wantStatus: http.StatusMethodNotAllowed,
			wantAllow:  "GET, HEAD",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare()
			}
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, httptest.NewRequest(test.method, test.requestURL, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if got := response.Header().Get("Allow"); got != test.wantAllow {
				t.Errorf("Allow = %q, want %q", got, test.wantAllow)
			}
		})
	}
	if lookupCount != 0 {
		t.Fatalf("rejected requests reached media reader %d times", lookupCount)
	}
}

func TestGatewayNotFoundDoesNotLeakSigningSecret(t *testing.T) {
	const signingSecret = "not-found-signing-secret-must-remain-private"
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	gateway := newTestGateway(t, now, signingSecret, readerFunc(func(context.Context, string) (domain.ThreadMedia, error) {
		return domain.ThreadMedia{}, store.ErrNotFound
	}))
	publicURL, err := gateway.PublicURL(testDeliveryKey)
	if err != nil {
		t.Fatalf("PublicURL() error = %v", err)
	}

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest(http.MethodGet, publicURL, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	exposed := publicURL + "\n" + response.Header().Get("Location") + "\n" + response.Body.String()
	if strings.Contains(exposed, signingSecret) {
		t.Fatalf("signed media response exposed signing secret: %q", exposed)
	}
}

func newTestGateway(t *testing.T, now time.Time, signingSecret string, reader Reader) *Gateway {
	t.Helper()
	gateway, err := New("https://media.example.test", []byte(signingSecret), DefaultURLTTL, reader)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	gateway.now = func() time.Time { return now }
	return gateway
}

func tamperSignature(t *testing.T, publicURL string) string {
	t.Helper()
	parsed, err := url.Parse(publicURL)
	if err != nil {
		t.Fatalf("Parse(PublicURL()) error = %v", err)
	}
	last := len(parsed.Path) - len(".jpg") - 1
	replacement := byte('A')
	if parsed.Path[last] == replacement {
		replacement = 'B'
	}
	parsed.Path = parsed.Path[:last] + string(replacement) + parsed.Path[last+1:]
	return parsed.String()
}

func tamperExpiry(t *testing.T, publicURL string) string {
	t.Helper()
	parsed, err := url.Parse(publicURL)
	if err != nil {
		t.Fatalf("Parse(PublicURL()) error = %v", err)
	}
	parts := strings.Split(parsed.Path, "/")
	if len(parts) != 6 {
		t.Fatalf("signed path parts = %v", parts)
	}
	expires, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		t.Fatalf("ParseInt(expiry) error = %v", err)
	}
	parts[4] = strconv.FormatInt(expires+1, 10)
	parsed.Path = strings.Join(parts, "/")
	return parsed.String()
}

type readerFunc func(context.Context, string) (domain.ThreadMedia, error)

func (function readerFunc) GetThreadMediaByDeliveryKey(ctx context.Context, deliveryKey string) (domain.ThreadMedia, error) {
	return function(ctx, deliveryKey)
}

var _ Reader = readerFunc(nil)
