package gateway

import (
	"context"
	"errors"
	"io"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// ChatCompletions serves one POST /v1/chat/completions request:
//
//  1. read and validate the body (413, 400);
//  2. resolve the model through the catalog (404 for an unknown model,
//     400 model_not_supported for a virtual or non-chat model) and choose
//     the cheapest deployment that has every capability the request needs
//     (400 no_eligible_model, naming each deployment and why it was
//     excluded);
//  3. remove the parameters the deployment cannot honor, if they may be
//     dropped, and call its adapter; a non-streaming answer is returned
//     byte for byte;
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
	rec := g.newRecord(ctx, EndpointChatCompletions)
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

	info, sel, ok := g.chooseChat(ex, rec, req, len(body))
	if !ok {
		return
	}
	g.serveChat(ctx, ex, rec, sel, req, info, nil)
}

// chooseChat resolves and selects the deployment for a chat request. When
// it fails it has already answered the client and returns false.
func (g *Gateway) chooseChat(ex Exchange, rec *record, req *schema.ChatRequest, size int) (Info, catalog.Selection, bool) {
	res, e := g.resolve(req.Model(), rec.endpoint, chatType)
	if e != nil {
		rec.respondError(ex, Info{}, e)
		return Info{}, catalog.Selection{}, false
	}
	info := Info{Model: res.Model.ID, Alias: res.Alias}
	rec.alias = res.Alias
	maxTokens, hasMax := req.MaxTokens()
	images, inline := req.ImageParts()
	sel, e := g.choose(res, req.RequiredCapabilities(), estimateIn(size, images, inline, res.Model), estimateOut(maxTokens, hasMax), nil)
	rec.excluded = len(sel.Exclusions)
	if e != nil {
		rec.respondError(ex, info, e)
		return info, sel, false
	}
	return info, sel, true
}

// serveChat sends req to the chosen deployment and answers the client.
// conv, when set, translates the response for the client (the completions
// shim); it is nil for chat completions, which are forwarded as is.
func (g *Gateway) serveChat(ctx context.Context, ex Exchange, rec *record, sel catalog.Selection, req *schema.ChatRequest, info Info, conv converter) {
	d := sel.Deployment
	rec.deployment = d
	info.Provider = d.Provider
	req, info.DroppedParams = req.Without(dropFields(sel.Dropped)...)
	rec.dropped = info.DroppedParams

	adapter := g.adapters[d.Provider]
	preq := &providers.Request{
		Chat: req, UpstreamModel: d.UpstreamModel, Header: ex.Header(),
		MaxOutputTokens: d.Model.MaxOutputTokens, Capabilities: d.Capabilities,
		NoStreamUsage: !d.Capabilities.Has(schema.CapStreamUsage),
	}
	if req.Stream() {
		rec.upstreamStart = time.Now()
		s, err := adapter.ChatStream(ctx, preq)
		g.serveStream(ex, rec, s, err, info, req.IncludeUsage(), conv)
		return
	}
	rec.upstreamStart = time.Now()
	resp, err := adapter.Chat(ctx, preq)
	g.serveResponse(ex, rec, resp, err, info, conv)
}

// converter translates a successful upstream response, and each stream
// chunk, into what the client gets. A chunk that translates to nothing is
// skipped.
type converter interface {
	response(body []byte) ([]byte, error)
	chunk(c *schema.ChatChunk) ([]byte, bool)
}

// serveResponse answers with a non-streaming upstream result.
func (g *Gateway) serveResponse(ex Exchange, rec *record, resp *providers.Response, err error, info Info, conv converter) {
	rec.upstreamEnd(err)
	if err != nil {
		rec.upstreamFailed(err)
		info.UpstreamRequestID = rec.upstreamRequestID
		rec.respondError(ex, info, upstreamError(err))
		return
	}
	rec.served = true
	rec.ttft = resp.TTFT
	rec.usage = resp.Usage
	rec.upstreamRequestID = resp.UpstreamRequestID
	info.UpstreamRequestID = resp.UpstreamRequestID
	body := resp.Body
	if conv != nil {
		out, cerr := conv.response(body)
		if cerr != nil {
			rec.errorMessage = "could not translate the upstream response"
			rec.respondError(ex, info, badUpstreamResponse())
			return
		}
		body = out
	}
	start := time.Now()
	ex.Respond(info, resp.Status, body)
	rec.wroteClient(start)
}

// serveStream answers with an upstream stream, or with the error that
// prevented it from starting.
func (g *Gateway) serveStream(ex Exchange, rec *record, s providers.Stream, err error, info Info, wantUsage bool, conv converter) {
	if err != nil {
		rec.upstreamEnd(err)
		rec.upstreamFailed(err)
		info.UpstreamRequestID = rec.upstreamRequestID
		rec.respondError(ex, info, upstreamError(err))
		return
	}
	defer func() { _ = s.Close() }()
	wait := time.Since(rec.upstreamStart)
	rec.served = true
	rec.ttft = s.TTFT()
	rec.upstreamRequestID = s.UpstreamRequestID()
	info.UpstreamRequestID = s.UpstreamRequestID()

	// The commit point: the first upstream event is in hand.
	rec.outcome = pump(ex, rec, s, info, wantUsage, &wait, conv)
	rec.usage = s.Usage()
	rec.upstreamWait = wait
	rec.upstreamDuration = time.Since(rec.upstreamStart)
}

// pump commits the stream and forwards its events until it ends. It adds
// the time spent waiting on the upstream to *wait and returns the upstream
// outcome.
func pump(ex Exchange, rec *record, s providers.Stream, info Info, wantUsage bool, wait *time.Duration, conv converter) string {
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
		if chunk.Usage != nil && !wantUsage {
			// Usage on a chunk that also has choices. The adapter has
			// captured it already.
			stripped := *chunk
			stripped.Raw = chunk.RawWithoutUsage()
			stripped.Usage = nil
			chunk = &stripped
		}
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
			return string(providers.KindCanceled)
		}
	}
}

// outcome returns the failure kind of an adapter error.
func outcome(err error) providers.Kind {
	return providers.Kind(providers.Outcome(err))
}
