// Package metrics keeps counters and gauges and writes them in the
// Prometheus text exposition format.
package metrics

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// Registry holds metrics. The zero value is not usable; use New.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*Counter
	gauges   map[string]*gauge
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{counters: map[string]*Counter{}, gauges: map[string]*gauge{}}
}

// Default is the registry the server exports.
var Default = New()

// Counter is a monotonically increasing value per label combination.
type Counter struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64 // key: label values joined by \x00
}

// Counter returns the counter with this name, creating it on first use.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{name: name, help: help, labels: labels, values: map[string]float64{}}
	r.counters[name] = c
	return c
}

// Add increases the counter for the given label values.
func (c *Counter) Add(n float64, labelValues ...string) {
	if len(labelValues) != len(c.labels) {
		panic(fmt.Sprintf("metric %s takes %d labels", c.name, len(c.labels)))
	}
	c.mu.Lock()
	c.values[strings.Join(labelValues, "\x00")] += n
	c.mu.Unlock()
}

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

type gauge struct {
	help string
	f    func() float64
}

// GaugeFunc registers a gauge read from f at export time. Registering a
// name again replaces the function.
func (r *Registry) GaugeFunc(name, help string, f func() float64) {
	r.mu.Lock()
	r.gauges[name] = &gauge{help: help, f: f}
	r.mu.Unlock()
}

// WriteText writes every metric in the Prometheus text format.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.Lock()
	counters := make([]*Counter, 0, len(r.counters))
	for _, c := range r.counters {
		counters = append(counters, c)
	}
	gaugeNames := make([]string, 0, len(r.gauges))
	for n := range r.gauges {
		gaugeNames = append(gaugeNames, n)
	}
	gauges := r.gauges
	r.mu.Unlock()

	slices.SortFunc(counters, func(a, b *Counter) int { return strings.Compare(a.name, b.name) })
	for _, c := range counters {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		c.mu.Lock()
		keys := make([]string, 0, len(c.values))
		for k := range c.values {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s%s %v\n", c.name, labelText(c.labels, k), c.values[k])
		}
		c.mu.Unlock()
	}
	slices.Sort(gaugeNames)
	for _, n := range gaugeNames {
		g := gauges[n]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", n, g.help, n, n, g.f())
	}
}

func labelText(names []string, key string) string {
	if len(names) == 0 {
		return ""
	}
	values := strings.Split(key, "\x00")
	parts := make([]string, len(names))
	for i, n := range names {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(values[i])
		parts[i] = fmt.Sprintf(`%s="%s"`, n, v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// The server's metrics.
var (
	HTTPRequests     = Default.Counter("shhttp_http_requests_total", "HTTP requests by route pattern and status.", "route", "status")
	AuthFailures     = Default.Counter("shhttp_auth_failures_total", "Requests with a missing, invalid, expired or revoked key.")
	SessionsStarted  = Default.Counter("shhttp_sessions_started_total", "Sessions created.")
	SessionsFinished = Default.Counter("shhttp_sessions_finished_total", "Sessions ended, by final state.", "state")
	OutputBytes      = Default.Counter("shhttp_session_output_bytes_total", "Bytes of stdout and stderr recorded.")
	JobsFinished     = Default.Counter("shhttp_jobs_finished_total", "Jobs ended, by final state.", "state")
)
