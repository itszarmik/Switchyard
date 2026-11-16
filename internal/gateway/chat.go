package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// ChatCompletions serves one POST /v1/chat/completions request:
//
//  1. read and validate the body (413, 400) and the deadline header;
//  2. route it (see route): resolve the model field (404 for an unknown
//     model, 400 model_not_supported for a non-chat model, or for auto
//     the policy does not allow), assess it with the client's hints and
//     resolve its policy (400 invalid_header for a malformed routing
//     header), and plan the deployments that meet the policy and have
//     every capability the request needs, the cheapest eligible first
//     (400 no_eligible_model, naming each deployment and why it was
//     excluded; 503 no_healthy_upstream when every eligible one is
//     unhealthy);
//  3. execute the plan (see execute): for each attempt, remove the
//     parameters the deployment cannot honor, if they may be dropped, and
//     call its adapter; a non-streaming answer is returned byte for byte;
//  4. for a stream, commit (200 and the SSE headers) only once the first
//     upstream event has arrived, so that a failure before it can still
//     be retried or fall back; forward each event as it comes, and end
//     with [DONE]. Unless the client asked for usage, the usage-only chunk
//     is dropped and usage on any other chunk is set to null. A failure
//     after the commit is sent as an error event, without [DONE], and is
//     never retried;
//  5. record the metrics, and annotate the request's access-log line with
//     the completion fields (see reqctx.Annotate).
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

	if !g.planChat(ctx, ex, rec, req, len(body)) {
		return
	}
	g.execute(ctx, ex, rec, g.chatTry(ex, rec, req, nil))
}

// planChat routes a chat request (see route) and makes the decision rec's
// plan. When it fails it has already answered the client and returns
// false.
func (g *Gateway) planChat(ctx context.Context, ex Exchange, rec *record, req *schema.ChatRequest, size int) bool {
	rr := chatRoute(ex, rec, req, size)
	rt, e := g.route(ctx, rec.endpoint, &rr, &rec.rt)
	if e != nil {
		g.failRoute(ex, rec, rt, e)
		return false
	}
	g.usePlan(rec, rt)
	return true
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
		case chunk.IsUsageOnly() && !wantUsage:
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
		rec.outBytes += chunkOutputBytes(chunk)
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
