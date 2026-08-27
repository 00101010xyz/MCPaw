// Package metrics provides a dependency-free Prometheus-compatible registry.
//
// Only three instrument types are needed by MCPaw (counter, gauge, histogram),
// so we implement them directly rather than pulling in the client library. That
// keeps the dependency surface — and therefore the supply-chain attack surface
// — at four direct modules.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultBuckets are latency buckets in seconds, chosen to straddle the range
// between a fast local API (Zotero, single-digit ms) and a slow remote one.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Registry holds all instruments and renders them in Prometheus text format.
type Registry struct {
	mu     sync.RWMutex
	series map[string]*family
}

type family struct {
	name    string
	help    string
	typ     string
	buckets []float64

	mu       sync.RWMutex
	counters map[string]*atomic.Uint64 // label signature -> value (x1000 for floats)
	gauges   map[string]*atomic.Int64
	hists    map[string]*histogram
	labels   map[string][]string // signature -> ordered label values
	keys     []string            // label names, fixed per family
}

type histogram struct {
	mu     sync.Mutex
	counts []uint64
	sum    float64
	total  uint64
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{series: make(map[string]*family)} }

func (r *Registry) family(name, help, typ string, keys []string, buckets []float64) *family {
	r.mu.RLock()
	f, ok := r.series[name]
	r.mu.RUnlock()
	if ok {
		return f
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.series[name]; ok {
		return f
	}
	f = &family{
		name: name, help: help, typ: typ, keys: keys, buckets: buckets,
		counters: map[string]*atomic.Uint64{},
		gauges:   map[string]*atomic.Int64{},
		hists:    map[string]*histogram{},
		labels:   map[string][]string{},
	}
	r.series[name] = f
	return f
}

// Counter is a monotonically increasing integer instrument.
type Counter struct {
	f   *family
	sig string
}

// NewCounter registers (or looks up) a counter family and binds label values.
func (r *Registry) NewCounter(name, help string, labelKeys, labelValues []string) *Counter {
	f := r.family(name, help, "counter", labelKeys, nil)
	sig := signature(labelValues)
	f.mu.Lock()
	if _, ok := f.counters[sig]; !ok {
		f.counters[sig] = &atomic.Uint64{}
		f.labels[sig] = labelValues
	}
	f.mu.Unlock()
	return &Counter{f: f, sig: sig}
}

// Inc adds one to the counter.
func (c *Counter) Inc() { c.Add(1) }

// Add adds n to the counter. Negative values are ignored: a counter that could
// go backwards would silently corrupt every rate() built on top of it.
func (c *Counter) Add(n uint64) {
	c.f.mu.RLock()
	v := c.f.counters[c.sig]
	c.f.mu.RUnlock()
	if v != nil {
		v.Add(n)
	}
}

// Gauge is an instrument whose value may go up or down.
type Gauge struct {
	f   *family
	sig string
}

// NewGauge registers (or looks up) a gauge family and binds label values.
func (r *Registry) NewGauge(name, help string, labelKeys, labelValues []string) *Gauge {
	f := r.family(name, help, "gauge", labelKeys, nil)
	sig := signature(labelValues)
	f.mu.Lock()
	if _, ok := f.gauges[sig]; !ok {
		f.gauges[sig] = &atomic.Int64{}
		f.labels[sig] = labelValues
	}
	f.mu.Unlock()
	return &Gauge{f: f, sig: sig}
}

// Set assigns the gauge value.
func (g *Gauge) Set(v int64) {
	g.f.mu.RLock()
	x := g.f.gauges[g.sig]
	g.f.mu.RUnlock()
	if x != nil {
		x.Store(v)
	}
}

// Add adds delta to the gauge value.
func (g *Gauge) Add(delta int64) {
	g.f.mu.RLock()
	x := g.f.gauges[g.sig]
	g.f.mu.RUnlock()
	if x != nil {
		x.Add(delta)
	}
}

// Histogram observes a distribution of values, typically latency in seconds.
type Histogram struct {
	f   *family
	sig string
}

// NewHistogram registers (or looks up) a histogram family and binds label values.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labelKeys, labelValues []string) *Histogram {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	f := r.family(name, help, "histogram", labelKeys, buckets)
	sig := signature(labelValues)
	f.mu.Lock()
	if _, ok := f.hists[sig]; !ok {
		f.hists[sig] = &histogram{counts: make([]uint64, len(f.buckets))}
		f.labels[sig] = labelValues
	}
	f.mu.Unlock()
	return &Histogram{f: f, sig: sig}
}

// Observe records one sample.
func (h *Histogram) Observe(v float64) {
	h.f.mu.RLock()
	hist := h.f.hists[h.sig]
	buckets := h.f.buckets
	h.f.mu.RUnlock()
	if hist == nil {
		return
	}
	hist.mu.Lock()
	defer hist.mu.Unlock()
	hist.sum += v
	hist.total++
	for i, b := range buckets {
		if v <= b {
			hist.counts[i]++
		}
	}
}

// Write renders every registered instrument in Prometheus text exposition
// format.
func (r *Registry) Write(w io.Writer) error {
	r.mu.RLock()
	names := make([]string, 0, len(r.series))
	for n := range r.series {
		names = append(names, n)
	}
	families := r.series
	r.mu.RUnlock()
	sort.Strings(names)

	var sb strings.Builder
	for _, n := range names {
		f := families[n]
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		f.mu.RLock()
		sigs := make([]string, 0, len(f.labels))
		for s := range f.labels {
			sigs = append(sigs, s)
		}
		sort.Strings(sigs)
		for _, s := range sigs {
			lbl := renderLabels(f.keys, f.labels[s])
			switch f.typ {
			case "counter":
				if c := f.counters[s]; c != nil {
					fmt.Fprintf(&sb, "%s%s %d\n", f.name, lbl, c.Load())
				}
			case "gauge":
				if g := f.gauges[s]; g != nil {
					fmt.Fprintf(&sb, "%s%s %d\n", f.name, lbl, g.Load())
				}
			case "histogram":
				h := f.hists[s]
				if h == nil {
					continue
				}
				h.mu.Lock()
				for i, b := range f.buckets {
					fmt.Fprintf(&sb, "%s_bucket%s %d\n", f.name,
						withLabel(f.keys, f.labels[s], "le", strconv.FormatFloat(b, 'g', -1, 64)), h.counts[i])
				}
				fmt.Fprintf(&sb, "%s_bucket%s %d\n", f.name,
					withLabel(f.keys, f.labels[s], "le", "+Inf"), h.total)
				fmt.Fprintf(&sb, "%s_sum%s %g\n", f.name, lbl, h.sum)
				fmt.Fprintf(&sb, "%s_count%s %d\n", f.name, lbl, h.total)
				h.mu.Unlock()
			}
		}
		f.mu.RUnlock()
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

func signature(values []string) string { return strings.Join(values, "\x00") }

func renderLabels(keys, values []string) string {
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, 0, len(keys))
	for i, k := range keys {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, escapeLabel(v)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func withLabel(keys, values []string, extraKey, extraVal string) string {
	k := append(append([]string{}, keys...), extraKey)
	v := append(append([]string{}, values...), extraVal)
	return renderLabels(k, v)
}

func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	return r.Replace(v)
}
