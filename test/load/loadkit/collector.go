package loadkit

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// maxExportBytes bounds one export request the Collector reads.
const maxExportBytes = 32 << 20

// Collector is a minimal OTLP/HTTP trace collector for load runs: it
// decodes every protobuf export posted to /v1/traces, counts its spans and
// keeps nothing, so it can take a soak's span stream for hours. Point a
// gateway's telemetry.tracing.endpoint at Endpoint.
type Collector struct {
	ts       *httptest.Server
	spans    atomic.Int64
	exports  atomic.Int64
	rejected atomic.Int64
}

// NewCollector starts a Collector on a loopback port.
func NewCollector() *Collector {
	c := &Collector{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", c.serve)
	c.ts = httptest.NewServer(mux)
	return c
}

// Endpoint is the collector's base URL, for telemetry.tracing.endpoint.
func (c *Collector) Endpoint() string { return c.ts.URL }

// Close shuts the collector down.
func (c *Collector) Close() { c.ts.Close() }

// Spans returns the spans received so far.
func (c *Collector) Spans() int64 { return c.spans.Load() }

// Exports returns the export requests accepted so far.
func (c *Collector) Exports() int64 { return c.exports.Load() }

// Rejected returns the export requests that could not be read or decoded.
func (c *Collector) Rejected() int64 { return c.rejected.Load() }

func (c *Collector) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxExportBytes))
	var td tracepb.TracesData
	if err != nil || r.Header.Get("Content-Type") != "application/x-protobuf" || proto.Unmarshal(body, &td) != nil {
		c.rejected.Add(1)
		http.Error(w, "bad export", http.StatusBadRequest)
		return
	}
	var n int64
	for _, rs := range td.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			n += int64(len(ss.GetSpans()))
		}
	}
	c.spans.Add(n)
	c.exports.Add(1)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}
