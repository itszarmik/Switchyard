package ratelimit

import (
	"context"
	"errors"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/auth"
	"switchyard/internal/reqctx"
	"switchyard/internal/server"
	"switchyard/internal/tracing"
)

// Labels of switchyard_ratelimit_decisions_total.
const (
	kindRequest   = "request"
	kindStream    = "stream"
	resultAllowed = "allowed"
	resultLimited = "limited"
	resultError   = "error"
)

// EnforcerOptions configures NewEnforcer.
type EnforcerOptions struct {
	// Limiter decides; usually a Circuit around a RedisLimiter, or a
	// MemoryLimiter. Required.
	Limiter Limiter
	// Limits returns the limits of the current config snapshot. It is
	// called once per request, so a reload that swaps what it returns
	// changes the limits at once. Required.
	Limits func() *Limits
	// Registerer registers switchyard_ratelimit_decisions_total. Required.
	Registerer prometheus.Registerer
}

// Enforcer applies the per-key limits to requests: the request rate in
// Middleware, and the concurrent-stream cap in AcquireStream. It counts
// every decision in switchyard_ratelimit_decisions_total{kind, result}, and
// applies rate_limits.fail_mode when the limiter fails. It is safe for
// concurrent use.
type Enforcer struct {
	limiter Limiter
	limits  func() *Limits
	// The children of switchyard_ratelimit_decisions_total.
	requestAllowed, requestLimited, requestError prometheus.Counter
	streamAllowed, streamLimited, streamError    prometheus.Counter
}

// NewEnforcer returns an Enforcer, registering its metric with
// o.Registerer.
func NewEnforcer(o EnforcerOptions) (*Enforcer, error) {
	if o.Limiter == nil || o.Limits == nil || o.Registerer == nil {
		return nil, errors.New("ratelimit: NewEnforcer needs a Limiter, Limits and a Registerer")
	}
	decisions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_ratelimit_decisions_total",
		Help: "Per-key rate-limit decisions, by kind (request: the request rate; stream: the concurrent-stream cap) " +
			"and result (allowed, limited, or error when the store failed and rate_limits.fail_mode applied).",
	}, []string{"kind", "result"})
	if err := o.Registerer.Register(decisions); err != nil {
		return nil, err
	}
	return &Enforcer{
		limiter:        o.Limiter,
		limits:         o.Limits,
		requestAllowed: decisions.WithLabelValues(kindRequest, resultAllowed),
		requestLimited: decisions.WithLabelValues(kindRequest, resultLimited),
		requestError:   decisions.WithLabelValues(kindRequest, resultError),
		streamAllowed:  decisions.WithLabelValues(kindStream, resultAllowed),
		streamLimited:  decisions.WithLabelValues(kindStream, resultLimited),
		streamError:    decisions.WithLabelValues(kindStream, resultError),
	}, nil
}

// streamCap is what AcquireStream needs to know about a request, stored in
// its context by Middleware when its key caps concurrent streams.
type streamCap struct {
	keyID    string
	max      int
	failOpen bool
}

// streamCapKey is the context key of the request's streamCap.
type streamCapKey struct{}

// Middleware charges every request to its key's request rate. It must run
// after authentication (auth.Middleware), whose Principal names the key;
// a request without one passes untouched.
//
// A key whose effective limit (auth.Principal.RateLimit) has a request
// rate is charged one request, whatever happens to the request afterwards: a cache
// hit counts like any other. Over the rate, the request gets 429
// rate_limit_exceeded with Retry-After in whole seconds, rounded up. A key
// without limits costs one map lookup and no store call.
//
// When the limiter fails, rate_limits.fail_mode open serves the request,
// and closed answers 503 rate_limit_unavailable. The failure is logged at
// WARN, except while a Circuit skips the limiter after a failure, which was
// logged already. A call that fails because the client went away (the
// request's context ended) is not counted, and the request ends there.
//
// When the key caps concurrent streams, the request's context carries the
// cap, for AcquireStream.
//
// The store call is traced as a tracing.SpanRateLimit span, a child of
// the request's span; the span ends before next runs.
func (e *Enforcer) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		p, ok := auth.FromContext(ctx)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		lim := p.RateLimit
		if lim.RequestsPerMinute <= 0 && lim.MaxConcurrentStreams <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		limits := e.limits()
		if lim.RequestsPerMinute > 0 {
			sctx, span := tracing.Start(ctx, tracing.SpanRateLimit)
			allowed, wait, err := e.limiter.Allow(sctx, p.KeyID, lim)
			span.End()
			switch {
			case err != nil && ctx.Err() != nil:
				// The client went away during the call: neither a store
				// failure nor a decision. 499 is the access log's status;
				// nobody reads it.
				w.WriteHeader(statusClientClosedRequest)
				return
			case err != nil:
				e.requestError.Inc()
				if !applyFailMode(ctx, limits.FailOpen(), err) {
					server.AbandonBody(w, r)
					StoreUnavailable().Write(w)
					return
				}
			case !allowed:
				e.requestLimited.Inc()
				server.AbandonBody(w, r)
				RequestLimited(wait).Write(w)
				return
			default:
				e.requestAllowed.Inc()
			}
		}
		if lim.MaxConcurrentStreams > 0 {
			ctx = context.WithValue(ctx, streamCapKey{}, &streamCap{
				keyID: p.KeyID, max: lim.MaxConcurrentStreams, failOpen: limits.FailOpen(),
			})
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// statusClientClosedRequest is the status recorded for a request whose
// client went away during the limiter call (nginx's convention, as the
// gateway's).
const statusClientClosedRequest = 499

// AcquireStream takes a concurrent-stream slot for the request whose
// context is ctx, once it is known to stream. It returns the function that
// gives the slot back, to call when the stream ends; it is never nil and
// may be called more than once. A request whose key has no stream cap, or
// that did not pass through Middleware, acquires nothing.
//
// When the key already has its maximum of streams open, it returns the 429
// rate_limit_exceeded rejection, with Retry-After 1 s. When the limiter
// fails, rate_limits.fail_mode open serves the stream without a slot, and
// closed returns the 503 rate_limit_unavailable rejection. The fail mode
// is the one Middleware saw, so a request uses one snapshot throughout.
func (e *Enforcer) AcquireStream(ctx context.Context) (release func(), rej *Rejection) {
	sc, _ := ctx.Value(streamCapKey{}).(*streamCap)
	if sc == nil {
		return noRelease, nil
	}
	release, ok, err := e.limiter.AcquireStream(ctx, sc.keyID, sc.max)
	switch {
	case err != nil && ctx.Err() != nil:
		// The client went away during the call: not a store failure.
		return noRelease, StoreUnavailable()
	case err != nil:
		e.streamError.Inc()
		if !applyFailMode(ctx, sc.failOpen, err) {
			return noRelease, StoreUnavailable()
		}
		return noRelease, nil
	case !ok:
		e.streamLimited.Inc()
		return noRelease, StreamLimited(sc.max)
	}
	e.streamAllowed.Inc()
	return release, nil
}

// applyFailMode logs the limiter failure err and returns open, whether
// the request is served anyway. A call the Circuit skipped is not logged,
// nor one whose request has ended.
func applyFailMode(ctx context.Context, open bool, err error) bool {
	if errors.Is(err, ErrCircuitOpen) || ctx.Err() != nil {
		return open
	}
	msg := "rate-limit store unavailable; serving the request without its rate limit (rate_limits.fail_mode open)"
	if !open {
		msg = "rate-limit store unavailable; rejecting the request (rate_limits.fail_mode closed)"
	}
	reqctx.Logger(ctx).WarnContext(ctx, msg, "error", err)
	return open
}
