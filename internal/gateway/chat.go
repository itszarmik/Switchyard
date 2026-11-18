package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"switchyard/internal/cache"
	"switchyard/internal/policy"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// ChatCompletions serves one POST /v1/chat/completions request:
//
//  1. read and validate the body (413, 400) and the deadline header;
//  2. resolve the model field (404 for an unknown model, 400
//     model_not_supported for a non-chat model, or for auto the policy
//     does not allow), assess it with the client's hints and resolve its
//     policy (400 invalid_header for a malformed routing or cache header);
//     see routePolicy;
//  3. look it up in the policy's response caches (see lookupCache); a hit
//     is answered from the cache at once (see serveHit), before any budget
//     or health check, and a miss carries on;
//  4. plan the deployments that meet the policy and have every capability
//     the request needs, the cheapest eligible first (400
//     no_eligible_model, naming each deployment and why it was excluded;
//     503 no_healthy_upstream when every eligible one is unhealthy); see
//     routeDecide. An identical concurrent non-streaming miss waits for
//     this request's answer instead (see serveMiss);
//  5. execute the plan (see execute): for each attempt, remove the
//     parameters the deployment cannot honor, if they may be dropped, and
//     call its adapter; a non-streaming answer is returned byte for byte;
//  6. for a stream, commit (200 and the SSE headers) only once the first
//     upstream event has arrived, so that a failure before it can still
//     be retried or fall back; forward each event as it comes, and end
//     with [DONE]. Unless the client asked for usage, the usage-only chunk
//     is dropped and usage on any other chunk is set to null. A failure
//     after the commit is sent as an error event, without [DONE], and is
//     never retried;
//  7. write a cacheable answer to the caches, asynchronously (see
//     writeCache); record the metrics, and annotate the request's
//     access-log line with the completion fields (see reqctx.Annotate).
//
// Canceling ctx, which net/http does when the client disconnects, cancels
// the upstream request.
func (g *Gateway) ChatCompletions(ctx context.Context, ex Exchange) {
	rec := g.newRecord(ctx, EndpointChatCompletions)
	defer rec.done()
	if !g.readTags(ex, rec) {
		return
	}

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
	ctx, cancel, ok := g.withDeadline(ctx, ex, rec)
	defer cancel()
	if !ok {
		return
	}

	rr := chatRoute(ex, rec, req, len(body))
	rt, preq, e := g.routePolicy(rec.endpoint, &rr, &rec.rt)
	if e == nil {
		e = g.lookupCache(ctx, ex, rec, req, rt)
	}
	if e != nil {
		g.failRoute(ex, rec, rt, e)
		return
	}
	if rec.cache.entry != nil && g.serveHit(ex, rec, req, rt) {
		return
	}
	g.serveMiss(ctx, ex, rec, req, &rr, rt, preq)
}

// serveMiss serves a chat request the caches did not answer: it plans the
// request (see routeDecide) and executes the plan, then writes the
// response to the caches it was looked up in (see writeCache).
//
// A non-streaming request with an exact key shares its upstream call with
// identical concurrent misses (cache.Coalescer): the first executes, and
// when its response is cacheable the others are answered with it as exact
// hits, without an upstream call; otherwise, or when the first fails or
// its client goes away, each executes on its own. So does one whose own
// policy forbids the deployment that answered the first (see
// entryAllowed), as for any cached answer.
func (g *Gateway) serveMiss(ctx context.Context, ex Exchange, rec *record, req *schema.ChatRequest, rr *routeRequest, rt *routed, preq policy.Request) {
	run := func(ctx context.Context) {
		rt, e := g.routeDecide(ctx, rr, rt, preq)
		if e != nil {
			g.failRoute(ex, rec, rt, e)
			return
		}
		g.usePlan(rec, rt)
		rec.cache.startAggregating(req.Stream())
		g.execute(ctx, ex, rec, g.chatTry(ex, rec, req, nil))
	}
	if !rec.cache.coalesces(req.Stream()) {
		run(ctx)
		g.writeCache(rec)
		return
	}
	led := false
	e, shared, _ := g.cache.coalescer.Do(ctx, rec.cache.key, func(ctx context.Context) (*cache.Entry, bool) {
		led = true
		run(ctx)
		e, _ := g.cacheEntry(rec)
		return e, e != nil
	})
	switch {
	case shared && g.entryAllowed(rt, e):
		g.metrics.cacheCoalesced.Inc()
		rec.cache.entry, rec.cache.status = e, cache.StatusExact
		g.serveHit(ex, rec, req, rt)
		return
	case !led:
		run(ctx)
	}
	g.writeCache(rec)
}

// chatRoute describes a chat request for routing.
func chatRoute(ex Exchange, rec *record, req *schema.ChatRequest, size int) routeRequest {
	maxTokens, hasMax := req.MaxTokens()
	images, inline := req.ImageParts()
	return routeRequest{
		model: req.Model(), header: ex.Header(), chat: req,
		tenant: rec.tenant, keyID: rec.keyID, tags: rec.tags,
		size: size, images: images, inline: inline,
		maxTokens: maxTokens, hasMax: hasMax, outputs: chatOutputs(req),
		required: req.RequiredCapabilities(), stream: req.Stream(),
	}
}

// chatTry returns the attempt function of a chat request. conv, when set,
// translates the response for the client (the completions shim); it is
// nil for chat completions, which are forwarded as is.
func (g *Gateway) chatTry(ex Exchange, rec *record, req *schema.ChatRequest, conv converter) tryFunc {
	return func(ctx context.Context, e *planEntry, info *Info) attempt {
		d := e.d
		info.Provider = d.Provider
		r, dropped := req.Without(dropFields(e.dropped)...)
		info.DroppedParams = dropped
		rec.dropped = dropped
		rec.shadow.req, rec.shadow.orig, rec.shadow.native = r, req, false
		adapter := g.adapters[d.Provider]
		preq := &providers.Request{
			Chat: r, UpstreamModel: d.UpstreamModel, Header: ex.Header(),
			MaxOutputTokens: d.Model.MaxOutputTokens, Capabilities: d.Capabilities,
			NoStreamUsage: !d.Capabilities.Has(schema.CapStreamUsage),
		}
		if r.Stream() {
			return g.streamAttempt(ctx, ex, rec, info, r.IncludeUsage(), conv, func(ctx context.Context) (providers.Stream, error) {
				return adapter.ChatStream(ctx, preq)
			})
		}
		start := time.Now()
		resp, err := adapter.Chat(ctx, preq)
		return g.responseAttempt(ex, rec, start, resp, err, info, conv)
	}
}

// converter translates a successful upstream response, and each stream
// chunk, into what the client gets. A chunk that translates to nothing is
// skipped.
type converter interface {
	response(body []byte) ([]byte, error)
	chunk(c *schema.ChatChunk) ([]byte, bool)
}

// pump commits the stream, unless keep-alives already did, and forwards
// its events until it ends. It adds the time spent waiting on the
// upstream to *wait and returns the kind of the stream's end ("" when it
// completed, canceled when the client went away) and its error, if any.
func pump(ex Exchange, rec *record, s providers.Stream, info Info, wantUsage bool, wait *time.Duration, conv converter) (providers.Kind, error) {
	if !rec.committed {
		start := time.Now()
		err := ex.StartStream(info)
		rec.committed = true
		rec.status = http.StatusOK
		rec.wroteClient(start)
		if err != nil {
			rec.errorCode = codeClientGone
			return providers.KindCanceled, clientGone(err)
		}
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
			return "", nil
		case err != nil:
			rec.upstreamFailed(err)
			kind := outcome(err)
			if kind == providers.KindCanceled {
				rec.errorCode = codeClientGone
			} else {
				rec.errorCode = codeUpstreamError
				start := time.Now()
				_ = ex.Send(streamErrorEvent(err))
				rec.wroteClient(start)
			}
			return kind, err
		}
		// The cache aggregates the chunk as the upstream sent it, usage
		// included, so that a hit can report the cached answer's tokens.
		text := chunkOutputBytes(chunk)
		rec.cache.aggregate(chunk, rec.g.cache.maxEntryBytes)
		if chunk.IsUsageOnly() && !wantUsage {
			continue
		}
		if chunk.Usage != nil && !wantUsage {
			// Usage on a chunk that also has choices. The adapter has
			// captured it already.
			stripped := *chunk
			stripped.Raw = chunk.RawWithoutUsage()
			stripped.Usage = nil
			chunk = &stripped
		}
		rec.outBytes += text
		rec.collectShadowChunk(chunk)
		data := chunk.Raw
		if conv != nil {
			var keep bool
			if data, keep = conv.chunk(chunk); !keep {
				continue
			}
		}
		start := time.Now()
		err = ex.Send(data)
		rec.wroteClient(start)
		if err != nil {
			// The client is gone; closing the stream cancels the upstream.
			rec.errorCode = codeClientGone
			return providers.KindCanceled, clientGone(err)
		}
	}
}

// clientGone is the error of a stream whose client went away, after a
// write to it failed with err.
func clientGone(err error) error {
	return &providers.Error{Kind: providers.KindCanceled, Message: "the client went away", Err: err}
}

// outcome returns the failure kind of an adapter error.
func outcome(err error) providers.Kind {
	return providers.Kind(providers.Outcome(err))
}
