package server

import (
	"bytes"
	"context"
	"net/http"

	"github.com/codemug/shhttp/v2/internal/metrics"
	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/danielgtaylor/huma/v2"
)

type metricsOutput struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

func (s *Server) registerMetrics() {
	if s.sessions != nil {
		metrics.Default.GaugeFunc("shhttp_sessions_running", "Sessions running now.", func() float64 {
			return float64(s.sessions.Running())
		})
	}
	if s.jobs != nil {
		metrics.Default.GaugeFunc("shhttp_jobs_running", "Jobs running now.", func() float64 {
			running, _ := s.jobs.Counts(context.Background())
			return float64(running)
		})
		metrics.Default.GaugeFunc("shhttp_jobs_queued", "Jobs waiting in queues.", func() float64 {
			_, queued := s.jobs.Counts(context.Background())
			return float64(queued)
		})
	}
	metrics.Default.GaugeFunc("shhttp_websocket_connections", "Open WebSocket connections.", func() float64 {
		return float64(s.wsConns.Load())
	})

	op := operation("metrics", http.MethodGet, "/metrics", "Prometheus metrics", "Server", access{scope: api.ScopeAdminRead})
	op.Description = "Counters and gauges in the Prometheus text format. Scrape with a key holding admin:read, " +
		"for example with `authorization: {credentials: shh_…}` in the Prometheus scrape config."
	op.Responses = map[string]*huma.Response{"200": {
		Description: "Metrics.",
		Content:     map[string]*huma.MediaType{"text/plain": {Schema: &huma.Schema{Type: "string"}}},
	}}
	huma.Register(s.api, op, func(ctx context.Context, _ *struct{}) (*metricsOutput, error) {
		var b bytes.Buffer
		metrics.Default.WriteText(&b)
		return &metricsOutput{ContentType: "text/plain; version=0.0.4", Body: b.Bytes()}, nil
	})
}
