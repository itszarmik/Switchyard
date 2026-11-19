package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"switchyard/internal/schema"
)

// EndpointExplain is the explain endpoint (DESIGN §9.7).
const EndpointExplain = "/switchyard/v1/route/explain"

// Explain serves one POST /switchyard/v1/route/explain request: a dry run
// of a chat completions request with the same body and headers. It routes
// the request as ChatCompletions would, with the replica's live health and
// budget state, and answers 200 with the router's Explanation as indented
// JSON (see ExplainRequest). It calls no upstream, charges no budget and
// records no usage event; a request ChatCompletions would reject before
// calling an upstream gets the same error.
func (g *snapshot) Explain(ctx context.Context, ex Exchange) {
	rec := g.newRecord(ctx, EndpointExplain)
	rec.dryRun = true
	defer rec.done()
	if !g.knownCaller(ex, rec) {
		return
	}
	body, berr := ex.Body()
	if berr != nil {
		rec.respondError(ex, Info{}, berr)
		return
	}
	data, rt, e := g.explain(ctx, rec.tenant, rec.keyID, ex.Header(), body)
	if rt != nil {
		rec.route, rec.requestedModel = rt, rt.requested
	}
	if e != nil {
		rec.respondError(ex, Info{}, e)
		return
	}
	rec.status = http.StatusOK
	ex.Respond(routeInfo(rt), http.StatusOK, data)
}

// ExplainRequest explains how the chat completions request body, with the
// request headers header, from the key keyID of tenant, would be routed:
// the Explanation as indented JSON followed by a newline, or the error
// ChatCompletions would answer with. It is the explain endpoint's answer;
// switchyard route explain calls it on an Options.Offline gateway.
func (g *snapshot) ExplainRequest(ctx context.Context, tenant, keyID string, header http.Header, body []byte) ([]byte, *Error) {
	data, _, e := g.explain(ctx, tenant, keyID, header, body)
	return data, e
}

// explain checks the request as ChatCompletions does before routing (tags,
// body, deadline header), routes it as a dry run and encodes the full
// decision's explanation. In observe mode that is the decision the
// gateway would make, not the pinned one it serves.
func (g *snapshot) explain(ctx context.Context, tenant, keyID string, header http.Header, body []byte) ([]byte, *routed, *Error) {
	tags, ok := parseTags(header)
	if !ok {
		return nil, nil, invalidTags()
	}
	req, err := schema.DecodeChatRequest(body)
	if err != nil {
		return nil, nil, invalidRequest(err)
	}
	if _, e := g.requestTimeout(header); e != nil {
		return nil, nil, e
	}
	ex := &headerOnly{h: header}
	rec := &record{tenant: tenant, keyID: keyID, tags: tags}
	rr := chatRoute(ex, rec, req, len(body))
	rr.dryRun = true
	rt, e := g.route(ctx, EndpointChatCompletions, &rr, &routed{})
	if e != nil {
		return nil, rt, e
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // reasons such as "quality 0.6 < 0.8" stay readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(rt.full.Explanation); err != nil {
		return nil, rt, internalError("Could not encode the explanation.")
	}
	return b.Bytes(), rt, nil
}

// headerOnly is an Exchange that only has request headers, for building a
// routeRequest outside a pipeline.
type headerOnly struct {
	Exchange
	h http.Header
}

// Header implements Exchange.
func (x *headerOnly) Header() http.Header { return x.h }
