package gateway

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/health"
	"switchyard/internal/providers"
	"switchyard/internal/reqctx"
	"switchyard/internal/schema"
)

// Route reasons, for the X-Switchyard-Route-Reason header and the logs.
const (
	reasonCheapest = "cheapest_eligible"
	reasonPinned   = "pinned"
	reasonFallback = "fallback:"
)

// maxChain caps the attempt chain in the access log.
const maxChain = 10

// attempt is the result of one upstream attempt.
type attempt struct {
	// kind is "" for success, else the failure's kind; for a committed
	// stream, the kind of its end (canceled when the client went away).
	kind providers.Kind
	// err is the failure, if any.
	err error
	// committed is set once the client has been answered: a complete
	// response, or a stream that has started. Nothing is retried after
	// it.
	committed bool
	ttft      time.Duration
	usage     *schema.Usage
}

// tryFunc makes one attempt of a request on plan entry e and, when the
// upstream answers with something the client gets, answers the client.
// It sets the provider-specific fields of info (Provider, DroppedParams,
// Shim, UpstreamRequestID) before answering; the executor has set the
// others.
type tryFunc func(ctx context.Context, e *planEntry, info *Info) attempt

// requestTimeout returns the request's deadline: routing.request_timeout,
// lowered by the client's X-Switchyard-Timeout-Ms header if it is shorter.
func (g *Gateway) requestTimeout(h http.Header) (time.Duration, *Error) {
	d := g.routing.RequestTimeout.Std()
	v := h.Get(HeaderTimeoutMs)
	if v == "" {
		return d, nil
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || ms <= 0 {
		return 0, invalidTimeout()
	}
	if ms < d.Milliseconds() {
		d = time.Duration(ms) * time.Millisecond
	}
	return d, nil
}

// withDeadline sets the request's deadline from its headers. When the
// header is unusable it answers the client and returns false.
func (g *Gateway) withDeadline(ctx context.Context, ex Exchange, rec *record) (context.Context, context.CancelFunc, bool) {
	timeout, e := g.requestTimeout(ex.Header())
	if e != nil {
		rec.respondError(ex, Info{}, e)
		return ctx, func() {}, false
	}
	rec.deadline = timeout
	ctx, cancel := context.WithDeadline(ctx, rec.start.Add(timeout))
	return ctx, cancel, true
}

// execute runs the plan: for each entry, up to 1 + retries_per_deployment
// attempts, then the next entry, until one answers the client.
//
// Before each attempt the deadline is checked, the provider's concurrency
// slot taken and the breaker asked; a deployment that is saturated or
// whose breaker refuses is passed over. A retry needs a retryable failure
// (see providers.Retryable: a non-streaming call's total timeout is not
// retried), room in the deployment's retry budget, and a wait that fits
// the remaining deadline: the upstream's Retry-After for a 429, else
// exponential backoff with full jitter. The retry is taken from the budget
// and counted only once the wait is over, so a client that goes away
// during it spends nothing. A failure that does not fall back (a 400, a
// content filter, a cancellation) is answered at once. Every attempt is
// recorded in the health tracker and the metrics.
//
// When every attempt failed, the answer is the most informative failure,
// with the headers (provider, upstream request id) of the attempt that
// failed so: 504 if the deadline expired, 429 if every failure was a rate
// limit, else the mapping of upstreamError for the best one (see
// failureRank), and 503 no_healthy_upstream if no attempt could be made
// at all.
//
// First, the request is admitted against its budgets (see admitBudget),
// and before an attempt on a deployment whose estimate is higher than the
// charge so far, the difference is admitted (see chargeFallback); a
// deployment the budgets have no room for is passed over.
//
// The plan is rec.plan, and the response headers start from rec.info.
func (g *Gateway) execute(ctx context.Context, ex Exchange, rec *record, try tryFunc) {
	if !g.admitBudget(ctx, ex, rec) {
		return
	}
	p, info := &rec.plan, &rec.info
	var (
		last      *planEntry // the deployment of the last failure
		lastErr   error
		best      failure
		bestRank  = math.MinInt
		failures  int
		limited   int // rate-limited failures
		lastLimit failure
		skipped   []catalog.Exclusion
	)
	retries := g.routing.Fallback.RetriesPerDeployment
	for i := range p.entries {
		e := &p.entries[i]
		for n := 0; n <= retries; n++ {
			retry := n > 0
			if retry {
				wait, ok := g.retryWait(ctx, lastErr, n-1)
				if !ok || !e.health.CanRetry() || !g.sleep(ctx, rec, wait) || !e.health.TryRetry() {
					break
				}
				g.metrics.retries.WithLabelValues(e.d.ID, providers.Outcome(lastErr)).Inc()
			}
			if ctx.Err() != nil {
				g.answerFailure(ctx, ex, rec, failureState{})
				return
			}
			if reason := g.chargeFallback(ctx, rec, e); reason != "" {
				skipped = append(skipped, catalog.Exclusion{Deployment: e.d.ID, Reason: reason})
				break
			}
			if !e.prov.Acquire() {
				skipped = append(skipped, catalog.Exclusion{Deployment: e.d.ID, Reason: "provider at max_concurrency " + strconv.Itoa(e.prov.Max())})
				break
			}
			adm, reason := e.health.Admit(retry)
			if reason != "" {
				e.prov.Release()
				skipped = append(skipped, catalog.Exclusion{Deployment: e.d.ID, Reason: reason})
				break
			}
			if last != nil && last != e {
				kind := providers.Outcome(lastErr)
				g.metrics.fallbacks.WithLabelValues(last.d.ID, e.d.ID, kind).Inc()
				if rec.fallbackReason == "" {
					rec.fallbackReason = kind
				}
			}
			rec.attempts++
			info.Attempts = rec.attempts
			// A cross-model fallback is served by another model.
			info.Model = e.d.Model.ID
			info.RouteReason = rec.routeReason(p)
			rec.deployment = e.d

			a := g.runAttempt(ctx, rec, e, adm, info, try)
			if a.committed || a.kind == "" {
				return
			}

			last, lastErr = e, a.err
			failures++
			f := failure{err: a.err, info: *info}
			if r := failureRank(a.kind); r >= bestRank {
				best, bestRank = f, r
			}
			if a.kind == providers.KindRateLimited {
				limited++
				lastLimit = f
			}
			if a.kind == providers.KindAuth {
				reqctx.Logger(ctx).ErrorContext(ctx, "upstream rejected the provider credential",
					"provider", e.d.Provider, "deployment", e.d.ID, "error", upstreamMessage(a.err))
			}
			if !a.kind.FallsBack() {
				rec.respondError(ex, *info, upstreamError(a.err))
				return
			}
			if !providers.Retryable(a.err, rec.stream) {
				break
			}
		}
	}
	g.answerFailure(ctx, ex, rec, failureState{
		failures: failures, best: best, allLimited: failures > 0 && limited == failures,
		lastLimit: lastLimit, skipped: skipped,
	})
}

// failure is a failed attempt: its error, and the response headers it
// would have been answered with.
type failure struct {
	err  error
	info Info
}

// failureState is what execute knows when no attempt answered.
type failureState struct {
	failures   int
	best       failure
	allLimited bool
	lastLimit  failure
	skipped    []catalog.Exclusion
}

// answerFailure answers a request that no attempt answered. When the
// answer is one attempt's failure, the provider, upstream request id,
// dropped parameters and shim headers are that attempt's; the attempt
// count and route reason are the request's.
func (g *Gateway) answerFailure(ctx context.Context, ex Exchange, rec *record, f failureState) {
	var (
		e      *Error
		answer *failure
	)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		e = deadlineExceeded(rec.deadline)
	case ctx.Err() != nil:
		e = upstreamError(&providers.Error{Kind: providers.KindCanceled})
		rec.errorCode = codeCanceled
	case f.failures == 0:
		e = noHealthyUpstream(rec.plan.model, f.skipped)
	case f.allLimited:
		answer = &f.lastLimit
	default:
		answer = &f.best
	}
	info := rec.info
	if answer != nil {
		e = upstreamError(answer.err)
		info = answer.info
		info.Attempts, info.RouteReason = rec.info.Attempts, rec.info.RouteReason
		rec.upstreamFailed(answer.err)
	}
	rec.respondError(ex, info, e)
}

// failureRank orders failure kinds by how much their error tells the
// client, for the answer when every attempt failed. Later failures win
// ties.
//
// An adapter's local rejection (unsupported) ranks below every other
// kind: it is the answer only when every attempt was rejected so. Once
// another deployment has tried the request, its failure explains the
// outcome better than a 400 that says the request itself is wrong when
// another provider would have accepted it.
func failureRank(k providers.Kind) int {
	switch k {
	case providers.KindContextLength:
		return 6
	case providers.KindNotFound:
		return 5
	case providers.KindServerError:
		return 4
	case providers.KindTimeout:
		return 3
	case providers.KindAuth:
		return 2
	case providers.KindTransport:
		return 1
	case providers.KindUnsupported:
		return -1
	}
	return 0
}

// retryWait returns how long to wait before retry n (0 for the first) of
// a failure err, and false when the failure may not be retried within the
// deadline: a 429 waits for its Retry-After, which must fit the remaining
// time (a 429 without one is not retried); anything else waits a full
// jitter backoff, which must fit too.
func (g *Gateway) retryWait(ctx context.Context, err error, n int) (time.Duration, bool) {
	remaining := time.Duration(1<<63 - 1)
	if dl, ok := ctx.Deadline(); ok {
		remaining = time.Until(dl)
	}
	var pe *providers.Error
	if errors.As(err, &pe) && pe.Kind == providers.KindRateLimited {
		if pe.RetryAfter <= 0 || pe.RetryAfter >= remaining {
			return 0, false
		}
		return pe.RetryAfter, true
	}
	w := g.jitter(backoffCap(g.routing.Fallback.BackoffBase.Std(), g.routing.Fallback.BackoffMax.Std(), n))
	if w >= remaining {
		return 0, false
	}
	return w, true
}

// backoffCap is the upper bound of the full-jitter sleep before retry n:
// min(max, base×2^n).
func backoffCap(base, maxWait time.Duration, n int) time.Duration {
	d := base
	for range n {
		if d >= maxWait {
			break
		}
		d *= 2
	}
	return min(d, maxWait)
}

// sleep waits for d, or until ctx is done, and reports whether it waited
// the whole time. The wait counts as neither overhead nor upstream time.
func (g *Gateway) sleep(ctx context.Context, rec *record, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	start := time.Now()
	t := time.NewTimer(d)
	defer t.Stop()
	defer func() { rec.backoff += time.Since(start) }()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// runAttempt makes one admitted attempt on e, which holds a concurrency
// slot of its provider, and records it. If try panics, the slot is
// released and the admission ended, as neutral for the breaker, before the
// panic goes on: neither may leak.
//
// It also notes whether the attempt reached the upstream (see sentTrace)
// and may have been billed without serving the request (see billable):
// such a request is charged its estimate (see record.account).
func (g *Gateway) runAttempt(ctx context.Context, rec *record, e *planEntry, adm health.Admission, info *Info, try tryFunc) attempt {
	rec.startAttempt(e.health, adm)
	ended := false
	defer func() {
		if !ended {
			e.prov.Release()
			rec.reportHealth(health.Result{Kind: providers.KindCanceled})
		}
	}()
	var sent atomic.Bool
	start := time.Now()
	a := try(sentTrace(ctx, &sent), e, info)
	dur := time.Since(start)
	ended = true
	e.prov.Release()
	if !rec.served && sent.Load() && billable(a.kind) {
		rec.sentTo = e.d
	}
	g.recordAttempt(ctx, rec, e, a, dur)
	return a
}

// sentTrace returns ctx with an HTTP client trace that sets sent once the
// upstream request has been written in full, or the upstream has begun to
// answer: from then on the upstream may process, and bill, the request
// whatever happens to the attempt. A request that failed before, such as
// on a dial failure, never reached it.
func sentTrace(ctx context.Context, sent *atomic.Bool) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err == nil {
				sent.Store(true)
			}
		},
		GotFirstResponseByte: func() { sent.Store(true) },
	})
}

// billable reports whether an attempt that reached the upstream and ended
// with kind, without serving the request, may have been billed: anything
// but an upstream refusal (a 4xx answer), which is not, and the adapter's
// own rejection, which never reaches the upstream. A cancellation, a
// timeout, a broken connection and a 5xx may all come after the upstream
// did the work.
func billable(k providers.Kind) bool {
	switch k {
	case providers.KindClientError, providers.KindAuth, providers.KindNotFound, providers.KindContextLength,
		providers.KindContentFilter, providers.KindRateLimited, providers.KindUnsupported:
		return false
	}
	return true
}

// recordAttempt records one attempt in the health tracker, the metrics and
// the request's record.
//
// A half-open probe ends at the attempt's commit point, when the upstream
// has answered (see responseAttempt and streamAttempt), so a probe that is
// a long stream closes the breaker at once. The attempt's outcome is
// recorded here, when it ends: a failure before the commit point ends its
// admission (health.Deployment.Done); after it, health.Deployment.Finish
// records a success, or an upstream failure mid-stream (an idle timeout,
// a dropped connection, an error event), which counts toward the breaker
// as any failure does.
//
// Some failures are not the deployment's fault and are recorded as
// neutral (canceled): a timeout or connection failure caused by the
// request's own deadline or by the client going away, and, after the
// commit point, the provider's total timeout, which is the request's own
// budget running out. A client that went away or stopped reading
// (server.write_timeout) is canceled already.
func (g *Gateway) recordAttempt(ctx context.Context, rec *record, e *planEntry, a attempt, dur time.Duration) {
	healthKind := a.kind
	switch {
	case ctx.Err() != nil && (a.kind == providers.KindTimeout || a.kind == providers.KindTransport):
		healthKind = providers.KindCanceled
	case rec.reported && a.kind == providers.KindTimeout && isTotalTimeout(a.err):
		healthKind = providers.KindCanceled
	}
	res := health.Result{Kind: healthKind}
	if a.kind == providers.KindRateLimited {
		var pe *providers.Error
		if errors.As(a.err, &pe) {
			res.RetryAfter = pe.RetryAfter
		}
	}
	outcome := string(a.kind)
	if a.kind == "" {
		outcome = providers.OutcomeOK
		res.TTFT = a.ttft
		if a.ttft > 0 && a.usage != nil && a.usage.CompletionTokens > 0 && dur > a.ttft {
			res.TokensPerSec = float64(a.usage.CompletionTokens) / (dur - a.ttft).Seconds()
		}
	}
	if rec.reported {
		e.health.Finish(res)
	} else {
		rec.reportHealth(res)
	}
	m := g.metrics
	d := e.d
	m.upstreamRequests.WithLabelValues(d.Provider, d.Model.ID, outcome).Inc()
	m.upstreamDuration.WithLabelValues(d.Provider, d.Model.ID).Observe(dur.Seconds())
	if a.kind == "" && a.ttft > 0 {
		m.upstreamTTFT.WithLabelValues(d.Provider, d.Model.ID).Observe(a.ttft.Seconds())
	}
	rec.addChain(d.ID, outcome)
}

// isTotalTimeout reports whether err is a provider's total timeout.
func isTotalTimeout(err error) bool {
	var pe *providers.Error
	return errors.As(err, &pe) && pe.Timeout == providers.TimeoutTotal
}

// upstreamMessage is the loggable message of an adapter error.
func upstreamMessage(err error) string {
	var pe *providers.Error
	if errors.As(err, &pe) {
		return pe.Message
	}
	return "connection to the upstream failed"
}

// responseAttempt finishes a non-streaming attempt that started at start:
// a failure is returned for the executor to handle, and a success is
// committed in the deployment's health, the attempt's commit point (see
// recordAttempt), then answered (translated by conv, if set).
func (g *Gateway) responseAttempt(ex Exchange, rec *record, start time.Time, resp *providers.Response, err error, info *Info, conv converter) attempt {
	rec.upstreamWait += time.Since(start)
	if err != nil {
		rec.upstreamFailed(err)
		info.UpstreamRequestID = rec.upstreamRequestID
		return attempt{kind: outcome(err), err: err}
	}
	rec.commitHealth(resp.TTFT)
	rec.served = true
	rec.ttft = resp.TTFT
	rec.usage = resp.Usage
	rec.upstreamRequestID = resp.UpstreamRequestID
	rec.errorMessage = ""
	info.UpstreamRequestID = resp.UpstreamRequestID
	a := attempt{committed: true, ttft: resp.TTFT, usage: resp.Usage}
	body := resp.Body
	if resp.Usage == nil {
		rec.outBytes = responseOutputBytes(body)
	}
	if conv != nil {
		out, cerr := conv.response(body)
		if cerr != nil {
			rec.errorMessage = "could not translate the upstream response"
			rec.respondError(ex, *info, badUpstreamResponse())
			return a
		}
		body = out
	}
	info.CostUSD = rec.costHeader()
	rec.status = resp.Status
	wstart := time.Now()
	ex.Respond(*info, resp.Status, body)
	rec.wroteClient(wstart)
	return a
}

// opener starts an upstream stream; it returns once the first event has
// arrived.
type opener func(ctx context.Context) (providers.Stream, error)

// streamAttempt makes a streaming attempt. Until the first upstream event
// nothing is sent, so a failure is returned for the executor to retry or
// fall back, unless keep-alives have already committed the response.
// The first event is the attempt's commit point, for its deployment's
// health too (see recordAttempt); from then on the stream is forwarded
// (see pump) and the attempt is committed.
func (g *Gateway) streamAttempt(ctx context.Context, ex Exchange, rec *record, info *Info, wantUsage bool, conv converter, open opener) attempt {
	start := time.Now()
	var (
		s   providers.Stream
		err error
	)
	if ka := g.routing.StreamKeepalive.Std(); ka > 0 {
		s, err = g.openWithKeepalive(ctx, ex, rec, info, ka, open)
	} else {
		s, err = open(ctx)
	}
	wait := time.Since(start)
	if err != nil {
		rec.upstreamWait += wait
		rec.upstreamFailed(err)
		info.UpstreamRequestID = rec.upstreamRequestID
		kind := outcome(err)
		if !rec.committed {
			return attempt{kind: kind, err: err}
		}
		// Keep-alives committed the response: no fallback is possible.
		if kind == providers.KindCanceled {
			rec.errorCode = codeClientGone
		} else {
			rec.errorCode = codeUpstreamError
			wstart := time.Now()
			_ = ex.Send(streamErrorEvent(err))
			rec.wroteClient(wstart)
		}
		return attempt{kind: kind, err: err, committed: true}
	}
	defer func() { _ = s.Close() }()
	// The commit point: the upstream has answered.
	rec.commitHealth(s.TTFT())
	rec.served = true
	rec.ttft = s.TTFT()
	rec.upstreamRequestID = s.UpstreamRequestID()
	rec.errorMessage = ""
	info.UpstreamRequestID = s.UpstreamRequestID()

	kind, serr := pump(ex, rec, s, *info, wantUsage, &wait, conv)
	// A stream that did not complete may have reported only part of its
	// usage, such as Anthropic's message_start (see record.account).
	rec.usage, rec.usagePartial = s.Usage(), kind != ""
	rec.upstreamWait += wait
	return attempt{kind: kind, err: serr, committed: true, ttft: s.TTFT(), usage: rec.usage}
}

// openWithKeepalive opens a stream while sending keep-alives: once the
// request has waited interval for a first event, the response is
// committed (200 and the SSE headers) and a ": keep-alive" comment is
// sent every interval until the stream opens or fails. A client that goes
// away cancels the attempt.
func (g *Gateway) openWithKeepalive(ctx context.Context, ex Exchange, rec *record, info *Info, interval time.Duration, open opener) (providers.Stream, error) {
	type result struct {
		s   providers.Stream
		err error
		// panicked is set, with the panic's value, when open panicked; the
		// panic is raised again on the handler's goroutine, for the
		// server's recovery to handle.
		panicked bool
		value    any
	}
	actx, cancel := context.WithCancel(ctx)
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			if v := recover(); v != nil {
				r = result{panicked: true, value: v}
			}
			done <- r
		}()
		r.s, r.err = open(actx)
	}()
	if rec.keepaliveDue.IsZero() {
		rec.keepaliveDue = rec.start.Add(interval)
	}
	timer := time.NewTimer(time.Until(rec.keepaliveDue))
	defer timer.Stop()
	for {
		select {
		case r := <-done:
			if r.panicked {
				cancel()
				panic(r.value)
			}
			if r.err != nil {
				cancel()
				return nil, r.err
			}
			return cancelOnClose{Stream: r.s, cancel: cancel}, nil
		case <-timer.C:
		}
		wstart := time.Now()
		err := error(nil)
		if !rec.committed {
			err = ex.StartStream(*info)
			rec.committed = true
			rec.status = http.StatusOK
		}
		if err == nil {
			err = ex.Comment("keep-alive")
		}
		rec.wroteClient(wstart)
		if err != nil {
			// The client is gone: stop the attempt.
			cancel()
			switch r := <-done; {
			case r.panicked:
				panic(r.value)
			case r.s != nil:
				_ = r.s.Close()
			}
			return nil, &providers.Error{Kind: providers.KindCanceled, Provider: info.Provider, Message: "the client went away"}
		}
		rec.keepaliveDue = rec.keepaliveDue.Add(interval)
		timer.Reset(time.Until(rec.keepaliveDue))
	}
}

// cancelOnClose is a stream whose Close also cancels its context.
type cancelOnClose struct {
	providers.Stream
	cancel context.CancelFunc
}

// Close implements providers.Stream.
func (s cancelOnClose) Close() error {
	err := s.Stream.Close()
	s.cancel()
	return err
}
