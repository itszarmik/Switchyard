// Package budget enforces tenant and key spend limits over calendar windows
// in UTC (DESIGN §12.3).
//
// A request is admitted against every budget scope that applies to it
// before its first upstream attempt: Admit atomically checks each scope
// and, only if every blocking scope has room for the estimated cost,
// charges the estimate to all of them. Delta charges more under the same
// rules, for a fallback to a pricier deployment, and Settle corrects the
// charge to the actual cost once the request is done. If the process dies
// between admission and settlement the estimate stays counted, so the
// counters can only over-count, never under-count.
//
// Amounts are integer micro-USD (1e-6 USD). Limits and costs are rounded
// up to whole micro-USD, which is conservative.
//
// Two stores implement Store: an in-process one for a single replica and
// tests, and a Redis one whose admission is a single Lua script over every
// scope, so concurrent admissions across replicas can never overshoot a
// limit together.
package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"switchyard/internal/config"
)

// Windows of a Scope.
const (
	// WindowDay is a calendar day in UTC.
	WindowDay = config.BudgetWindowDay
	// WindowMonth is a calendar month in UTC.
	WindowMonth = config.BudgetWindowMonth
)

// microPerUSD is the number of micro-USD in one USD.
const microPerUSD = 1_000_000

// counterGrace is how long a window's counter is kept after the window
// ends, so a late Settle still finds it.
const counterGrace = 48 * time.Hour

// Scope is one budget as it applies to a request: a tenant's budget, or a
// key budget as it applies to one key.
type Scope struct {
	// Tenant is the tenant that owns the budget.
	Tenant string
	// KeyID is the key the budget applies to when the budget's scope is
	// key, and "" for a tenant budget.
	KeyID string
	// Window is WindowDay or WindowMonth.
	Window string
	// LimitMicro is limit_usd in micro-USD, rounded up.
	LimitMicro int64
	// SoftAtMicro is the soft threshold, soft_at × limit in micro-USD
	// rounded up, or 0 if the budget has none.
	SoftAtMicro int64
	// SoftProfile is the profile auto requests use once the soft
	// threshold has been crossed in the current window (DESIGN §9.5), or
	// "" for none.
	SoftProfile string
	// Block is true when on_exhausted is block: a request that would
	// exceed the limit is refused. Otherwise (notify) it is charged and
	// reported in Admission.Exceeded.
	Block bool
	// Name identifies the scope in logs, metrics and store keys, for
	// example "acme/month" or "acme/key:k1/day". It is unique among the
	// scopes of a valid configuration.
	Name string
}

// ScopesFor returns the scopes that apply to a request by tenant t with the
// key keyID, in the order of t's budgets. Key budgets are skipped when
// keyID is "". t is expected to be valid (see config.Load).
func ScopesFor(t config.Tenant, keyID string) []Scope {
	var out []Scope
	for _, b := range t.Budgets {
		s := Scope{
			Tenant:     t.ID,
			Window:     b.Window,
			LimitMicro: microCeil(priceRat(b.LimitUSD)),
			Block:      b.OnExhausted != config.BudgetNotify,
		}
		if b.SoftAt > 0 {
			s.SoftAtMicro = softMicro(b.SoftAt, s.LimitMicro)
			s.SoftProfile = b.SoftProfile
		}
		switch b.Scope {
		case config.BudgetScopeKey:
			if keyID == "" {
				continue
			}
			s.KeyID = keyID
			s.Name = t.ID + "/key:" + keyID + "/" + b.Window
		default:
			s.Name = t.ID + "/" + b.Window
		}
		out = append(out, s)
	}
	return out
}

// priceRat returns the exact value of p, or 0 if it is not set.
func priceRat(p config.Price) *big.Rat {
	r, ok := new(big.Rat).SetString(p.String())
	if !ok {
		return new(big.Rat)
	}
	return r
}

// microCeil returns usd in micro-USD, rounded up and capped at
// math.MaxInt64.
func microCeil(usd *big.Rat) int64 {
	m := new(big.Rat).Mul(usd, big.NewRat(microPerUSD, 1))
	return ratCeil(m)
}

// ratCeil returns ⌈r⌉ for r >= 0, capped at math.MaxInt64; negative values
// give 0.
func ratCeil(r *big.Rat) int64 {
	if r.Sign() <= 0 {
		return 0
	}
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if rem.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return math.MaxInt64
	}
	return q.Int64()
}

// softMicro returns ⌈frac × limit⌉, using the shortest decimal form of
// frac so that, for example, 0.8 × 1000 is exactly 800.
func softMicro(frac float64, limit int64) int64 {
	f, ok := new(big.Rat).SetString(strconv.FormatFloat(frac, 'g', -1, 64))
	if !ok {
		return 0
	}
	return ratCeil(f.Mul(f, new(big.Rat).SetInt64(limit)))
}

// Store keeps the spend counters of every scope and window.
type Store interface {
	// Admit atomically checks every scope and, only if every Block scope
	// has room for est (spent + est <= limit), adds est to all of them
	// (DESIGN §12.3). Notify scopes are always charged; their exhaustion
	// is reported in Admission.Exceeded but not refused. A refusal is an
	// *ExhaustedError, and nothing is charged. With no scopes, Admit
	// returns an empty Admission without touching the store. est must not
	// be negative.
	Admit(ctx context.Context, scopes []Scope, est int64, now time.Time) (Admission, error)
	// Settle adds (actual - charged) to the counters Admit and Delta
	// charged, where charged is a.Charged(); actual may be less than the
	// charge. It applies to the windows of the admission, even if they
	// have ended since. Call it once per admission. Settling the zero
	// Admission does nothing. A settlement that crosses a soft threshold
	// is reported by the next Admit or Delta that charges that scope.
	Settle(ctx context.Context, a Admission, actual int64) error
	// Delta charges extra more to the admission's scopes and windows
	// mid-request, for example before a fallback to a pricier deployment,
	// under the same rules as Admit. On success it adds extra to
	// a.Charged() and replaces a.Exceeded and a.SoftCrossed with what this
	// charge found; on refusal a is unchanged. An extra of 0 or less does
	// nothing: the charge is never lowered before Settle. now is accepted
	// for symmetry with Admit; the admission's windows are charged even if
	// now is in a later one.
	Delta(ctx context.Context, a *Admission, extra int64, now time.Time) error
	// Spent returns the amount charged to s in the window containing now.
	Spent(ctx context.Context, s Scope, now time.Time) (int64, error)
	// SoftCrossed reads the state of every scope in the window containing
	// now, in one call to the store, for routing (DESIGN §9.5): what was
	// spent, and whether the soft threshold has been crossed. A threshold
	// is crossed once a charge reached it (the flag Admit and Delta set)
	// or the spend has reached it since, for example by a settlement.
	// The result has one entry per scope, in order; with no scopes it is
	// empty and the store is not called.
	SoftCrossed(ctx context.Context, scopes []Scope, now time.Time) ([]State, error)
}

// State is a scope's spend in the current window, as SoftCrossed reads it.
type State struct {
	// Spent is the amount charged in the window, in micro-USD.
	Spent int64
	// SoftCrossed reports that the scope has a soft threshold and the
	// window's spend has reached it.
	SoftCrossed bool
}

// Remaining returns the room left under the scope's limit, at least 0.
func (st State) Remaining(s Scope) int64 { return max(s.LimitMicro-st.Spent, 0) }

// Admission records what Admit charged, for Delta and Settle.
type Admission struct {
	// Exceeded lists the notify scopes that are over their limit after
	// this charge. Block scopes never are.
	Exceeded []Scope
	// SoftCrossed lists the scopes whose soft threshold this charge
	// reached first in the current window. Each scope and window is
	// reported once, across every replica sharing the store.
	SoftCrossed []Scope

	scopes  []Scope
	windows []time.Time // the start of the window charged, per scope
	charged int64
}

// Charged returns the total amount charged by Admit and Delta.
func (a Admission) Charged() int64 { return a.charged }

// Scopes returns the scopes the admission charged.
func (a Admission) Scopes() []Scope { return a.scopes }

// ErrExhausted is matched, with errors.Is, by every *ExhaustedError.
var ErrExhausted = errors.New("budget exhausted")

// ErrUnavailable is returned, wrapped, when the store failed; the caller
// applies budgets.fail_mode. A canceled or expired ctx is returned as
// ctx.Err(), wrapped, instead.
var ErrUnavailable = errors.New("budget store unavailable")

// ExhaustedError reports that a request does not fit a blocking budget.
type ExhaustedError struct {
	// Scope is the budget that has no room. When several have none, it is
	// the one that resets last.
	Scope Scope
	// ResetAt is the start of the scope's next window, when its counter
	// starts again from zero.
	ResetAt time.Time
}

// Error names the scope and when it resets.
func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("budget %s exhausted until %s", e.Scope.Name, e.ResetAt.Format(time.RFC3339))
}

// Is reports whether target is ErrExhausted.
func (e *ExhaustedError) Is(target error) bool { return target == ErrExhausted }

// windowStart returns the start of the window of kind containing t, in UTC.
func windowStart(kind string, t time.Time) time.Time {
	t = t.UTC()
	if kind == WindowMonth {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// windowEnd returns the start of the window after the one starting at
// start.
func windowEnd(kind string, start time.Time) time.Time {
	if kind == WindowMonth {
		return start.AddDate(0, 1, 0)
	}
	return start.AddDate(0, 0, 1)
}

// ResetAt returns when the window of s containing now ends, which is when
// its counter starts again from zero.
func ResetAt(s Scope, now time.Time) time.Time {
	return windowEnd(s.Window, windowStart(s.Window, now))
}

// expiry returns when a counter of the window of kind starting at start
// may be deleted.
func expiry(kind string, start time.Time) time.Time {
	return windowEnd(kind, start).Add(counterGrace)
}

// checkArgs validates the arguments of Admit and returns the window start
// of each scope at now.
func checkArgs(scopes []Scope, est int64, now time.Time) ([]time.Time, error) {
	if est < 0 {
		return nil, fmt.Errorf("budget: negative estimate %d", est)
	}
	windows := make([]time.Time, len(scopes))
	for i, s := range scopes {
		if s.Window != WindowDay && s.Window != WindowMonth {
			return nil, fmt.Errorf("budget: scope %s has unknown window %q", s.Name, s.Window)
		}
		windows[i] = windowStart(s.Window, now)
	}
	return windows, nil
}

// exhausted returns the error for the block scopes at the indexes full,
// which must not be empty: the scope that resets last.
func exhausted(scopes []Scope, windows []time.Time, full []int) error {
	var best *ExhaustedError
	for _, i := range full {
		reset := windowEnd(scopes[i].Window, windows[i])
		if best == nil || reset.After(best.ResetAt) {
			best = &ExhaustedError{Scope: scopes[i], ResetAt: reset}
		}
	}
	return best
}

// unavailable wraps a store error: as ctx's error if ctx is done, and as
// ErrUnavailable otherwise.
func unavailable(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("budget %s: %w", op, ctxErr)
	}
	return fmt.Errorf("budget %s: %w: %w", op, ErrUnavailable, err)
}
