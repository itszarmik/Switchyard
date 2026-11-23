package gateway

import (
	"errors"
	"fmt"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/providers"
)

// Staged is a snapshot built for a reload (see Stage) but not yet in
// force. A caller that must check it, or swap other state with it, does so
// between Stage and Commit.
type Staged struct {
	g *Gateway
	s *snapshot
}

// Stage builds the snapshot of cfg, which must be validated, over the
// gateway's long-lived state, without putting it in force: it changes
// nothing the snapshot in force uses, so a staged snapshot that is never
// committed leaves no trace. In particular the health tracker's new
// handles and settings (providers' max_concurrency, deployments' providers
// and latency hints) are applied by Commit. Stages must not overlap:
// commit or drop one before staging the next. Breakers,
// cooldowns and latency statistics carry over for every deployment ID that
// still exists. Changes to the health tracker's settings
// (routing.breaker, routing.rate_limit_cooldown and
// routing.fallback.retry_budget_ratio and retry_budget_min) and to the
// cache limits (cache.*) are not applied: they need a restart.
//
// The staged catalog has the prices in force at the gateway's clock, so
// staging the configuration in force again, with its version, once the
// catalog's NextPriceChange has passed puts dated price changes in force
// (the model list keeps its created time).
func (g *Gateway) Stage(cfg *config.Config, configVersion string) (*Staged, error) {
	return g.StageAt(cfg, configVersion, g.now())
}

// StageAt is Stage with the catalog's prices those in force at t, for a
// caller that has already read the clock and must see the same time.
func (g *Gateway) StageAt(cfg *config.Config, configVersion string, t time.Time) (*Staged, error) {
	if cfg == nil {
		return nil, errors.New("gateway: reload: missing Config")
	}
	s, err := g.build(cfg, configVersion, t)
	if err != nil {
		return nil, fmt.Errorf("gateway: reload: %w", err)
	}
	if cur := g.current(); cur != nil && cur.configVersion == configVersion {
		s.created = cur.created
	}
	return &Staged{g: g, s: s}, nil
}

// Catalog returns the staged snapshot's catalog.
func (st *Staged) Catalog() *catalog.Catalog { return st.s.catalog }

// Adapter returns the staged snapshot's adapter of provider, or nil.
func (st *Staged) Adapter(provider string) providers.Adapter { return st.s.Adapter(provider) }

// Commit puts the staged snapshot in force: requests that start afterwards
// use it, while those in flight finish on the snapshot they started with.
// The replaced snapshot's adapters close their idle connections; those in
// use stay open until their requests end.
func (st *Staged) Commit() {
	st.s.batch.Apply()
	old := st.g.cur.Swap(st.s)
	if old == nil {
		return
	}
	for _, a := range old.adapters {
		if c, ok := a.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
}

// Reload stages the snapshot of cfg and commits it (see Stage and Commit).
// On error the snapshot in force stays.
func (g *Gateway) Reload(cfg *config.Config, configVersion string) error {
	st, err := g.Stage(cfg, configVersion)
	if err != nil {
		return err
	}
	st.Commit()
	return nil
}

// ConfigVersion returns the version of the configuration in force.
func (g *Gateway) ConfigVersion() string { return g.current().configVersion }
