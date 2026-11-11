// Package openaiapi serves the OpenAI-compatible /v1 endpoints over HTTP:
// POST /v1/chat/completions and POST /v1/completions (JSON and SSE),
// POST /v1/embeddings and GET /v1/models.
//
// The handlers own the HTTP details (reading the body, the response and
// X-Switchyard-* headers, the OpenAI error envelope, SSE framing and
// flushing, and the read and write deadlines) and leave the pipeline itself
// to the gateway. They do no authentication; the composition root wraps
// every route with the auth middleware, and that with gateway.Instrument.
package openaiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"switchyard/internal/apierror"
	"switchyard/internal/gateway"
	"switchyard/internal/reqctx"
	"switchyard/internal/server"
	"switchyard/internal/sse"
)

// Response headers that say who served a request.
const (
	headerModel             = "X-Switchyard-Model"
	headerProvider          = "X-Switchyard-Provider"
	headerUpstreamRequestID = "X-Switchyard-Upstream-Request-Id"
	headerAlias             = "X-Switchyard-Alias"
	headerDroppedParams     = "X-Switchyard-Dropped-Params"
	headerShim              = "X-Switchyard-Shim"
	headerAttempts          = "X-Switchyard-Attempts"
	headerRouteReason       = "X-Switchyard-Route-Reason"
)

// Defaults for the zero values of Options.
const (
	defaultBodyReadTimeout = 60 * time.Second
	defaultWriteTimeout    = time.Minute
)

// Options configures the handlers.
type Options struct {
	// BodyReadTimeout bounds reading a request body; a client that has not
	// sent it all in time gets 408. Zero means 60s.
	BodyReadTimeout time.Duration
	// WriteTimeout bounds each write to the client: a whole JSON response,
	// or one streamed event. A client that stops reading cannot hold the
	// handler (and the upstream) longer. Zero means one minute.
	WriteTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.BodyReadTimeout <= 0 {
		o.BodyReadTimeout = defaultBodyReadTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	return o
}

// Route is a handler and the ServeMux pattern to register it under.
type Route struct {
	Pattern string
	Handler http.Handler
	// Endpoint is the route's endpoint label, for gateway.Instrument.
	Endpoint string
}

// Routes returns the /v1 routes served by gw.
func Routes(gw *gateway.Gateway, opts Options) []Route {
	opts = opts.withDefaults()
	post := func(endpoint string, serve func(context.Context, gateway.Exchange)) Route {
		return Route{Pattern: "POST " + endpoint, Endpoint: endpoint, Handler: pipelineHandler{serve: serve, opts: opts}}
	}
	return []Route{
		post(gateway.EndpointChatCompletions, gw.ChatCompletions),
		post(gateway.EndpointCompletions, gw.Completions),
		post(gateway.EndpointEmbeddings, gw.Embeddings),
		{Pattern: "GET " + gateway.EndpointModels, Endpoint: gateway.EndpointModels, Handler: modelsHandler{gw: gw}},
	}
}

// pipelineHandler serves one POST endpoint through a gateway pipeline.
type pipelineHandler struct {
	serve func(context.Context, gateway.Exchange)
	opts  Options
}

// ServeHTTP serves the endpoint.
func (h pipelineHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ex := &exchange{w: w, r: r, rc: http.NewResponseController(w), opts: h.opts}
	defer ex.finish()
	h.serve(r.Context(), ex)
}

type modelsHandler struct{ gw *gateway.Gateway }

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// ServeHTTP serves GET /v1/models.
func (h modelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	models := h.gw.ListModels()
	list := modelList{Object: "list", Data: make([]modelObject, 0, len(models))}
	for _, m := range models {
		list.Data = append(list.Data, modelObject{ID: m.ID, Object: "model", Created: m.Created, OwnedBy: "switchyard"})
	}
	data, err := json.Marshal(list)
	if err != nil {
		ctx := r.Context()
		reqctx.Logger(ctx).ErrorContext(ctx, "encode model list", "error", err)
		apierror.Write(w, http.StatusInternalServerError, apierror.TypeServer, "internal_error", "Could not encode the model list.")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// exchange implements gateway.Exchange over one HTTP request.
type exchange struct {
	w    http.ResponseWriter
	r    *http.Request
	rc   *http.ResponseController
	opts Options

	tooLarge    int64 // the body limit, once the body was found too large
	sw          *sse.Writer
	hasDeadline bool // a write deadline was set
}

var _ gateway.Exchange = (*exchange)(nil)

// Body implements gateway.Exchange. The read is bounded by BodyReadTimeout.
//
// After a successful read the deadline is cleared at once: net/http then
// reads the connection in the background to notice a client that goes
// away, and a deadline left in place would cut off a long stream. After a
// failed read the rest of the body is abandoned (see server.AbandonBody).
func (e *exchange) Body() ([]byte, *gateway.Error) {
	_ = e.rc.SetReadDeadline(time.Now().Add(e.opts.BodyReadTimeout))
	data, err := io.ReadAll(e.r.Body)
	if err == nil {
		_ = e.rc.SetReadDeadline(time.Time{})
		return data, nil
	}
	server.AbandonBody(e.w, e.r)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return nil, &gateway.Error{
			Status: http.StatusRequestTimeout, Type: apierror.TypeInvalidRequest, Code: "request_timeout",
			Message: fmt.Sprintf("The request body was not received within %s.", e.opts.BodyReadTimeout),
		}
	}
	var mbe *http.MaxBytesError
	if server.IsRequestTooLarge(err) && errors.As(err, &mbe) {
		e.tooLarge = mbe.Limit
		return nil, &gateway.Error{
			Status: http.StatusRequestEntityTooLarge, Type: apierror.TypeInvalidRequest, Code: "request_too_large",
			Message: fmt.Sprintf("Request body too large: the limit is %d bytes.", mbe.Limit),
		}
	}
	return nil, &gateway.Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: "invalid_body",
		Message: "The request body could not be read.",
	}
}

// Header implements gateway.Exchange.
func (e *exchange) Header() http.Header { return e.r.Header }

// Respond implements gateway.Exchange.
func (e *exchange) Respond(info gateway.Info, status int, body []byte) {
	setInfo(e.w.Header(), info)
	e.setWriteDeadline()
	writeJSON(e.w, status, body)
}

// RespondError implements gateway.Exchange.
func (e *exchange) RespondError(info gateway.Info, ge *gateway.Error) {
	h := e.w.Header()
	setInfo(h, info)
	if ge.RetryAfter > 0 {
		h.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(ge.RetryAfter.Seconds())), 10))
	}
	e.setWriteDeadline()
	switch {
	case ge.Body != nil:
		writeJSON(e.w, ge.Status, ge.Body)
	case e.tooLarge > 0:
		server.WriteRequestTooLarge(e.w, e.tooLarge)
	default:
		apierror.WriteParam(e.w, ge.Status, ge.Type, ge.Code, ge.Message, ge.Param)
	}
}

// StartStream implements gateway.Exchange.
func (e *exchange) StartStream(info gateway.Info) error {
	h := e.w.Header()
	setInfo(h, info)
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	e.setWriteDeadline()
	e.w.WriteHeader(http.StatusOK)
	if err := e.rc.Flush(); err != nil {
		return fmt.Errorf("openaiapi: start stream: %w", err)
	}
	e.sw = sse.NewWriter(e.w, e.rc.Flush)
	return nil
}

// Send implements gateway.Exchange.
func (e *exchange) Send(data []byte) error {
	e.setWriteDeadline()
	return e.sw.WriteData(data)
}

// Comment implements gateway.Exchange.
func (e *exchange) Comment(text string) error {
	e.setWriteDeadline()
	if err := e.sw.WriteComment(text); err != nil {
		return fmt.Errorf("openaiapi: %w", err)
	}
	return nil
}

// Done implements gateway.Exchange.
func (e *exchange) Done() error {
	e.setWriteDeadline()
	return e.sw.WriteDone()
}

// setWriteDeadline bounds the next write by WriteTimeout. Writers that do
// not support deadlines (such as test recorders) are left unbounded.
func (e *exchange) setWriteDeadline() {
	e.hasDeadline = true
	_ = e.rc.SetWriteDeadline(time.Now().Add(e.opts.WriteTimeout))
}

// finish clears the write deadline, which would otherwise carry over to
// the next request on a kept-alive connection.
func (e *exchange) finish() {
	if e.hasDeadline {
		_ = e.rc.SetWriteDeadline(time.Time{})
	}
}

// setInfo sets the X-Switchyard-* headers that are known.
func setInfo(h http.Header, info gateway.Info) {
	if info.Model != "" {
		h.Set(headerModel, info.Model)
	}
	if info.Provider != "" {
		h.Set(headerProvider, info.Provider)
	}
	if info.Alias != "" {
		h.Set(headerAlias, info.Alias)
	}
	if len(info.DroppedParams) > 0 {
		h.Set(headerDroppedParams, strings.Join(info.DroppedParams, ", "))
	}
	if info.Shim != "" {
		h.Set(headerShim, info.Shim)
	}
	if info.UpstreamRequestID != "" {
		h.Set(headerUpstreamRequestID, info.UpstreamRequestID)
	}
	if info.Attempts > 0 {
		h.Set(headerAttempts, strconv.Itoa(info.Attempts))
	}
	if info.RouteReason != "" {
		h.Set(headerRouteReason, info.RouteReason)
	}
}

// writeJSON sends body as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	// A write error means the client has gone away.
	_, _ = w.Write(body)
}
