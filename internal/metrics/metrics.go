// Package metrics is a deliberately small Prometheus exposition library:
// counters, labelled counters, histograms and function-backed values,
// written in the text format (version 0.0.4). It exists so linkly needs no
// client library for the handful of series it exports.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metrics and writes them in registration order.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
	names   map[string]bool
}

type metric interface {
	write(w io.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{names: make(map[string]bool)}
}

func (r *Registry) register(name string, m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		panic("metrics: duplicate metric " + name)
	}
	r.names[name] = true
	r.metrics = append(r.metrics, m)
}

// WriteTo writes every metric in the Prometheus text format.
func (r *Registry) WriteTo(w io.Writer) {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// Handler serves the registry at a scrape endpoint.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WriteTo(w)
	})
}

func header(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
}

// Counter is a monotonically increasing integer.
type Counter struct {
	name, help string
	v          atomic.Uint64
}

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	c := &Counter{name: name, help: help}
	r.register(name, c)
	return c
}

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n.
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Value returns the current count.
func (c *Counter) Value() uint64 { return c.v.Load() }

func (c *Counter) write(w io.Writer) {
	header(w, c.name, c.help, "counter")
	fmt.Fprintf(w, "%s %d\n", c.name, c.v.Load())
}

// CounterVec is a family of counters partitioned by label values.
type CounterVec struct {
	name, help string
	labels     []string

	mu     sync.RWMutex
	series map[string]*seriesCounter
}

type seriesCounter struct {
	values []string
	v      atomic.Uint64
}

// NewCounterVec registers a labelled counter family.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, series: make(map[string]*seriesCounter)}
	r.register(name, c)
	return c
}

// Inc increments the series identified by values, given in label order.
func (c *CounterVec) Inc(values ...string) {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", c.name, len(c.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	c.mu.RLock()
	s, ok := c.series[key]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		if s, ok = c.series[key]; !ok {
			s = &seriesCounter{values: append([]string(nil), values...)}
			c.series[key] = s
		}
		c.mu.Unlock()
	}
	s.v.Add(1)
}

// Value returns the count for one series (0 if it does not exist yet).
func (c *CounterVec) Value(values ...string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.series[strings.Join(values, "\xff")]; ok {
		return s.v.Load()
	}
	return 0
}

func (c *CounterVec) write(w io.Writer) {
	header(w, c.name, c.help, "counter")
	c.mu.RLock()
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := c.series[k]
		fmt.Fprintf(w, "%s%s %d\n", c.name, labelString(c.labels, s.values), s.v.Load())
	}
	c.mu.RUnlock()
}

// Histogram counts observations into cumulative buckets.
type Histogram struct {
	name, help string
	bounds     []float64 // upper bounds, ascending, excluding +Inf

	mu     sync.Mutex
	counts []uint64 // per bucket (not cumulative); last is +Inf
	sum    float64
	count  uint64
}

// DefaultLatencyBuckets suits an in-process handler backed by local SQLite:
// 100µs to 1s.
var DefaultLatencyBuckets = []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1}

// NewHistogram registers a histogram with the given ascending upper bounds.
func (r *Registry) NewHistogram(name, help string, bounds []float64) *Histogram {
	if !sort.Float64sAreSorted(bounds) {
		panic("metrics: histogram bounds must be ascending")
	}
	h := &Histogram{name: name, help: help, bounds: bounds, counts: make([]uint64, len(bounds)+1)}
	r.register(name, h)
	return h
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v, i.e. le semantics
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.count++
	h.mu.Unlock()
}

// Count returns the number of observations.
func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

func (h *Histogram) write(w io.Writer) {
	h.mu.Lock()
	counts := append([]uint64(nil), h.counts...)
	sum, count := h.sum, h.count
	h.mu.Unlock()

	header(w, h.name, h.help, "histogram")
	var cum uint64
	for i, b := range h.bounds {
		cum += counts[i]
		fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", h.name, formatFloat(b), cum)
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, count)
	fmt.Fprintf(w, "%s_sum %s\n%s_count %d\n", h.name, formatFloat(sum), h.name, count)
}

// funcMetric reports a value computed at scrape time.
type funcMetric struct {
	name, help, typ string
	fn              func() float64
}

// NewGaugeFunc registers a gauge whose value is read from fn at scrape time.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	r.register(name, &funcMetric{name, help, "gauge", fn})
}

// NewCounterFunc registers a counter whose value is read from fn, which
// must be monotonic.
func (r *Registry) NewCounterFunc(name, help string, fn func() float64) {
	r.register(name, &funcMetric{name, help, "counter", fn})
}

func (f *funcMetric) write(w io.Writer) {
	header(w, f.name, f.help, f.typ)
	fmt.Fprintf(w, "%s %s\n", f.name, formatFloat(f.fn()))
}

func labelString(names, values []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
