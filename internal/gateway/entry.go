package gateway

import (
	"context"
	"net/http"

	"switchyard/internal/catalog"
	"switchyard/internal/providers"
)

// The gateway's entry points. Each loads the snapshot in force once, when
// the request starts, and serves the whole request on it (see Gateway).

// ChatCompletions serves one POST /v1/chat/completions request, on the
// snapshot in force when it starts: decode and validate, route, look up
// the caches, admit the budgets, execute the plan, respond, settle and
// record (see the package documentation). Canceling ctx, which net/http
// does when the client disconnects, cancels the upstream request.
func (g *Gateway) ChatCompletions(ctx context.Context, ex Exchange) {
	g.current().ChatCompletions(ctx, ex)
}

// Completions serves one POST /v1/completions request, in the legacy
// OpenAI shape, natively or through the chat shim, on the snapshot in
// force when it starts.
func (g *Gateway) Completions(ctx context.Context, ex Exchange) {
	g.current().Completions(ctx, ex)
}

// Embeddings serves one POST /v1/embeddings request on the snapshot in
// force when it starts. The provider may change, the model never does.
func (g *Gateway) Embeddings(ctx context.Context, ex Exchange) {
	g.current().Embeddings(ctx, ex)
}

// ListModels serves GET /v1/models: every catalog model (chat and
// embedding) in config order, then the virtual models the caller may use,
// then every alias in name order, from the snapshot in force. The caller
// is the principal the auth middleware stored in ctx, or the default
// policy's caller without one.
func (g *Gateway) ListModels(ctx context.Context) []Model {
	return g.current().ListModels(ctx)
}

// Explain serves one POST /switchyard/v1/route/explain request: a dry run
// of a chat completions request with the same body and headers, routed as
// ChatCompletions would with the replica's live health and budget state.
// It calls no upstream, charges no budget and records no usage event.
func (g *Gateway) Explain(ctx context.Context, ex Exchange) {
	g.current().Explain(ctx, ex)
}

// ExplainRequest explains how the chat completions request body, with the
// request headers header, from the key keyID of tenant, would be routed:
// the Explanation as indented JSON followed by a newline, or the error
// ChatCompletions would answer with. It is the explain endpoint's answer;
// switchyard route explain calls it on an Options.Offline gateway.
func (g *Gateway) ExplainRequest(ctx context.Context, tenant, keyID string, header http.Header, body []byte) ([]byte, *Error) {
	return g.current().ExplainRequest(ctx, tenant, keyID, header, body)
}

// Adapter returns the adapter of the provider with the given id in the
// snapshot in force, or nil when the provider is not configured or its
// type is not supported.
func (g *Gateway) Adapter(provider string) providers.Adapter {
	return g.current().Adapter(provider)
}

// Catalog returns the catalog of the snapshot in force.
func (g *Gateway) Catalog() *catalog.Catalog { return g.current().catalog }

// ProvidersHandler serves GET /admin/v1/providers: for every provider its
// in-flight count and max_concurrency, and for each of its deployments the
// breaker state and the time of its next transition, the rate-limit
// cooldown, the recent request and failure counts, the retry budget and
// the latency statistics. With Options.BudgetStoreCheck, it also reports
// whether the budget store answers, and budgets.fail_mode. It shows no
// secrets. It belongs on the admin listener.
func (g *Gateway) ProvidersHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.current().serveProviders(w, r) })
}

// CachePurgeHandler serves POST /admin/v1/cache/purge with the body
// {"tenant":"<id>"}: it bumps the tenant's namespace version, which every
// exact key and semantic partition of the tenant includes, so all of its
// entries stop matching at once (they then expire with their TTLs or are
// evicted) and other tenants are untouched. It answers 200 with
// {"tenant", "namespace_version"}, 400 for a missing or unknown tenant or
// a malformed body, and 503 when the namespace store fails. Errors never
// echo the body. The gateway keeps no per-tenant cache state in process,
// so nothing survives a purge on this replica. It belongs on the admin
// listener.
// Every purge writes an audit line (server.Audit) with action cache.purge.
func (g *Gateway) CachePurgeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.current().purge(w, r) })
}
