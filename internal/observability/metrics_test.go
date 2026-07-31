package observability

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsHandler(t *testing.T) {
	metrics := NewMetrics()
	metrics.Inc("accepted_text")
	metrics.Set("queue_depth", 2)
	metrics.Observe("provider_latency", 1500*time.Millisecond)

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, expected := range []string{"witty_reply_accepted_text_total 1", "witty_reply_queue_depth 2", "witty_reply_provider_latency_seconds_bucket{le=\"2.5\"} 1", "witty_reply_provider_latency_seconds_count 1"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics output missing %q:\n%s", expected, body)
		}
	}
}

func TestUserHashStableAndScoped(t *testing.T) {
	first := UserHash("secret-a", 42)
	if first != UserHash("secret-a", 42) {
		t.Fatal("hash is not stable")
	}
	if first == UserHash("secret-b", 42) || first == UserHash("secret-a", 43) {
		t.Fatal("hash does not scope identity")
	}
}
