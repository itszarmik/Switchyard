package config

import (
	"slices"
	"time"
)

// Defaults documented on the fields in types.go.
const (
	defaultListen            = ":8080"
	defaultAdminListen       = "127.0.0.1:9090"
	defaultMaxRequestBytes   = 10 * mib
	defaultMaxHeaderBytes    = 64 * kib
	defaultReadHeaderTimeout = 10 * time.Second
	defaultBodyReadTimeout   = 60 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultShutdownGrace     = 60 * time.Second
	defaultLogLevel          = "info"
	defaultLogFormat         = "json"
	defaultStorageMode       = "memory"
	defaultLedgerDriver      = "memory"
	defaultConnectTimeout    = 5 * time.Second
	defaultFirstTokenTimeout = 30 * time.Second
	defaultIdleChunkTimeout  = 60 * time.Second
	defaultTotalTimeout      = 10 * time.Minute
	defaultModelType         = ModelTypeChat
	defaultTokenRatio        = 1.0
)

// applyDefaults fills zero-valued fields with their documented defaults.
// ShutdownDelay defaults to 0, so it needs no entry.
func applyDefaults(c *Config) {
	setDefault(&c.Server.Listen, defaultListen)
	setDefault(&c.Server.AdminListen, defaultAdminListen)
	setDefault(&c.Server.MaxRequestBytes, defaultMaxRequestBytes)
	setDefault(&c.Server.MaxHeaderBytes, defaultMaxHeaderBytes)
	setDefault(&c.Server.ReadHeaderTimeout, Duration(defaultReadHeaderTimeout))
	setDefault(&c.Server.BodyReadTimeout, Duration(defaultBodyReadTimeout))
	setDefault(&c.Server.IdleTimeout, Duration(defaultIdleTimeout))
	setDefault(&c.Server.ShutdownGrace, Duration(defaultShutdownGrace))
	setDefault(&c.Logging.Level, defaultLogLevel)
	setDefault(&c.Logging.Format, defaultLogFormat)
	setDefault(&c.Storage.Mode, defaultStorageMode)
	setDefault(&c.Storage.Ledger.Driver, defaultLedgerDriver)
	for i := range c.Providers {
		t := &c.Providers[i].Timeouts
		setDefault(&t.Connect, Duration(defaultConnectTimeout))
		setDefault(&t.FirstToken, Duration(defaultFirstTokenTimeout))
		setDefault(&t.Idle, Duration(defaultIdleChunkTimeout))
		setDefault(&t.Total, Duration(defaultTotalTimeout))
	}
	for i := range c.Models {
		m := &c.Models[i]
		setDefault(&m.Type, defaultModelType)
		setDefault(&m.TokenRatio, defaultTokenRatio)
	}
}

// setDefault sets *p to def if *p is the zero value.
func setDefault[T comparable](p *T, def T) {
	var zero T
	if *p == zero {
		*p = def
	}
}

// applyOverrides applies LoadOptions.Overrides on top of the files and
// defaults. Keys are applied in sorted order so problems are deterministic.
func (st *loadState) applyOverrides(c *Config) {
	keys := make([]string, 0, len(st.opts.Overrides))
	for k := range st.opts.Overrides {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := st.opts.Overrides[k]
		switch k {
		case "server.listen":
			c.Server.Listen = v
		case "server.admin_listen":
			c.Server.AdminListen = v
		case "logging.level":
			c.Logging.Level = v
		default:
			st.problems = append(st.problems, Problem{
				Severity: SeverityError, Path: k,
				Message: "unsupported override; only server.listen, server.admin_listen and logging.level can be overridden",
			})
			continue
		}
		st.overridden[k] = true
		delete(st.failed, k) // the override replaces whatever failed to decode
	}
}
