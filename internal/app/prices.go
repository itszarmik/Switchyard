package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/gateway"
)

// Dated price changes (pricing.changes; DESIGN §12.1, §16.4). The
// catalog has the prices in force when the gateway's snapshot is built,
// and names the next change after that (catalog.Catalog.NextPriceChange).
// A timer set for that time wakes the App, which then rebuilds the
// snapshot from the configuration in force, without reading the files
// again, the way a reload stages one, and puts it in force. The timer is
// set again after every switch and every reload. A check once a minute
// retries a rebuild that failed, and catches a change the timer missed
// (the clock jumped, or the machine slept). A reload after a change picks
// up the new prices itself. Requests in flight finish at the prices they
// started with. A rebuild that fails leaves the snapshot in force serving.

// priceCheckInterval is how often the App checks, besides the timer, for a
// price change that has taken effect.
const priceCheckInterval = time.Minute

// Price-switch triggers, logged with each switch.
const (
	priceTriggerSchedule = "schedule"
	priceTriggerReload   = "reload"
)

// clock is the App's time source: the gateway's clock, which the catalog
// resolves prices with, and the timers of the price-change check. Tests
// replace it.
type clock struct {
	now func() time.Time
	// ticker returns a channel that ticks every d and a function that
	// stops it.
	ticker func(d time.Duration) (<-chan time.Time, func())
	// timer returns a channel that receives once, after d, and a function
	// that stops it.
	timer func(d time.Duration) (<-chan time.Time, func())
}

// systemClock is the real clock.
var systemClock = clock{
	now: time.Now,
	ticker: func(d time.Duration) (<-chan time.Time, func()) {
		t := time.NewTicker(d)
		return t.C, t.Stop
	},
	timer: func(d time.Duration) (<-chan time.Time, func()) {
		t := time.NewTimer(d)
		return t.C, func() { t.Stop() }
	},
}

// newPriceMetrics registers the price-switch metrics: the counter of
// switches by result and the gauge of the next change, read from the
// catalog in force at each scrape.
func newPriceMetrics(reg prometheus.Registerer, next func() time.Time) (*prometheus.CounterVec, error) {
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_catalog_price_switches_total",
		Help: "Snapshot rebuilds that put a dated catalog price change in force, by result (ok or error).",
	}, []string{"result"})
	gauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "switchyard_catalog_next_price_change_timestamp_seconds",
		Help: "Unix time of the next dated catalog price change of the configuration in force, or 0 if none is pending.",
	}, func() float64 {
		if t := next(); !t.IsZero() {
			return float64(t.Unix())
		}
		return 0
	})
	for _, c := range []prometheus.Collector{total, gauge} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("app: register the price-switch metrics: %w", err)
		}
	}
	total.WithLabelValues(reloadOK)
	total.WithLabelValues(reloadError)
	return total, nil
}

// watchPrices runs the price-change check at the catalog's next price
// change and every priceCheckInterval until ctx is done or the returned
// function is called, which stops it and waits for a check in progress to
// finish.
func (a *App) watchPrices(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	ticks, stopTicker := a.clock.ticker(priceCheckInterval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stopTicker()
		// tried is the change the last check was for. A change already due
		// that no check has tried yet (it became due between the build of
		// the catalog and now: at startup, or after a reload) is checked at
		// once; one that a check has tried failed to switch, and the ticker
		// retries it, so that a failing rebuild is not retried in a loop.
		var tried time.Time
		check := func() {
			tried = a.gw.Catalog().NextPriceChange()
			a.checkPrices()
		}
		for {
			var at <-chan time.Time
			stopTimer := func() {}
			if next := a.gw.Catalog().NextPriceChange(); !next.IsZero() {
				d := next.Sub(a.clock.now())
				switch {
				case d > 0:
					// The timer is for the next change of the catalog in
					// force.
					at, stopTimer = a.clock.timer(d)
				case !next.Equal(tried):
					check()
					continue
				}
			}
			select {
			case <-ctx.Done():
				stopTimer()
				return
			case <-a.pricesRearm:
				// A reload replaced the catalog: set the timer again.
			case <-at:
				check()
			case <-ticks:
				check()
			}
			stopTimer()
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// checkPrices runs switchPrices and reports its result to priceChecked.
func (a *App) checkPrices() {
	err := a.switchPrices()
	if a.priceChecked != nil {
		a.priceChecked(err)
	}
}

// rearmPrices makes the price-change check set its timer again, for a
// catalog a reload put in force.
func (a *App) rearmPrices() {
	select {
	case a.pricesRearm <- struct{}{}:
	default: // already pending
	}
}

// errNoPriceChange is returned by switchPrices when no change is due.
var errNoPriceChange = errors.New("no price change is due")

// switchPrices puts the dated price changes that have taken effect in
// force, if any: it rebuilds the gateway's snapshot from the configuration
// in force, at the time it read, and commits it. It returns
// errNoPriceChange when none is due, and the rebuild's error, which it
// logs and counts, when the rebuild fails.
//
// It does not make the checks a reload makes on the judge and the
// embedding deployment (checkStaged): the configuration is the one in
// force, which passed them, and their choice of deployment depends on
// prices only among permitted deployments (eval.ShadowJudge,
// semantic.ConfiguredDeployment). The shadow runner and the semantic
// cache keep their startup deployment anyway, so a staged choice is
// never used.
func (a *App) switchPrices() error {
	r := &a.reload
	r.mu.Lock()
	defer r.mu.Unlock()
	now := a.clock.now()
	if !a.priceChangeDue(now) {
		return errNoPriceChange
	}
	previous := a.gw.Catalog()
	staged, err := r.stage(r.cfg, r.running, now)
	if err != nil {
		msg := scrub(err.Error(), r.secrets)
		r.priceSwitches.WithLabelValues(reloadError).Inc()
		a.log.Error("catalog price change failed; the prices in force keep serving and the change is tried again at the next check",
			"config_version", r.running, "price_change", formatPriceTime(previous.NextPriceChange()), "error", msg)
		return errors.New(msg)
	}
	staged.Commit()
	a.logPriceSwitch(previous, staged.Catalog(), priceTriggerSchedule)
	return nil
}

// priceChangeDue reports whether the next price change of the catalog in
// force has taken effect at now. It runs with a.reload.mu held.
func (a *App) priceChangeDue(now time.Time) bool {
	next := a.gw.Catalog().NextPriceChange()
	return !next.IsZero() && !now.Before(next)
}

// logPriceSwitch counts a price switch from catalog previous to next and
// logs it: the deployments whose prices changed, with their old and new
// prices, and the next change. Prices hold no secrets.
func (a *App) logPriceSwitch(previous, next *catalog.Catalog, trigger string) {
	a.reload.priceSwitches.WithLabelValues(reloadOK).Inc()
	old := make(map[string]config.Pricing)
	for _, m := range previous.Models() {
		for _, d := range m.Deployments {
			old[d.ID] = d.Prices
		}
	}
	ids, changes := []string{}, []string{}
	for _, m := range next.Models() {
		for _, d := range m.Deployments {
			p, ok := old[d.ID]
			if !ok {
				continue
			}
			if diff := priceDiff(p, d.Prices); diff != "" {
				ids = append(ids, d.ID)
				changes = append(changes, d.ID+": "+diff)
			}
		}
	}
	a.log.Info("catalog prices changed", "trigger", trigger, "config_version", a.reload.running,
		"deployments", ids, "changes", changes, "next_price_change", formatPriceTime(next.NextPriceChange()))
}

// livePrices returns the prices in force of dep, a deployment of the
// startup catalog that a long-lived component calls (the shadow judge, the
// semantic cache's embedder): those of the deployment with dep's id in the
// gateway's catalog in force, so that a reload or a dated price change
// reaches their charges at once. When that catalog has no such deployment
// (a reload removed it) or does not know its prices, the last prices known
// are kept, starting with dep's: the component keeps calling the
// deployment until a restart, so its calls are still charged.
func livePrices(gw *gateway.Gateway, dep *catalog.Deployment) func() config.Pricing {
	var last atomic.Pointer[config.Pricing]
	initial := dep.Prices
	last.Store(&initial)
	return func() config.Pricing {
		if d := gw.Catalog().Deployment(dep.ID); d != nil && d.Pricing.Known {
			if p := last.Load(); !samePrices(*p, d.Prices) {
				cur := d.Prices
				last.Store(&cur)
			}
			return d.Prices
		}
		return *last.Load()
	}
}

// samePrices reports whether a and b have the same four prices.
func samePrices(a, b config.Pricing) bool {
	return a.Input.Equal(b.Input) && a.Output.Equal(b.Output) &&
		a.CachedInput.Equal(b.CachedInput) && a.CacheWrite.Equal(b.CacheWrite)
}

// priceDiff describes the prices that differ between a and b, such as
// "input 2.00 -> 4.00, output 10.00 -> 20.00", or returns "".
func priceDiff(a, b config.Pricing) string {
	var parts []string
	for _, f := range []struct {
		name string
		a, b config.Price
	}{
		{"input", a.Input, b.Input}, {"output", a.Output, b.Output},
		{"cached_input", a.CachedInput, b.CachedInput}, {"cache_write", a.CacheWrite, b.CacheWrite},
	} {
		if !f.a.Equal(f.b) {
			parts = append(parts, f.name+" "+priceText(f.a)+" -> "+priceText(f.b))
		}
	}
	return strings.Join(parts, ", ")
}

// priceText is a price as written, or "unset".
func priceText(p config.Price) string {
	if !p.IsSet() {
		return "unset"
	}
	return p.String()
}

// formatPriceTime formats a price change's time for the logs: RFC 3339 in
// UTC, or "none" for the zero time.
func formatPriceTime(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return t.UTC().Format(time.RFC3339)
}
