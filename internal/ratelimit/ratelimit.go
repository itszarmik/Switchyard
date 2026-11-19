// Package ratelimit enforces per-key request rates and concurrent-stream
// caps (DESIGN §7 step 3, §17), and limits failed authentications per
// client address.
//
// # Request rate
//
// The request rate uses GCRA, the generic cell rate algorithm. A key
// limited to rpm requests per minute with burst b has an emission interval
// T = 1 min / rpm and a tolerance τ = b·T. The limiter keeps one number per
// key, its theoretical arrival time (TAT). A request at time now is allowed
// when max(TAT, now) − τ ≤ now, and then moves TAT to max(TAT, now) + T. So
// an idle key may send b+1 requests at once (b on top of the sustained
// rate, as config.RateLimit.Burst says), and then one every T. A refused
// request is not counted; it is told to retry after max(TAT, now) − τ − now,
// when the next request would be allowed. A TAT more than τ + T ahead,
// left by a slower limit before a reload, is clamped to now + τ + T, so a
// raised limit applies within one new interval.
//
// # Concurrent streams
//
// A stream takes a lease when the request is known to stream and gives it
// back when the stream ends. With Redis, a lease expires unless its holder
// renews it, so a replica that crashes cannot leak slots for longer than
// the lease TTL.
//
// # Stores
//
// MemoryLimiter keeps the state in the process, for one replica.
// RedisLimiter keeps it in the state Redis, shared by every replica; each
// decision is one atomic Lua script that reads the Redis server's clock,
// never the replica's, so replicas agree. Circuit wraps either, so that a
// store that is down is not asked on every request.
//
// # Enforcement
//
// Enforcer applies the limits: its Middleware charges every authenticated
// request, cache hits included, right after authentication, and its
// AcquireStream takes the stream lease once the gateway has decoded a
// streaming request. Both read the limit of the request's key from its
// auth.Principal, and the fail mode from the current Limits; both are
// rebuilt on every config reload, so a reload changes limits at once. When the store fails, rate_limits.fail_mode
// decides: open serves the request, closed answers 503.
//
// AuthFailures limits failed authentications per client address, in the
// memory of each replica (not shared through Redis).
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"switchyard/internal/apierror"
	"switchyard/internal/config"
)

// Limiter decides whether a key may make a request or open a stream.
// Implementations are safe for concurrent use.
type Limiter interface {
	// Allow charges one request to keyID under lim. When ok is false
	// nothing is charged, and retryAfter is the wait until the next
	// request would be allowed. A lim without a request rate allows
	// every request without asking the store. Errors wrap ErrUnavailable,
	// or the context's error when ctx ended.
	Allow(ctx context.Context, keyID string, lim config.RateLimit) (ok bool, retryAfter time.Duration, err error)
	// AcquireStream takes one of keyID's maxStreams concurrent-stream
	// slots.
	// When ok is true, release gives the slot back; it must be called
	// once the stream ends, and calling it again does nothing. When ok
	// is false, or err is not nil, release is nil. A maxStreams of 0 or
	// less means no cap: ok is true and nothing is acquired.
	AcquireStream(ctx context.Context, keyID string, maxStreams int) (release func(), ok bool, err error)
}

// ErrUnavailable is returned, wrapped, when the store failed; the caller
// applies rate_limits.fail_mode.
var ErrUnavailable = errors.New("rate-limit store unavailable")

// unavailable wraps a store failure in op. A canceled or expired ctx is
// reported as such rather than as ErrUnavailable.
func unavailable(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("ratelimit %s: %w", op, ctxErr)
	}
	return fmt.Errorf("ratelimit %s: %w: %w", op, ErrUnavailable, err)
}

// noRelease is the release of a stream that acquired nothing.
func noRelease() {}

// gcra returns the emission interval and the tolerance of lim, which must
// have a request rate. Values above the config maximums are clamped to
// them, so the arithmetic cannot overflow for a hand-built config. The
// interval is rounded up to a whole microsecond, the Redis script's unit,
// so both stores enforce the same rate and never a faster one.
func gcra(lim config.RateLimit) (interval, tolerance time.Duration) {
	rpm := min(lim.RequestsPerMinute, config.MaxRequestsPerMinute)
	burst := min(max(lim.Burst, 0), config.MaxRateLimitBurst)
	us := (time.Minute.Microseconds() + int64(rpm) - 1) / int64(rpm)
	interval = time.Duration(us) * time.Microsecond
	return interval, time.Duration(burst) * interval
}

// Limits holds the gateway-wide rate-limit settings of one config
// snapshot: rate_limits.fail_mode and rate_limits.auth_failures_per_minute.
// A key's own limit is not here: it is auth.Principal.RateLimit, resolved
// from the same snapshot's key store, so there is one source of truth.
// Limits is immutable and safe for concurrent use; a nil *Limits fails
// open and does not limit authentication failures.
type Limits struct {
	failOpen     bool
	authFailures int
}

// NewLimits returns the gateway-wide limits of c, which config.Load has
// validated and filled with defaults.
func NewLimits(c *config.Config) *Limits {
	return &Limits{
		failOpen:     c.RateLimits.FailMode != config.RateLimitFailClosed,
		authFailures: c.RateLimits.AuthFailuresPerMinute,
	}
}

// FailOpen reports whether requests are served while the store is
// unavailable (rate_limits.fail_mode open).
func (l *Limits) FailOpen() bool { return l == nil || l.failOpen }

// AuthFailuresPerMinute returns rate_limits.auth_failures_per_minute; 0 or
// less means the limit is off.
func (l *Limits) AuthFailuresPerMinute() int {
	if l == nil {
		return 0
	}
	return l.authFailures
}

// Error envelope fields of the rejections.
const (
	// typeRequests is OpenAI's error type for request-rate 429s.
	typeRequests = "requests"
	// CodeRateLimited is the error code of every 429 the limits send.
	CodeRateLimited = "rate_limit_exceeded"
	// CodeUnavailable is the error code of the 503 sent when the store is
	// unavailable and rate_limits.fail_mode is closed.
	CodeUnavailable = "rate_limit_unavailable"
	// StreamRetryAfter is the Retry-After of a request refused for the
	// concurrent-stream cap, which has no natural wait.
	StreamRetryAfter = time.Second
)

// Rejection is the answer to a request a limit refuses, in the fields of
// the OpenAI error envelope. The gateway maps it to its own error type;
// Write sends it directly.
type Rejection struct {
	// Status is the HTTP status: 429, or 503 when the store failed and
	// fail_mode is closed.
	Status int
	// Type, Code and Message are the envelope fields.
	Type, Code, Message string
	// RetryAfter, when positive, is sent as the Retry-After header in
	// whole seconds, rounded up.
	RetryAfter time.Duration
}

// Error returns the message.
func (r *Rejection) Error() string { return r.Message }

// Write sends the rejection: the Retry-After header, if any, and the
// OpenAI error envelope.
func (r *Rejection) Write(w http.ResponseWriter) {
	if r.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(RetryAfterSeconds(r.RetryAfter), 10))
	}
	apierror.Write(w, r.Status, r.Type, r.Code, r.Message)
}

// RetryAfterSeconds returns d in whole seconds, rounded up, and at least 1.
func RetryAfterSeconds(d time.Duration) int64 {
	return max(int64((d+time.Second-1)/time.Second), 1)
}

// RequestLimited is the rejection of a request over its key's request
// rate, to be retried after retryAfter.
func RequestLimited(retryAfter time.Duration) *Rejection {
	return &Rejection{
		Status: http.StatusTooManyRequests, Type: typeRequests, Code: CodeRateLimited, RetryAfter: retryAfter,
		Message: fmt.Sprintf("Rate limit reached for requests on this API key. Please retry in %ds.", RetryAfterSeconds(retryAfter)),
	}
}

// StreamLimited is the rejection of a streaming request whose key already
// has maxStreams streams open.
func StreamLimited(maxStreams int) *Rejection {
	return &Rejection{
		Status: http.StatusTooManyRequests, Type: typeRequests, Code: CodeRateLimited, RetryAfter: StreamRetryAfter,
		Message: fmt.Sprintf("Too many concurrent streams on this API key: the limit is %d. Please retry when one has finished.", maxStreams),
	}
}

// AuthLimited is the rejection of a request from a client address with too
// many failed authentications, to be retried after retryAfter.
func AuthLimited(retryAfter time.Duration) *Rejection {
	return &Rejection{
		Status: http.StatusTooManyRequests, Type: typeRequests, Code: CodeRateLimited, RetryAfter: retryAfter,
		Message: fmt.Sprintf("Too many failed authentication attempts from this address. Please retry in %ds.", RetryAfterSeconds(retryAfter)),
	}
}

// StoreUnavailable is the rejection of a request whose limit could not be
// checked because the store failed, under rate_limits.fail_mode closed.
func StoreUnavailable() *Rejection {
	return &Rejection{
		Status: http.StatusServiceUnavailable, Type: apierror.TypeServer, Code: CodeUnavailable,
		Message: "The rate-limit store is unavailable, so the request cannot be checked against its rate limit. Please retry later.",
	}
}

// WriteRateLimited sends the 429 rate_limit_exceeded answer to a request
// over its key's request rate, with Retry-After set to retryAfter in whole
// seconds, rounded up.
func WriteRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	RequestLimited(retryAfter).Write(w)
}
