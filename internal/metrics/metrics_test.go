package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("jobs_total", "Jobs run.")
	cv := r.NewCounterVec("http_requests_total", "Requests.", "route", "code")
	h := r.NewHistogram("latency_seconds", "Latency.", []float64{0.25, 1})
	r.NewGaugeFunc("queue_depth", "Queued items.", func() float64 { return 3 })
	r.NewCounterFunc("dropped_total", "Dropped items.\nSecond line.", func() float64 { return 7 })

	c.Add(2)
	c.Inc()
	cv.Inc("GET /{code}", "302")
	cv.Inc("GET /{code}", "302")
	cv.Inc(`we"ird\`, "500")
	h.Observe(0.125)
	h.Observe(0.25) // le is inclusive
	h.Observe(0.5)
	h.Observe(4)

	var b strings.Builder
	r.WriteTo(&b)
	want := `# HELP jobs_total Jobs run.
# TYPE jobs_total counter
jobs_total 3
# HELP http_requests_total Requests.
# TYPE http_requests_total counter
http_requests_total{route="GET /{code}",code="302"} 2
http_requests_total{route="we\"ird\\",code="500"} 1
# HELP latency_seconds Latency.
# TYPE latency_seconds histogram
latency_seconds_bucket{le="0.25"} 2
latency_seconds_bucket{le="1"} 3
latency_seconds_bucket{le="+Inf"} 4
latency_seconds_sum 4.875
latency_seconds_count 4
# HELP queue_depth Queued items.
# TYPE queue_depth gauge
queue_depth 3
# HELP dropped_total Dropped items.\nSecond line.
# TYPE dropped_total counter
dropped_total 7
`
	if got := b.String(); got != want {
		t.Fatalf("exposition mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if cv.Value("GET /{code}", "302") != 2 || cv.Value("nope", "x") != 0 {
		t.Fatal("CounterVec.Value wrong")
	}
	if h.Count() != 4 || c.Value() != 3 {
		t.Fatal("Count/Value wrong")
	}
}

func TestHandlerContentType(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("x_total", "x")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "x_total 0") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestPanics(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{"duplicate name", func() {
			r := NewRegistry()
			r.NewCounter("a", "")
			r.NewCounter("a", "")
		}},
		{"wrong label count", func() {
			NewRegistry().NewCounterVec("a", "", "x", "y").Inc("only-one")
		}},
		{"unsorted buckets", func() {
			NewRegistry().NewHistogram("a", "", []float64{2, 1})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			tt.fn()
		})
	}
}

func TestConcurrentUse(t *testing.T) {
	r := NewRegistry()
	cv := r.NewCounterVec("c_total", "", "k")
	h := r.NewHistogram("h", "", DefaultLatencyBuckets)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 1000 {
				cv.Inc([]string{"a", "b"}[(i+j)%2])
				h.Observe(float64(j) / 1000)
			}
		})
		wg.Go(func() { r.WriteTo(&strings.Builder{}) })
	}
	wg.Wait()
	if got := cv.Value("a") + cv.Value("b"); got != 8000 {
		t.Fatalf("total = %d, want 8000", got)
	}
	if h.Count() != 8000 {
		t.Fatalf("histogram count = %d", h.Count())
	}
}
