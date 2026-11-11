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

// MemoryLedger is a Sink and Reporter that keeps events in memory, for the
// quickstart and tests. It holds at most a fixed number of events and
// forgets the oldest beyond that, so reports cover only recent traffic and
// nothing survives a restart. It is safe for concurrent use.
type MemoryLedger struct {
	mu     sync.Mutex
	ring   []*Event // circular, oldest at head
	head   int
	n      int
	closed bool
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
	return l
}

var (
	_ Sink     = (*MemoryLedger)(nil)
	_ Reporter = (*MemoryLedger)(nil)
)

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
	if l.n == len(l.ring) {
		l.ring[l.head] = ev
		l.head = (l.head + 1) % len(l.ring)
		return
	}
	l.ring[(l.head+l.n)%len(l.ring)] = ev
	l.n++
}

// Close implements Sink. It stops recording; the kept events can still be
// reported.
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
	for i, ev := range events {
		if i%reportCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		total.add(ev)
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
		a.add(ev)
	}

	r := &Report{From: q.From, To: q.To, GroupBy: q.GroupBy, Totals: total.totals()}
	if len(q.GroupBy) == 0 {
		return r, nil
	}
	r.Groups = make([]Group, 0, len(groups))
	for id, a := range groups {
		r.Groups = append(r.Groups, Group{Key: keys[id], Totals: a.totals()})
	}
	sortGroups(r.Groups, q.GroupBy)
	if len(r.Groups) > q.Limit {
		r.Groups = r.Groups[:q.Limit]
	}
	return r, nil
}

// matching returns the kept events of q's tenant (every tenant when it is
// "") in [q.From, q.To), oldest first. Recorded events are never modified
// (see Sink.Record), so they can be read without the lock.
func (l *MemoryLedger) matching(q Query) []*Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*Event
	for i := range l.n {
		ev := l.ring[(l.head+i)%len(l.ring)]
		if q.Tenant != "" && ev.Tenant != q.Tenant {
			continue
		}
		if ev.Time.Before(q.From) || !ev.Time.Before(q.To) {
			continue
		}
		out = append(out, ev)
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
