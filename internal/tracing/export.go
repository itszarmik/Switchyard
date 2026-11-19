package tracing

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/protobuf/proto"
)

// tracesPath is appended to the endpoint's path.
const tracesPath = "/v1/traces"

// redactedText replaces every part of the endpoint in exporter errors.
const redactedText = "[REDACTED]"

// Exporter limits. An export that has not finished within exportTimeout is
// abandoned and its spans dropped; there are no retries.
const (
	exportTimeout      = 10 * time.Second
	dialTimeout        = 5 * time.Second
	maxResponseBytes   = 64 << 10
	contentTypeProto   = "application/x-protobuf"
	exporterUserAgent  = "switchyard-otlp-exporter"
	maxIdleCollConns   = 2
	idleCollConnExpiry = 90 * time.Second
)

// endpoint is a parsed telemetry.tracing.endpoint.
type endpoint struct {
	// url is where spans are posted: the endpoint without its userinfo,
	// with /v1/traces appended to its path.
	url string
	// authorization is the Basic credential built from the URL's userinfo,
	// or "".
	authorization string
	// redactor removes every part of the endpoint from a string.
	redactor *strings.Replacer
}

// parseEndpoint parses an OTLP/HTTP base URL. Userinfo becomes a Basic
// Authorization header rather than staying in the URL. The error never
// includes the URL.
func parseEndpoint(raw string) (endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return endpoint{}, errors.New("tracing: the endpoint must be an absolute http:// or https:// URL with a host and no query or fragment")
	}
	post := url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimSuffix(u.Path, "/") + tracesPath}
	ep := endpoint{url: post.String()}
	secrets := []string{raw, u.String(), u.Redacted(), ep.url, u.Host, u.Hostname()}
	if u.Path != "" && u.Path != "/" {
		secrets = append(secrets, u.Path, u.EscapedPath())
	}
	if u.User != nil {
		pass, _ := u.User.Password()
		creds := u.User.Username() + ":" + pass
		ep.authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
		secrets = append(secrets, u.User.String(), u.User.Username(), pass, creds, ep.authorization)
	}
	ep.redactor = newRedactor(secrets)
	return ep, nil
}

// newRedactor returns a replacer of every non-empty secret by redactedText,
// longest first, so a secret that contains another is removed whole.
func newRedactor(secrets []string) *strings.Replacer {
	secrets = slices.DeleteFunc(slices.Clone(secrets), func(s string) bool { return s == "" })
	slices.SortFunc(secrets, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	secrets = slices.Compact(secrets)
	pairs := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		pairs = append(pairs, s, redactedText)
	}
	return strings.NewReplacer(pairs...)
}

// ExportStats counts the spans handled since New. Exported counts the
// spans the collector accepted. Dropped counts every sampled span that
// never reached it: those the span processor dropped because its queue was
// full or it was shut down, and those of failed exports.
type ExportStats struct {
	Exported uint64
	Dropped  uint64
}

// otlpExporter posts spans to an OTLP/HTTP collector as protobuf. It is
// Switchyard's own, minimal exporter: the upstream otlptracehttp module
// links gRPC, which would add about 10 MB to the binary. Every export is a
// single attempt; a failed batch is dropped, counted and reported, since
// tracing fails open.
type otlpExporter struct {
	client        *http.Client
	url           string
	authorization string
	report        func(error)

	exported, dropped atomic.Uint64
	stopped           atomic.Bool
}

// newOTLPExporter returns an exporter for ep. It never contacts the
// collector.
func newOTLPExporter(ep endpoint, report func(error)) *otlpExporter {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: exportTimeout,
		MaxIdleConns:          maxIdleCollConns,
		MaxIdleConnsPerHost:   maxIdleCollConns,
		IdleConnTimeout:       idleCollConnExpiry,
		ForceAttemptHTTP2:     true,
	}
	return &otlpExporter{
		client: &http.Client{
			Transport: transport,
			Timeout:   exportTimeout,
			// A redirect would resend the spans, and the credentials, to a
			// URL nobody configured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		url:           ep.url,
		authorization: ep.authorization,
		report:        report,
	}
}

// ExportSpans posts spans in one request, within exportTimeout and ctx. A
// failure is counted and reported, not returned: the SDK would pass a
// returned error to the OpenTelemetry global error handler, and there is
// nothing to retry.
func (e *otlpExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	n := uint64(len(spans))
	if e.stopped.Load() {
		e.dropped.Add(n)
		return nil
	}
	if err := e.post(ctx, spans); err != nil {
		e.dropped.Add(n)
		e.report(err)
		return nil
	}
	e.exported.Add(n)
	return nil
}

// post sends one export request and checks the response. A 2xx response
// is a success, even one that reports a partial success: the collector has
// taken what it could, and resending would not help.
func (e *otlpExporter) post(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	body, err := proto.Marshal(tracesData(spans))
	if err != nil {
		return fmt.Errorf("encode %d spans: %w", len(spans), err)
	}
	ctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the export request: %w", err)
	}
	req.Header.Set("Content-Type", contentTypeProto)
	req.Header.Set("User-Agent", exporterUserAgent)
	if e.authorization != "" {
		req.Header.Set("Authorization", e.authorization)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("export %d spans: %w", len(spans), err)
	}
	// Drain a bounded part of the body so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("export %d spans: the collector answered %s", len(spans), resp.Status)
	}
	return nil
}

// Shutdown stops the exporter: later exports are dropped and counted. It
// closes idle connections and returns ctx's error if ctx is already done.
func (e *otlpExporter) Shutdown(ctx context.Context) error {
	e.stopped.Store(true)
	e.client.CloseIdleConnections()
	return ctx.Err()
}

// stats returns the exporter's counters.
func (e *otlpExporter) stats() ExportStats {
	return ExportStats{Exported: e.exported.Load(), Dropped: e.dropped.Load()}
}

// errorReporter logs tracing errors with the endpoint scrubbed, at most
// once per errorLogInterval; the errors in between are counted in the next
// line's "suppressed" attribute.
type errorReporter struct {
	log      *slog.Logger
	now      func() time.Time
	redactor *strings.Replacer

	mu         sync.Mutex
	last       time.Time // when the last line was logged; zero before the first
	suppressed int
}

func newErrorReporter(log *slog.Logger, now func() time.Time, redactor *strings.Replacer) *errorReporter {
	return &errorReporter{log: log, now: now, redactor: redactor}
}

// report logs err, or counts it when a line was logged less than
// errorLogInterval ago.
func (r *errorReporter) report(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	now := r.now()
	if !r.last.IsZero() && now.Sub(r.last) < errorLogInterval {
		r.suppressed++
		r.mu.Unlock()
		return
	}
	suppressed := r.suppressed
	r.last, r.suppressed = now, 0
	r.mu.Unlock()
	r.log.Warn("tracing: span export failed; spans are dropped until the collector is reachable",
		"error", r.redactor.Replace(err.Error()), "suppressed", suppressed)
}

// sanitize returns err with the endpoint scrubbed from its text, or nil.
// The result no longer wraps err, since err's text holds the endpoint.
func (r *errorReporter) sanitize(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(r.redactor.Replace(err.Error()))
}
