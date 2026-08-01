package observability

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Metrics struct {
	mu       sync.RWMutex
	counters map[string]*atomic.Int64
	gauges   map[string]*atomic.Int64
	latency  map[string]*durationMetric
}

type durationMetric struct {
	count   atomic.Int64
	nanos   atomic.Int64
	buckets []atomic.Int64
}

var durationBucketBounds = [...]float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 45, 60}

func NewMetrics() *Metrics {
	return &Metrics{counters: make(map[string]*atomic.Int64), gauges: make(map[string]*atomic.Int64), latency: make(map[string]*durationMetric)}
}

func (m *Metrics) Inc(name string) { m.Add(name, 1) }

func (m *Metrics) Add(name string, delta int64) {
	metric := m.counter(sanitize(name))
	metric.Add(delta)
}

func (m *Metrics) Set(name string, value int64) {
	name = sanitize(name)
	m.mu.Lock()
	metric := m.gauges[name]
	if metric == nil {
		metric = &atomic.Int64{}
		m.gauges[name] = metric
	}
	m.mu.Unlock()
	metric.Store(value)
}

func (m *Metrics) Observe(name string, duration time.Duration) {
	name = sanitize(name)
	m.mu.Lock()
	metric := m.latency[name]
	if metric == nil {
		metric = &durationMetric{buckets: make([]atomic.Int64, len(durationBucketBounds))}
		m.latency[name] = metric
	}
	m.mu.Unlock()
	metric.count.Add(1)
	metric.nanos.Add(duration.Nanoseconds())
	seconds := duration.Seconds()
	for index, upperBound := range durationBucketBounds {
		if seconds <= upperBound {
			metric.buckets[index].Add(1)
		}
	}
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.write(w)
	})
}

func (m *Metrics) write(w http.ResponseWriter) {
	m.mu.RLock()
	names := make([]string, 0, len(m.counters)+len(m.gauges)+len(m.latency))
	for name := range m.counters {
		names = append(names, name+"|counter")
	}
	for name := range m.gauges {
		names = append(names, name+"|gauge")
	}
	for name := range m.latency {
		names = append(names, name+"|latency")
	}
	sort.Strings(names)
	for _, item := range names {
		parts := strings.SplitN(item, "|", 2)
		name, kind := parts[0], parts[1]
		switch kind {
		case "counter":
			_, _ = fmt.Fprintf(w, "witty_reply_%s_total %d\n", name, m.counters[name].Load())
		case "gauge":
			_, _ = fmt.Fprintf(w, "witty_reply_%s %d\n", name, m.gauges[name].Load())
		case "latency":
			metric := m.latency[name]
			for index, upperBound := range durationBucketBounds {
				_, _ = fmt.Fprintf(w, "witty_reply_%s_seconds_bucket{le=\"%g\"} %d\n", name, upperBound, metric.buckets[index].Load())
			}
			_, _ = fmt.Fprintf(w, "witty_reply_%s_seconds_bucket{le=\"+Inf\"} %d\n", name, metric.count.Load())
			_, _ = fmt.Fprintf(w, "witty_reply_%s_seconds_count %d\n", name, metric.count.Load())
			_, _ = fmt.Fprintf(w, "witty_reply_%s_seconds_sum %.6f\n", name, float64(metric.nanos.Load())/float64(time.Second))
		}
	}
	m.mu.RUnlock()
}

func (m *Metrics) counter(name string) *atomic.Int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	metric := m.counters[name]
	if metric == nil {
		metric = &atomic.Int64{}
		m.counters[name] = metric
	}
	return metric
}

func sanitize(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	return strings.Trim(builder.String(), "_")
}
