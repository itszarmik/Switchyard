package anthropic

import (
	"context"
	"errors"
	"fmt"
	"time"

	"switchyard/internal/providers"
)

// errCallDone cancels a call's context once the call is over.
var errCallDone = errors.New("anthropic: upstream call finished")

// timeoutError is the cancellation cause of a call that hit a timeout.
type timeoutError struct {
	name string // first_token, idle or total
	d    time.Duration
}

func (e *timeoutError) Error() string {
	switch e.name {
	case "first_token":
		return fmt.Sprintf("upstream sent no first token within %s (first_token timeout)", e.d)
	case "idle":
		return fmt.Sprintf("upstream stream was idle for %s (idle timeout)", e.d)
	default:
		return fmt.Sprintf("upstream call exceeded %s (%s timeout)", e.d, e.name)
	}
}

// call is the state of one upstream request: a context that the timeouts
// cancel with a *timeoutError cause, and the timers that do it. It mirrors
// the openai adapter's.
type call struct {
	a      *Adapter
	parent context.Context
	ctx    context.Context
	cancel context.CancelCauseFunc
	start  time.Time
	// phase is the first_token timer until the first event, then the idle
	// timer, re-armed after every event.
	phase  *deadline
	total  *deadline
	inIdle bool
}

func (a *Adapter) newCall(parent context.Context) *call {
	ctx, cancel := context.WithCancelCause(parent)
	return &call{
		a:      a,
		parent: parent,
		ctx:    ctx,
		cancel: cancel,
		start:  time.Now(),
		phase:  startDeadline(a.timeouts.firstToken, "first_token", cancel),
		total:  startDeadline(a.timeouts.total, "total", cancel),
	}
}

// eventArrived switches from the first_token timeout to the idle timeout,
// or re-arms the idle timeout.
func (c *call) eventArrived() {
	if c.inIdle {
		c.phase.reset()
		return
	}
	c.inIdle = true
	c.phase.Stop()
	c.phase = startDeadline(c.a.timeouts.idle, "idle", c.cancel)
}

// keepAlive re-arms the idle timeout once the first event has arrived. It
// is what a ping does: proof that the upstream is alive, which Anthropic
// sends while the model works without output (thinking, for example), but
// never a first token.
func (c *call) keepAlive() {
	if c.inIdle {
		c.phase.reset()
	}
}

// stop ends the call: it stops the timers and cancels the context, which
// aborts the upstream request if it is still running.
func (c *call) stop() {
	c.phase.Stop()
	c.total.Stop()
	c.cancel(errCallDone)
}

// classify turns an error from the HTTP client or the body into a
// *providers.Error, telling timeouts and client cancellation apart from
// connection failures.
func (c *call) classify(err error, rid string) *providers.Error {
	e := &providers.Error{Provider: c.a.id, UpstreamRequestID: rid, Err: err}
	var te *timeoutError
	switch {
	case errors.As(context.Cause(c.ctx), &te):
		e.Kind = providers.KindTimeout
		e.Message = te.Error()
	case c.parent.Err() != nil:
		e.Kind = providers.KindCanceled
		e.Message = "the request was canceled before the upstream answered"
	default:
		e.Kind = providers.KindTransport
		e.Message = "connection to the upstream failed"
	}
	return e
}

// deadline cancels a call when it expires. A nil *deadline (timeout
// disabled) is valid and does nothing.
type deadline struct {
	t *time.Timer
	d time.Duration
}

func startDeadline(d time.Duration, name string, cancel context.CancelCauseFunc) *deadline {
	if d <= 0 {
		return nil
	}
	cause := &timeoutError{name: name, d: d}
	return &deadline{t: time.AfterFunc(d, func() { cancel(cause) }), d: d}
}

// Stop disarms the deadline.
func (dl *deadline) Stop() {
	if dl != nil {
		dl.t.Stop()
	}
}

// reset re-arms the deadline for its full duration from now.
func (dl *deadline) reset() {
	if dl != nil {
		dl.t.Reset(dl.d)
	}
}
