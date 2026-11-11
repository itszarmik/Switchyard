package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"switchyard/internal/providers"
	"switchyard/internal/reqctx"
	"switchyard/internal/schema"
)

// ChatCompletions serves one POST /v1/chat/completions request:
//
//  1. read and validate the body (413, 400);
//  2. resolve the model to its first deployment (404);
//  3. call the provider; a non-streaming answer is returned byte for byte;
//  4. for a stream, commit (200 and the SSE headers) only once the first
//     upstream event has arrived, forward each event as it comes, and end
//     with [DONE]. Unless the client asked for usage, the usage-only chunk
//     is dropped and usage on any other chunk is set to null. A failure
//     after the commit is sent as an error event, without [DONE];
//  5. record the metrics, and annotate the request's access-log line with
//     the completion fields (see reqctx.Annotate).
//
// Canceling ctx, which net/http does when the client disconnects, cancels
// the upstream request.
func (g *Gateway) ChatCompletions(ctx context.Context, ex Exchange) {
	rec := &record{g: g, ctx: ctx, start: time.Now()}
	defer rec.finish()

	body, berr := ex.Body()
	if berr != nil {
		rec.respondError(ex, Info{}, berr)
		return
	}
	req, err := schema.DecodeChatRequest(body)
	if err != nil {
		rec.respondError(ex, Info{}, invalidRequest(err))
		return
	}
	rec.requestedModel = req.Model()
	rec.stream = req.Stream()
	markStream(ctx, req.Stream())

	t, ok := g.targets[req.Model()]
	if !ok {
		rec.respondError(ex, Info{}, modelNotFound(req.Model()))
		return
	}
	rec.target = t
	info := Info{Model: t.model, Provider: t.provider}
	if t.adapter == nil {
		reqctx.Logger(ctx).ErrorContext(ctx, "model is served by an unsupported provider type",
			"model", t.model, "provider", t.provider, "type", t.providerType)
		rec.respondError(ex, info, unsupportedProvider(t.model))
		return
	}

	preq := &providers.Request{Chat: req, UpstreamModel: t.upstreamModel, Header: ex.Header()}
	if req.Stream() {
		g.serveStream(ctx, ex, rec, t, preq, info)
	} else {
		g.serveChat(ctx, ex, rec, t, preq, info)
	}
}

func (g *Gateway) serveChat(ctx context.Context, ex Exchange, rec *record, t *target, preq *providers.Request, info Info) {
	rec.upstreamStart = time.Now()
	resp, err := t.adapter.Chat(ctx, preq)
	rec.upstreamEnd(err)
	if err != nil {
		rec.upstreamFailed(err)
		info.UpstreamRequestID = rec.upstreamRequestID
		rec.respondError(ex, info, upstreamError(err))
		return
	}
	rec.ttft = resp.TTFT
	rec.usage = resp.Usage
	rec.upstreamRequestID = resp.UpstreamRequestID
	info.UpstreamRequestID = resp.UpstreamRequestID
	start := time.Now()
	ex.Respond(info, resp.Status, resp.Body)
	rec.wroteClient(start)
}

func (g *Gateway) serveStream(ctx context.Context, ex Exchange, rec *record, t *target, preq *providers.Request, info Info) {
	rec.upstreamStart = time.Now()
	s, err := t.adapter.ChatStream(ctx, preq)
	if err != nil {
		rec.upstreamEnd(err)
		rec.upstreamFailed(err)
		info.UpstreamRequestID = rec.upstreamRequestID
		rec.respondError(ex, info, upstreamError(err))
		return
	}
	defer func() { _ = s.Close() }()
	wait := time.Since(rec.upstreamStart)
	rec.ttft = s.TTFT()
	rec.upstreamRequestID = s.UpstreamRequestID()
	info.UpstreamRequestID = s.UpstreamRequestID()

	// The commit point: the first upstream event is in hand.
	rec.outcome = pump(ex, rec, s, info, preq.Chat.IncludeUsage(), &wait)
	rec.usage = s.Usage()
	rec.upstreamWait = wait
	rec.upstreamDuration = time.Since(rec.upstreamStart)
}

// pump commits the stream and forwards its events until it ends. It adds
// the time spent waiting on the upstream to *wait and returns the upstream
// outcome.
func pump(ex Exchange, rec *record, s providers.Stream, info Info, wantUsage bool, wait *time.Duration) string {
	start := time.Now()
	err := ex.StartStream(info)
	rec.wroteClient(start)
	if err != nil {
		rec.errorCode = codeClientGone
		return string(providers.KindCanceled)
	}
	for {
		waitStart := time.Now()
		chunk, err := s.Next()
		*wait += time.Since(waitStart)
		switch {
		case errors.Is(err, io.EOF):
			start := time.Now()
			if ex.Done() != nil {
				rec.errorCode = codeClientGone
			}
			rec.wroteClient(start)
			return providers.OutcomeOK
		case err != nil:
			rec.upstreamFailed(err)
			if outcome(err) == providers.KindCanceled {
				rec.errorCode = codeClientGone
			} else {
				rec.errorCode = codeUpstreamError
				start := time.Now()
				_ = ex.Send(streamErrorEvent(err))
				rec.wroteClient(start)
			}
			return providers.Outcome(err)
		case chunk.IsUsageOnly() && !wantUsage:
			continue
		}
		data := chunk.Raw
		if chunk.Usage != nil && !wantUsage {
			// Usage on a chunk that also has choices. The adapter has
			// captured it already.
			data = chunk.RawWithoutUsage()
		}
		start := time.Now()
		err = ex.Send(data)
		rec.wroteClient(start)
		if err != nil {
			// The client is gone; closing the stream cancels the upstream.
			rec.errorCode = codeClientGone
			return string(providers.KindCanceled)
		}
	}
}

// outcome returns the failure kind of an adapter error.
func outcome(err error) providers.Kind {
	return providers.Kind(providers.Outcome(err))
}

// record collects what one request did, for its metrics and its
// access-log annotations.
type record struct {
	g     *Gateway
	ctx   context.Context
	start time.Time

	requestedModel string
	stream         bool
	target         *target
	errorCode      string
	errorMessage   string

	upstreamStart     time.Time
	upstreamWait      time.Duration // time spent blocked on the upstream
	upstreamDuration  time.Duration
	clientWait        time.Duration // time spent writing to the client
	outcome           string        // upstream outcome; "" if no upstream call
	ttft              time.Duration
	usage             *schema.Usage
	upstreamRequestID string
}

// respondError sends e and records it.
func (r *record) respondError(ex Exchange, info Info, e *Error) {
	if r.errorCode == "" {
		r.errorCode = e.Code
		if e.Code == "" && e.Body != nil {
			r.errorCode = codeUpstreamError
		}
	}
	start := time.Now()
	ex.RespondError(info, e)
	r.wroteClient(start)
}

// wroteClient records a write to the client that began at start. That
// time is the client's, not the gateway's overhead.
func (r *record) wroteClient(start time.Time) {
	r.clientWait += time.Since(start)
}

// upstreamEnd records the end of an upstream call that the request spent
// waiting on from start to finish, with result err.
func (r *record) upstreamEnd(err error) {
	r.upstreamDuration = time.Since(r.upstreamStart)
	r.upstreamWait = r.upstreamDuration
	r.outcome = providers.Outcome(err)
}

// upstreamFailed records the details of an adapter error.
func (r *record) upstreamFailed(err error) {
	var pe *providers.Error
	if errors.As(err, &pe) {
		r.upstreamRequestID = pe.UpstreamRequestID
		r.errorMessage = pe.Message
		if pe.Kind == providers.KindCanceled {
			r.errorCode = codeCanceled
		}
	}
}

// finish records the metrics and annotates the access-log line with the
// completion fields. The status is the access line's own. Overhead is the
// request's time minus the time spent waiting on the upstream and writing
// to the client. Instrument records the request count and duration.
func (r *record) finish() {
	latency := time.Since(r.start)
	overhead := max(latency-r.upstreamWait-r.clientWait, 0)
	m := r.g.metrics
	m.overhead.WithLabelValues(EndpointChatCompletions).Observe(overhead.Seconds())

	deployment := ""
	if t := r.target; t != nil {
		deployment = t.deployment()
		if r.outcome != "" {
			m.upstreamRequests.WithLabelValues(t.provider, t.model, r.outcome).Inc()
			m.upstreamDuration.WithLabelValues(t.provider, t.model).Observe(r.upstreamDuration.Seconds())
			if r.ttft > 0 {
				m.upstreamTTFT.WithLabelValues(t.provider, t.model).Observe(r.ttft.Seconds())
			}
		}
	}

	var prompt, completion int64
	if r.usage != nil {
		prompt, completion = r.usage.PromptTokens, r.usage.CompletionTokens
	}
	attrs := []slog.Attr{
		slog.String("requested_model", r.requestedModel),
		slog.String("deployment", deployment),
		slog.Bool("stream", r.stream),
		slog.Float64("ttft_ms", ms(r.ttft)),
		slog.Float64("latency_ms", ms(latency)),
		slog.Float64("overhead_ms", ms(overhead)),
		slog.Int64("prompt_tokens", prompt),
		slog.Int64("completion_tokens", completion),
		slog.String("upstream_request_id", r.upstreamRequestID),
		slog.String("error_code", r.errorCode),
	}
	if r.errorMessage != "" {
		attrs = append(attrs, slog.String("error", r.errorMessage))
	}
	reqctx.Annotate(r.ctx, attrs...)
}

// ms returns d in fractional milliseconds.
func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
