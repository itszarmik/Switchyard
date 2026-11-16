package usage

import (
	"context"
	"strconv"
	"strings"
	"sync"
)

// DefaultMemoryMaxEvents is the number of events a MemoryLedger keeps by
// default.
const DefaultMemoryMaxEvents = 100_000

// MemoryLedger is a Sink, Reporter, ShadowStore and FeedbackStore that
// keeps events, shadow evaluations and feedback in memory, for the
// quickstart and tests. It holds at most a fixed number of each and
// forgets the oldest beyond that, so reports cover only recent traffic and
// nothing survives a restart. It is safe for concurrent use.
type MemoryLedger struct {
	mu     sync.Mutex
	ring   []*Event // circular, oldest at head
	head   int
	n      int
	closed bool
	// ids counts the events in ring by tenant and ID, so that feedback
	// finds its request without scanning the ring.
	ids map[rowKey]int

	// shadows and feedback are keyed by tenant and request ID.
	shadows  boundedRows[ShadowEval]
	feedback boundedRows[Feedback]
}

// rowKey identifies a shadow evaluation or feedback: its tenant and
// request ID.
type rowKey struct{ tenant, id string }

// boundedRows holds at most max rows by key, forgetting the oldest beyond
// that.
type boundedRows[T any] struct {
	byKey map[rowKey]*T
	order []rowKey // the keys in insertion order, circular once full
	head  int      // the oldest key, once order is full
	max   int
}

// put stores v under k. An existing row is replaced when replace is set and
// kept otherwise; either way it keeps its place in the order.
func (b *boundedRows[T]) put(k rowKey, v *T, replace bool) {
	if b.byKey == nil {
		b.byKey = make(map[rowKey]*T)
	}
	if _, ok := b.byKey[k]; ok {
		if replace {
			b.byKey[k] = v
		}
		return
	}
	if len(b.order) < b.max {
		b.order = append(b.order, k)
	} else {
		delete(b.byKey, b.order[b.head])
		b.order[b.head] = k
		b.head = (b.head + 1) % b.max
	}
	b.byKey[k] = v
}

// MemoryOption configures a MemoryLedger.
type MemoryOption func(*MemoryLedger)

// WithMaxEvents sets how many events the ledger keeps; n <= 0 means
// DefaultMemoryMaxEvents.
func WithMaxEvents(n int) MemoryOption {
	return func(l *MemoryLedger) {
		if n > 0 {
			l.ring = make([]*Event, n)
		}
	}
}

// NewMemoryLedger returns an empty MemoryLedger.
func NewMemoryLedger(opts ...MemoryOption) *MemoryLedger {
	l := &MemoryLedger{}
	for _, o := range opts {
		o(l)
	}
	if l.ring == nil {
		l.ring = make([]*Event, DefaultMemoryMaxEvents)
	}
	l.ids = make(map[rowKey]int)
	l.shadows.max, l.feedback.max = len(l.ring), len(l.ring)
	return l
}

var (
	_ Sink          = (*MemoryLedger)(nil)
	_ Reporter      = (*MemoryLedger)(nil)
	_ ShadowStore   = (*MemoryLedger)(nil)
	_ FeedbackStore = (*MemoryLedger)(nil)
)

// RecordShadowEval implements ShadowStore: it keeps a copy of e, unless the
// ledger already holds an evaluation of the same tenant and request. It
// fails after Close.
func (l *MemoryLedger) RecordShadowEval(_ context.Context, e *ShadowEval) error {
	if err := checkShadowEval(e); err != nil {
		return err
	}
	c := *e
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errClosed
	}
	l.shadows.put(rowKey{c.Tenant, c.RequestID}, &c, false)
	return nil
}

// RecordFeedback implements FeedbackStore. The ledger knows every request
// it keeps, by an index of tenant and ID, so feedback on a request of f's
// tenant that it does not hold, because there is none or it has been
// forgotten, fails with ErrUnknownRequest, without a scan of the events.
// It fails after Close.
func (l *MemoryLedger) RecordFeedback(_ context.Context, f *Feedback) error {
	if err := checkFeedback(f); err != nil {
		return err
	}
	c := *f
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errClosed
	}
	if l.ids[rowKey{c.Tenant, c.RequestID}] == 0 {
		return ErrUnknownRequest
	}
	l.feedback.put(rowKey{c.Tenant, c.RequestID}, &c, true)
	return nil
}

// Record implements Sink. It never blocks for long: it only takes a mutex.
// A nil ev, and events recorded after Close, are ignored.
func (l *MemoryLedger) Record(ev *Event) {
	if ev == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.ids[rowKey{ev.Tenant, ev.ID}]++
	if l.n == len(l.ring) {
		old := l.ring[l.head]
		if k := (rowKey{old.Tenant, old.ID}); l.ids[k] <= 1 {
			delete(l.ids, k)
		} else {
			l.ids[k]--
		}
		l.ring[l.head] = ev
		l.head = (l.head + 1) % len(l.ring)
		return
	}
	l.ring[(l.head+l.n)%len(l.ring)] = ev
	l.n++
}

// Close implements Sink. It stops recording events, shadow evaluations and
// feedback; what is kept can still be reported.
func (l *MemoryLedger) Close(context.Context) error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return nil
}

// reportCheckEvery is how many events Report aggregates between checks of
// its context.
const reportCheckEvery = 1000

// Report implements Reporter. It holds the ledger's lock only to collect
// the matching events, so Record is never held up by the aggregation, and
// it stops with ctx's error once ctx is done.
func (l *MemoryLedger) Report(ctx context.Context, q Query) (*Report, error) {
	q, err := normalizeQuery(q)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	events := l.matching(q)

	var total accumulator
	groups := map[string]*accumulator{}
	keys := map[string]map[string]string{}
	values := make([]string, len(q.GroupBy))
	for i, m := range events {
		if i%reportCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		ev := m.ev
		total.addJoined(ev, m.shadow, m.feedback)
		if len(q.GroupBy) == 0 {
			continue
		}
		for j, g := range q.GroupBy {
			values[j] = groupValue(ev, g)
		}
		id := groupID(values)
		a := groups[id]
		if a == nil {
			a = &accumulator{}
			groups[id] = a
			key := make(map[string]string, len(q.GroupBy))
			for j, g := range q.GroupBy {
				key[g] = values[j]
			}
			keys[id] = key
		}
		a.addJoined(ev, m.shadow, m.feedback)
	}

	r := &Report{From: q.From, To: q.To, GroupBy: q.GroupBy, Verification: q.Verification, Totals: total.totals(q.Verification)}
	if len(q.GroupBy) == 0 {
		return r, nil
	}
	r.Groups = make([]Group, 0, len(groups))
	for id, a := range groups {
		r.Groups = append(r.Groups, Group{Key: keys[id], Totals: a.totals(q.Verification)})
	}
	sortGroups(r.Groups, q.GroupBy)
	if len(r.Groups) > q.Limit {
		r.Groups = r.Groups[:q.Limit]
	}
	return r, nil
}

// joinedEvent is an event with the shadow evaluation and the feedback of
// its tenant and ID, if any.
type joinedEvent struct {
	ev       *Event
	shadow   *ShadowEval
	feedback *Feedback
}

// matching returns the kept events of q's tenant (every tenant when it is
// "") in [q.From, q.To), oldest first, joined to their shadow evaluations
// and feedback. Recorded events, evaluations and feedback are never
// modified (see Sink.Record; the stores keep copies and replace them
// whole), so they can be read without the lock.
func (l *MemoryLedger) matching(q Query) []joinedEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []joinedEvent
	for i := range l.n {
		ev := l.ring[(l.head+i)%len(l.ring)]
		if q.Tenant != "" && ev.Tenant != q.Tenant {
			continue
		}
		if ev.Time.Before(q.From) || !ev.Time.Before(q.To) {
			continue
		}
		k := rowKey{ev.Tenant, ev.ID}
		out = append(out, joinedEvent{ev: ev, shadow: l.shadows.byKey[k], feedback: l.feedback.byKey[k]})
	}
	return out
}

// groupID encodes group values as one map key, length-prefixing each so
// that no two different value lists collide.
func groupID(values []string) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
	}
	return b.String()
}
