// Package threadmedia exposes normalized Belcanto post images to Meta through
// short-lived, capability-style HTTPS URLs. Telegram file URLs are never used:
// they contain the bot token and must not leave the service.
package threadmedia

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/store"
)

const (
	PathPrefix       = "/threads/media/"
	DefaultURLTTL    = time.Hour
	maxClockSkew     = time.Minute
	deliveryKeyBytes = 16
	signatureBytes   = 18
)

var ErrUnavailable = errors.New("threads media delivery is unavailable")

type Reader interface {
	GetThreadMediaByDeliveryKey(context.Context, string) (domain.ThreadMedia, error)
}

type Gateway struct {
	baseURL string
	secret  []byte
	ttl     time.Duration
	reader  Reader
	now     func() time.Time
}

func New(baseURL string, secret []byte, ttl time.Duration, reader Reader) (*Gateway, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return &Gateway{reader: reader, now: time.Now}, nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("threads media base URL must be an HTTPS origin")
	}
	if len(secret) < 16 {
		return nil, errors.New("threads media signing secret must contain at least 16 bytes")
	}
	if ttl <= 5*time.Minute || ttl > 24*time.Hour {
		return nil, errors.New("threads media URL TTL must be greater than 5 minutes and at most 24 hours")
	}
	if reader == nil {
		return nil, errors.New("threads media reader is required")
	}
	return &Gateway{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  deriveKey(secret),
		ttl:     ttl,
		reader:  reader,
		now:     time.Now,
	}, nil
}

func (g *Gateway) Enabled() bool {
	return g != nil && g.baseURL != "" && len(g.secret) != 0 && g.reader != nil
}

func (g *Gateway) PublicURL(deliveryKey string) (string, error) {
	if !g.Enabled() || !validDeliveryKey(deliveryKey) {
		return "", ErrUnavailable
	}
	expires := g.now().UTC().Add(g.ttl).Unix()
	signature := g.signature(deliveryKey, expires)
	return fmt.Sprintf("%s%s%s/%d/%s.jpg", g.baseURL, PathPrefix, deliveryKey, expires, signature), nil
}

func (g *Gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !g.Enabled() {
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deliveryKey, expires, signature, ok := parsePath(request.URL.Path)
	if !ok || !g.verify(deliveryKey, expires, signature) {
		http.NotFound(writer, request)
		return
	}
	mediaValue, err := g.reader.GetThreadMediaByDeliveryKey(request.Context(), deliveryKey)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(writer, request)
		return
	}
	if err != nil || mediaValue.MediaType != "image/jpeg" || len(mediaValue.Data) == 0 || len(mediaValue.Data) > domain.MaxThreadMediaBytes {
		http.Error(writer, "media unavailable", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", mediaValue.MediaType)
	writer.Header().Set("Content-Length", strconv.Itoa(len(mediaValue.Data)))
	writer.Header().Set("Cache-Control", "private, max-age=60")
	writer.Header().Set("Content-Disposition", "inline")
	writer.Header().Set("X-Robots-Tag", "noindex, noimageindex")
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = writer.Write(mediaValue.Data)
	}
}

func (g *Gateway) verify(deliveryKey string, expires int64, signature string) bool {
	if !validDeliveryKey(deliveryKey) || expires <= 0 {
		return false
	}
	now := g.now().UTC()
	expiresAt := time.Unix(expires, 0).UTC()
	if expiresAt.Before(now) || expiresAt.After(now.Add(g.ttl+maxClockSkew)) {
		return false
	}
	want := g.signature(deliveryKey, expires)
	return subtle.ConstantTimeCompare([]byte(want), []byte(signature)) == 1
}

func (g *Gateway) signature(deliveryKey string, expires int64) string {
	mac := hmac.New(sha256.New, g.secret)
	_, _ = fmt.Fprintf(mac, "threads-media-v1\n%s\n%d", deliveryKey, expires)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:signatureBytes])
}

func deriveKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("witty-reply/threads-media-url/v1"))
	return mac.Sum(nil)
}

func parsePath(path string) (string, int64, string, bool) {
	if !strings.HasPrefix(path, PathPrefix) {
		return "", 0, "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, PathPrefix), "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".jpg") {
		return "", 0, "", false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	signature := strings.TrimSuffix(parts[2], ".jpg")
	if len(signature) != base64.RawURLEncoding.EncodedLen(signatureBytes) {
		return "", 0, "", false
	}
	return parts[0], expires, signature, true
}

func validDeliveryKey(value string) bool {
	if len(value) != deliveryKeyBytes*2 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
