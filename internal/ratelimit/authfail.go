package ratelimit

import (
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/server"
)

// Defaults of AuthFailuresOptions.
const (
	// DefaultMaxTrackedAddresses bounds the client addresses AuthFailures
	// tracks at once.
	DefaultMaxTrackedAddresses = 100_000
	// sweepEvery is how often, at most, a full table is swept for
	// addresses whose failures no longer count.
	sweepEvery = time.Second
	// ipv6Prefix is the length of the prefix an IPv6 client is limited by:
	// one /64 is one subscriber, who can pick any address in it.
	ipv6Prefix = 64
)

// AuthFailuresOptions configures NewAuthFailures.
type AuthFailuresOptions struct {
	// PerMinute returns rate_limits.auth_failures_per_minute of the
	// current config snapshot (Limits.AuthFailuresPerMinute); 0 or less
	// turns the limit off. It is called on every check, so a reload
	// applies at once. Required.
	PerMinute func() int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// MaxTracked bounds the addresses tracked at once
	// (DefaultMaxTrackedAddresses).
	MaxTracked int
	// Registerer registers switchyard_ratelimit_auth_limited_total.
	// Required.
	Registerer prometheus.Registerer
}

// AuthFailures limits failed authentications per client address (DESIGN
// §17): an address that failed PerMinute times within a minute gets 429
// on every request, whatever its key, until its failures age
// out; then it may try again at PerMinute a minute.
//
// It is GCRA again, charged by failures rather than requests: each
// failure moves the address's arrival time on by T = 1 min / PerMinute,
// and the address is refused while that time is more than (PerMinute−1)·T
// ahead, that is, after PerMinute failures in a burst. The arrival time is
// capped one minute ahead, so concurrent failures cannot lock an address
// out for longer than T.
//
// The authentication middleware checks the credential first and then
// calls Check, which decides and charges in one critical section (see
// Attempt), so concurrent failed attempts from one address cannot exceed
// the limit, and valid ones never count against it.
//
// The state is in memory, per replica: with n replicas, an address may
// fail up to n times PerMinute a minute in all. Addresses are those of the
// connection (see ClientIP); behind a reverse proxy every client has the
// proxy's address, so one client's failures lock out everyone, and the
// limit should be turned off (-1) unless the proxy limits them itself.
//
// At most MaxTracked addresses are tracked. When the table is full, it is
// swept of addresses whose failures have aged out, at most once a second;
// failures from new addresses are not tracked while it stays full, so an
// attacker who fills it cannot lock out anyone else.
//
// It is safe for concurrent use.
type AuthFailures struct {
	perMinute  func() int
	now        func() time.Time
	maxTracked int
	limited    prometheus.Counter

	mu        sync.Mutex
	tats      map[string]time.Time
	lastSweep time.Time
}

// NewAuthFailures returns an AuthFailures that tracks nothing yet.
func NewAuthFailures(o AuthFailuresOptions) (*AuthFailures, error) {
	if o.PerMinute == nil || o.Registerer == nil {
		return nil, errors.New("ratelimit: NewAuthFailures needs PerMinute and a Registerer")
	}
	a := &AuthFailures{perMinute: o.PerMinute, now: o.Now, maxTracked: o.MaxTracked, tats: make(map[string]time.Time)}
	if a.now == nil {
		a.now = time.Now
	}
	if a.maxTracked <= 0 {
		a.maxTracked = DefaultMaxTrackedAddresses
	}
	a.limited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "switchyard_ratelimit_auth_limited_total",
		Help: "Requests answered 429 before authentication because their client address exceeded rate_limits.auth_failures_per_minute.",
	})
	if err := o.Registerer.Register(a.limited); err != nil {
		return nil, err
	}
	return a, nil
}

// authInterval returns the interval T between failures under a limit of
// perMinute failures a minute, rounded up.
func authInterval(perMinute int) time.Duration {
	n := time.Duration(min(perMinute, 1_000_000))
	return (time.Minute + n - 1) / n
}

// Attempt records an authentication attempt from ip, whose credential was
// valid unless failed, and reports whether it may go on. It may not, and
// wait is how long until it may, when the address already has PerMinute
// failures in a burst, whatever its credential: a refused attempt learns
// nothing about it. A failed attempt that may go on is charged. The check
// and the charge are one critical section, so concurrent failed attempts
// from one address never exceed the limit, while valid ones are never
// charged, however many run at once.
func (a *AuthFailures) Attempt(ip string, failed bool) (ok bool, wait time.Duration) {
	n := a.perMinute()
	if n <= 0 {
		return true, 0
	}
	t := authInterval(n)
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	tat, tracked := a.tats[ip]
	if tracked {
		if wait := tat.Sub(now) - time.Duration(n-1)*t; wait > 0 {
			a.limited.Inc()
			return false, wait
		}
	}
	if !failed || (!tracked && len(a.tats) >= a.maxTracked && !a.sweep(now)) {
		return true, 0
	}
	if tat.Before(now) {
		tat = now
	}
	a.tats[ip] = minTime(tat.Add(t), now.Add(time.Duration(n)*t))
	return true, 0
}

// sweep deletes the addresses whose failures have aged out, unless the
// table was swept less than a second ago, and reports whether there is
// room for another address. a.mu must be held.
func (a *AuthFailures) sweep(now time.Time) bool {
	if now.Sub(a.lastSweep) >= sweepEvery {
		a.lastSweep = now
		for ip, tat := range a.tats {
			if !tat.After(now) {
				delete(a.tats, ip)
			}
		}
	}
	return len(a.tats) < a.maxTracked
}

// minTime returns the earlier of x and y.
func minTime(x, y time.Time) time.Time {
	if y.Before(x) {
		return y
	}
	return x
}

// Check is Attempt for r's client address (ClientIP), for the
// authentication middleware to call once it knows whether r's credential
// is valid, and before it answers. When the attempt may not go on, Check
// answers 429 rate_limit_exceeded with Retry-After, without reading the
// body, so a refused client cannot hold the connection by stalling it,
// and returns false: the caller then writes nothing more.
func (a *AuthFailures) Check(w http.ResponseWriter, r *http.Request, authenticated bool) bool {
	ok, wait := a.Attempt(ClientIP(r), !authenticated)
	if !ok {
		server.AbandonBody(w, r)
		AuthLimited(wait).Write(w)
	}
	return ok
}

// ClientIP returns the address AuthFailures limits r by: the IPv4 address
// of the connection's peer (server.RemoteIP), or the /64 prefix of its
// IPv6 address. Forwarding headers such as X-Forwarded-For are not
// trusted.
func ClientIP(r *http.Request) string {
	host := server.RemoteIP(r)
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(ipv6Prefix); err == nil {
			return p.String()
		}
	}
	return addr.String()
}
