// Package app is Switchyard's composition root. It turns a validated
// configuration snapshot into the running service: the redacting logger, the
// metrics registry and the HTTP server, wired together.
//
// The public listener serves the built-in health routes without
// authentication, and the OpenAI-compatible /v1 routes, the tenant's usage
// report, GET /switchyard/v1/usage, and feedback on the tenant's requests,
// POST /switchyard/v1/feedback, behind the API key middleware.
// The admin listener adds GET /admin/v1/providers, the deployments'
// health, GET /admin/v1/usage, the usage report of any tenant, and
// POST /admin/v1/cache/purge, which purges a tenant's cache entries, and
// POST /admin/v1/reload, which reloads the configuration (see Reload), to
// its built-in routes. With server.admin_token set, every /admin/v1/*
// route requires it. Reloads and purges write an audit line.
//
// Every authenticated public route runs behind the per-address limit on
// failed authentications, authentication, and the key's request rate
// (ratelimit.Enforcer), whose counters are in the Redis state instance
// with storage.mode redis, shared by every replica, and in memory
// otherwise; streaming requests also take a slot of the key's
// concurrent-stream cap in the gateway. Tracing (telemetry.tracing) starts
// the root span of every request outermost in the server's chain; the log
// output goes through an asynchronous writer, drained last at shutdown.
//
// The usage ledger is kept in memory or in Postgres (storage.ledger), and
// the budget counters in memory or, with storage.mode redis, in the Redis
// state instance. Postgres must be reachable at startup, since the
// ledger's migrations run then; Redis need not be, since budgets follow
// budgets.fail_mode while it is down. After a Redis failure the budget
// store is skipped for 5 seconds (budget.Circuit), so requests do not each
// wait for it. With fail_mode closed, /readyz reports Redis as the
// budget_store check; with open, the replica stays ready and
// GET /admin/v1/providers reports it.
//
// The response caches (DESIGN §11) follow storage.mode too: with redis,
// exact entries and the semantic vector index live in the cache instance
// (storage.redis.cache_url, which should run allkeys-lru) and the tenants'
// namespace versions in the state instance, beside the budgets; with
// memory, all of it is in process. A cache Redis that is unreachable is
// only logged, since every cache failure is a miss, and /readyz never
// checks it. The semantic cache is built only when a policy enables it,
// with semantic_cache.embedding_model called through the gateway's
// adapters. POST /admin/v1/cache/purge purges a tenant.
//
// Dated catalog price changes (pricing.changes) take effect at their time
// without a reload: a timer for the catalog's next change, with a check
// once a minute as the retry, rebuilds the gateway's snapshot from the
// configuration in force (see switchPrices).
//
// When a policy sets shadow.sample_rate > 0, a shadow.Runner evaluates the
// sampled requests in the background, with the judge
// (evaluation.judge.model) and the comparison deployments called through
// the gateway's adapters, its spend charged to the budget store and its
// results stored in the usage ledger.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"switchyard/internal/api/openaiapi"
	"switchyard/internal/api/usageapi"
	"switchyard/internal/auth"
	"switchyard/internal/budget"
	"switchyard/internal/cache"
	"switchyard/internal/cache/semantic"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/eval"
	"switchyard/internal/gateway"
	"switchyard/internal/judge"
	"switchyard/internal/providers"
	"switchyard/internal/ratelimit"
	"switchyard/internal/server"
	"switchyard/internal/shadow"
	"switchyard/internal/telemetry"
	"switchyard/internal/tracing"
	"switchyard/internal/usage"
	"switchyard/internal/version"
)

// App is the wired service. Create it with New and start it with Run.
type App struct {
	log *slog.Logger
	srv *server.Server
	// ledger is the usage ledger, flushed by Run after the server has
	// drained. redis is the state instance's client, if any.
	ledger ledger
	redis  *redis.Client
	// shadow is the shadow sampling runner, or nil when no policy samples.
	shadow *shadow.Runner
	// gw is the gateway, whose cache writes drain at shutdown; cacheRedis
	// is the cache instance's client, if any.
	gw         *gateway.Gateway
	cacheRedis *redis.Client
	// reload is the hot reload state (see Reload); budgetFailClosed is
	// budgets.fail_mode closed in the configuration in force, for the
	// budget store's readiness check.
	reload           reloader
	budgetFailClosed atomic.Bool
	// limits is the gateway-wide rate-limit settings in force, swapped on
	// every reload; rlimiter is the Redis rate limiter, if any, closed
	// before the state client.
	limits   atomic.Pointer[ratelimit.Limits]
	rlimiter *ratelimit.RedisLimiter
	// tracing is the tracer provider, shut down after the server drains.
	tracing *tracing.Provider
	// clock is the gateway's clock and the price-change check's ticker;
	// priceChecked, when set (by tests, before Run), is called after each
	// check with switchPrices' result.
	clock        clock
	priceChecked func(error)
	// pricesRearm tells the price-change check that a reload replaced the
	// catalog, so that it sets its timer for the new next change.
	pricesRearm chan struct{}
	// logWriter queues the log lines for a background goroutine; Run
	// closes it last, after everything else has logged.
	logWriter *telemetry.AsyncWriter
}

// ledger is a usage ledger: where events go, what reports read, and where
// shadow evaluations and feedback are stored.
type ledger interface {
	usage.Sink
	usage.Reporter
	usage.ShadowStore
	usage.FeedbackStore
}

// Startup timeouts.
const (
	// ledgerStartTimeout bounds connecting to Postgres and migrating.
	ledgerStartTimeout = 30 * time.Second
	// redisPingTimeout bounds the startup check of the Redis state
	// instance.
	redisPingTimeout = 2 * time.Second
)

// Redis client timeouts, used unless the URL sets them: budget checks
// are on the request path, so a Redis that stops answering must fail
// fast for budgets.fail_mode to apply.
const (
	redisDialTimeout = 2 * time.Second
	redisIOTimeout   = time.Second
)

// Cache Redis client settings, used unless the URL sets them. A dial is
// bounded by cacheRedisDialTimeout. With ContextTimeoutEnabled, go-redis
// bounds each socket read and write by the earlier of the call's context
// deadline and the client's ReadTimeout or WriteTimeout, so those are set
// above every deadline the cache calls carry (see cacheRedisIOTimeout):
// the contexts are the real bound, cache.lookup_timeout for an exact
// lookup, semantic_cache.lookup_timeout for a semantic one and
// gateway.CacheWriteTimeout for a write, rather than a shorter client
// timeout cutting them off. A failed command is not retried: the lookup is
// a miss instead.
const (
	cacheRedisDialTimeout = 200 * time.Millisecond
	// cacheRedisIOMargin is added to the longest context deadline.
	cacheRedisIOMargin = 100 * time.Millisecond
	cacheRedisPoolSize = 64
)

// cacheRedisIOTimeout is the cache Redis client's read and write timeout:
// the longest deadline its calls carry, plus cacheRedisIOMargin.
func cacheRedisIOTimeout(cfg *config.Config) time.Duration {
	return max(cfg.Cache.LookupTimeout.Std(), cfg.SemanticCache.LookupTimeout.Std(), gateway.CacheWriteTimeout) + cacheRedisIOMargin
}

// New builds the service from snap, writing logs to logOutput. load, which
// may be nil to turn reloads off, loads the configuration again for a
// reload (see Reload), from the same sources as snap.
//
// It also installs the service logger as slog's default logger, and logs the
// loaded configuration: "config loaded" at INFO, each snapshot warning at
// WARN and the effective configuration at DEBUG.
func New(snap *config.Snapshot, logOutput io.Writer, load Loader) (*App, error) {
	a, err := newApp(snap, logOutput, openStorage, systemClock)
	if err != nil {
		return nil, err
	}
	a.reload.load = load
	return a, nil
}

// The bounds of the shutdown work after the server has drained (see Run).
// It all fits in PostDrainTimeout, so a process stops within
// server.shutdown_delay + server.shutdown_grace + PostDrainTimeout of
// SIGTERM: the formula for compose's stop_grace_period and Kubernetes'
// terminationGracePeriodSeconds.
const (
	// PostDrainTimeout bounds everything Run does once the server has
	// drained.
	PostDrainTimeout = 15 * time.Second
	// tracingShutdownTimeout bounds the flush of the queued spans, and
	// logCloseTimeout the final drain of the log queue; they come last.
	tracingShutdownTimeout = 2 * time.Second
	logCloseTimeout        = 2 * time.Second
	// storageCloseTimeout is the rest: the shadow runner, the cache
	// writes and the usage ledger's flush, which share one deadline.
	storageCloseTimeout = PostDrainTimeout - tracingShutdownTimeout - logCloseTimeout
	// shadowCloseTimeout and cacheCloseTimeout bound the shadow runner and
	// the cache writes within it; the ledger gets what they leave, at
	// least storageCloseTimeout - 5 s.
	shadowCloseTimeout = 4 * time.Second
	cacheCloseTimeout  = time.Second
)

// closeLogWriter drains the log queue, within logCloseTimeout. Lines
// logged afterwards are written synchronously.
func closeLogWriter(w *telemetry.AsyncWriter) {
	ctx, cancel := context.WithTimeout(context.Background(), logCloseTimeout)
	defer cancel()
	_ = w.Close(ctx) // the error says lines were still queued; nowhere to log it
}

// storage is what the storage settings open: the usage ledger, the budget
// store, the exact cache and the namespace versions and, with
// storage.mode redis, the clients of the state and cache instances.
type storage struct {
	ledger     ledger
	budgets    budget.Store
	redis      *redis.Client
	cache      gateway.CacheOptions
	cacheRedis *redis.Client
}

// storageOpener opens the storage for New; tests replace openStorage.
type storageOpener func(cfg *config.Config, reg prometheus.Registerer, logger *slog.Logger) (storage, error)

// openStorage opens the usage ledger and the budget store.
func openStorage(cfg *config.Config, reg prometheus.Registerer, logger *slog.Logger) (storage, error) {
	led, err := newLedger(cfg.Storage.Ledger, reg, logger)
	if err != nil {
		return storage{}, err
	}
	budgets, rdb, err := newBudgetStore(cfg, logger)
	if err != nil {
		closeLedger(led, logger)
		return storage{}, err
	}
	caches, cacheRdb, err := newCacheStorage(cfg, rdb, logger)
	if err != nil {
		closeLedger(led, logger)
		if rdb != nil {
			_ = rdb.Close()
		}
		return storage{}, err
	}
	return storage{ledger: led, budgets: budgets, redis: rdb, cache: caches, cacheRedis: cacheRdb}, nil
}

// newApp is New with the storage opened by open and the clock clk. The log
// output is wrapped in an asynchronous writer (telemetry.AsyncWriter), so
// writing the access log never blocks a request; on error newApp closes
// it, so the lines logged so far are written.
func newApp(snap *config.Snapshot, logOutput io.Writer, open storageOpener, clk clock) (*App, error) {
	if snap == nil {
		return nil, errors.New("app: nil config snapshot")
	}
	if logOutput == nil {
		return nil, errors.New("app: nil log output")
	}
	// Audit lines wait for room in a full queue rather than being dropped.
	logWriter := telemetry.NewAsyncWriter(logOutput, telemetry.AsyncOptions{Durable: server.IsAuditLine})
	a, err := buildApp(snap, logWriter, open, clk)
	if err != nil {
		closeLogWriter(logWriter)
		return nil, err
	}
	a.logWriter = logWriter
	return a, nil
}

// buildApp is newApp with the log output wrapped in logWriter.
func buildApp(snap *config.Snapshot, logWriter *telemetry.AsyncWriter, open storageOpener, clk clock) (*App, error) {
	cfg := snap.Config()

	level, err := telemetry.ParseLevel(cfg.Logging.Level)
	if err != nil {
		return nil, fmt.Errorf("app: logging.level: %w", err)
	}
	logger := telemetry.NewLogger(logWriter, telemetry.LogOptions{
		Level:   level,
		Format:  cfg.Logging.Format,
		Secrets: cfg.SecretValues(),
	})
	// The one sanctioned process-global: code that logs without a
	// request-scoped logger (reqctx.Logger's fallback, the log package) must
	// go through the redacting handler too.
	slog.SetDefault(logger)
	// go-redis logs through a process-global logger of its own; route it
	// through the redacting handler too.
	redis.SetLogger(redisLogger{log: logger})

	// The server and the gateway share one registry, which admin /metrics
	// serves.
	registry := telemetry.NewRegistry()
	if err := registry.Register(logWriter.Collector()); err != nil {
		return nil, fmt.Errorf("app: register the log writer metrics: %w", err)
	}
	// Tracing starts first, so every later component's requests are
	// traced; telemetry is restart-only, so it is never rebuilt.
	tp, err := tracing.New(context.Background(), cfg.Telemetry.Tracing, version.Get(), logger)
	if err != nil {
		return nil, fmt.Errorf("app: telemetry.tracing: %w", err)
	}
	built := false
	defer func() {
		if !built {
			shutdownTracing(tp, logger)
		}
	}()
	if err := registerTracingMetrics(registry, tp); err != nil {
		return nil, err
	}
	srv, err := server.New(server.Options{
		Listen:            cfg.Server.Listen,
		AdminListen:       cfg.Server.AdminListen,
		MaxRequestBytes:   cfg.Server.MaxRequestBytes.Int64(),
		MaxHeaderBytes:    int(cfg.Server.MaxHeaderBytes.Int64()),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		ShutdownDelay:     cfg.Server.ShutdownDelay.Std(),
		ShutdownGrace:     cfg.Server.ShutdownGrace.Std(),
		ConfigVersion:     snap.Version(),
		TLSCertFile:       cfg.Server.TLS.CertFile,
		TLSKeyFile:        cfg.Server.TLS.KeyFile,
		TLSMinVersion:     cfg.Server.TLS.TLSMinVersion(),
		AdminToken:        cfg.Server.AdminToken.Value(),
		Logger:            logger,
		Registry:          registry,
		Tracing:           tp,
	})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	st, err := open(cfg, registry, logger)
	if err != nil {
		return nil, err
	}
	led, rdb := st.ledger, st.redis
	a := &App{log: logger, srv: srv, ledger: led, redis: rdb, cacheRedis: st.cacheRedis, tracing: tp, clock: clk,
		pricesRearm: make(chan struct{}, 1)}
	a.budgetFailClosed.Store(cfg.Budgets.FailMode == config.BudgetFailClosed)
	a.limits.Store(ratelimit.NewLimits(cfg))
	enf, af, err := a.newRateLimits(rdb, registry)
	if err != nil {
		_ = a.closeStorage(context.Background())
		return nil, err
	}
	var budgetCheck func(ctx context.Context) error
	if rdb != nil {
		budgetCheck = func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
		// With fail_mode open, requests are served while Redis is down, so
		// the replica stays ready; GET /admin/v1/providers and
		// budget_store_errors_total show the outage. fail_mode can change
		// on reload, so the check reads it each time.
		srv.AddReadinessCheck("budget_store", func(ctx context.Context) error {
			if !a.budgetFailClosed.Load() {
				return nil
			}
			return budgetCheck(ctx)
		})
	}

	gw, err := gateway.New(gateway.Options{
		Config: cfg, Logger: logger, Registry: registry,
		Clock: clk.now, Ledger: led, Budgets: st.budgets, BudgetStoreCheck: budgetCheck, ConfigVersion: snap.Version(),
		Cache: st.cache, Streams: enf, PropagateTrace: cfg.Telemetry.Tracing.Enabled && cfg.Telemetry.Tracing.PropagateUpstream,
	})
	if err != nil {
		_ = a.closeStorage(context.Background())
		return nil, fmt.Errorf("app: %w", err)
	}
	a.gw = gw
	if semanticEnabled(cfg) {
		sc, dep, err := newSemanticCache(cfg, gw, st.cacheRedis, logger)
		if err != nil {
			_ = a.closeStorage(context.Background())
			return nil, fmt.Errorf("app: %w", err)
		}
		gw.SetSemanticCache(sc, dep)
	}
	if a.shadow, err = newShadowRunner(cfg, gw, st.budgets, led, registry, logger, clk.now); err != nil {
		_ = a.closeStorage(context.Background())
		return nil, fmt.Errorf("app: %w", err)
	}
	if a.shadow != nil {
		gw.SetShadow(a.shadow)
	}
	store := auth.NewStore(cfg.Tenants)
	keys := auth.NewKeyring(store)
	authenticate := auth.Middleware(keys, auth.Options{Guard: af.Check})
	// Every authenticated public route: authentication, with the limit on
	// failed authentications per client address, then the key's request
	// rate. Instrument goes
	// outside them all, so that rejected requests count in the API
	// metrics too.
	protect := func(endpoint string, h http.Handler) http.Handler {
		return gw.Instrument(endpoint, authenticate(enf.Middleware(h)))
	}
	routes := openaiapi.Routes(gw, openaiapi.Options{
		BodyReadTimeout: cfg.Server.BodyReadTimeout.Std(),
		WriteTimeout:    cfg.Server.WriteTimeout.Std(),
	})
	for _, r := range routes {
		srv.Handle(r.Pattern, protect(r.Endpoint, r.Handler))
	}
	usageOpts := usageapi.Options{Verification: usage.Verification{
		MinSamples:  cfg.Evaluation.Shadow.VerifiedMinSamples,
		MinPassRate: cfg.Evaluation.Shadow.VerifiedMinPassRate,
	}}
	srv.Handle("GET "+usageEndpoint, protect(usageEndpoint, usageapi.TenantHandler(led, usageOpts)))
	srv.Handle("POST "+feedbackEndpoint, protect(feedbackEndpoint, usageapi.FeedbackHandler(led, usageOpts)))
	// The admin API shares the guard: failed admin tokens count against
	// the client address as failed keys do.
	srv.SetAdminGuard(af)
	srv.HandleAdmin("GET /admin/v1/providers", gw.ProvidersHandler())
	srv.HandleAdmin("GET /admin/v1/usage", usageapi.AdminHandler(led, usageOpts))
	srv.HandleAdmin("POST /admin/v1/cache/purge", gw.CachePurgeHandler())
	total, lastSuccess, err := newReloadMetrics(registry)
	if err != nil {
		_ = a.closeStorage(context.Background())
		return nil, err
	}
	priceSwitches, err := newPriceMetrics(registry, func() time.Time { return gw.Catalog().NextPriceChange() })
	if err != nil {
		_ = a.closeStorage(context.Background())
		return nil, err
	}
	a.reload = reloader{
		start: cfg, cfg: cfg, running: snap.Version(), secrets: cfg.SecretValues(), keys: keys,
		stage:     gw.StageAt,
		hasShadow: a.shadow != nil, hasSemantic: semanticEnabled(cfg),
		now: time.Now, total: total, lastSuccess: lastSuccess, priceSwitches: priceSwitches,
	}
	lastSuccess.Set(float64(time.Now().UnixNano()) / 1e9)
	srv.HandleAdmin(reloadEndpoint, a.reloadHandler())

	logger.Info("config loaded",
		"config_version", snap.Version(),
		"sources", snap.Sources(),
		"providers", len(cfg.Providers),
		"models", len(cfg.Models),
		"api_keys", store.Len(),
	)
	for _, w := range snap.Warnings() {
		// A warning about an environment or flag override has no file.
		attrs := make([]any, 0, 8)
		if w.File != "" {
			attrs = append(attrs, "file", w.File)
		}
		if w.Line > 0 {
			attrs = append(attrs, "line", w.Line)
		}
		if w.Path != "" {
			attrs = append(attrs, "path", w.Path)
		}
		attrs = append(attrs, "problem", w.Message)
		logger.Warn("config warning", attrs...)
	}
	// Secret fields redact themselves when encoded; the redacting handler is
	// the second line of defense.
	logger.Debug("effective config", slog.Any("config", cfg))

	built = true
	return a, nil
}

// newRateLimits builds the rate limiting (DESIGN §7 step 3, §17): the
// limiter, in the state Redis behind a ratelimit.Circuit when state is
// set (storage.mode redis), so every replica shares the counters, and in
// memory otherwise; the Enforcer of the keys' request rates and stream
// caps; and the per-replica, in-memory limit on failed authentications.
// Both read the settings in force through a.limits, so a reload applies
// at once.
func (a *App) newRateLimits(state *redis.Client, reg prometheus.Registerer) (*ratelimit.Enforcer, *ratelimit.AuthFailures, error) {
	var limiter ratelimit.Limiter
	if state != nil {
		a.rlimiter = ratelimit.NewRedis(state, ratelimit.RedisOptions{})
		limiter = ratelimit.NewCircuit(a.rlimiter, ratelimit.CircuitOptions{})
	} else {
		limiter = ratelimit.NewMemory(time.Now)
	}
	enf, err := ratelimit.NewEnforcer(ratelimit.EnforcerOptions{Limiter: limiter, Limits: a.limits.Load, Registerer: reg})
	if err != nil {
		return nil, nil, fmt.Errorf("app: rate limits: %w", err)
	}
	af, err := ratelimit.NewAuthFailures(ratelimit.AuthFailuresOptions{
		PerMinute:  func() int { return a.limits.Load().AuthFailuresPerMinute() },
		Registerer: reg,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("app: rate limits: %w", err)
	}
	return enf, af, nil
}

// registerTracingMetrics exports the tracer provider's span counts:
// switchyard_tracing_spans_exported_total and
// switchyard_tracing_spans_dropped_total, both 0 with tracing disabled.
func registerTracingMetrics(reg prometheus.Registerer, tp *tracing.Provider) error {
	for _, c := range []prometheus.Collector{
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "switchyard_tracing_spans_exported_total",
			Help: "Spans the trace collector accepted.",
		}, func() float64 { return float64(tp.ExportStats().Exported) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "switchyard_tracing_spans_dropped_total",
			Help: "Spans dropped: the export queue was full, or the collector failed or refused them.",
		}, func() float64 { return float64(tp.ExportStats().Dropped) }),
	} {
		if err := reg.Register(c); err != nil {
			return fmt.Errorf("app: register the tracing metrics: %w", err)
		}
	}
	return nil
}

// shutdownTracing flushes the queued spans, within tracingShutdownTimeout,
// and stops the exporter. A failure is only logged: tracing fails open.
func shutdownTracing(tp *tracing.Provider, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		logger.Warn("tracing: spans were lost at shutdown", "error", err)
	}
}

// Paths of the public Switchyard endpoints: the usage report and feedback.
const (
	usageEndpoint    = "/switchyard/v1/usage"
	feedbackEndpoint = "/switchyard/v1/feedback"
)

// newShadowRunner returns the shadow sampling runner when a policy has
// shadow.sample_rate > 0, and nil otherwise. The judge is
// evaluation.judge.model, pinned to one deployment (a model@provider, or
// the model's cheapest priced deployment) that every sampling policy must
// permit (eval.ShadowJudge), called through the gateway's
// adapter; the comparison calls use the gateway's adapters too.
// The judge's charges use its deployment's prices in force (livePrices). Shadow
// spend is charged to the budget store (through its circuit, with Redis),
// in the daily window of now, the App's clock, and the results go to the
// usage ledger.
func newShadowRunner(cfg *config.Config, gw *gateway.Gateway, budgets budget.Store, store usage.ShadowStore,
	reg prometheus.Registerer, logger *slog.Logger, now func() time.Time) (*shadow.Runner, error) {
	dep, err := eval.ShadowJudge(gw.Catalog(), cfg)
	if err != nil || dep == nil {
		return nil, err
	}
	s := cfg.Evaluation.Shadow
	adapter := gw.Adapter(dep.Provider)
	if adapter == nil {
		return nil, fmt.Errorf("evaluation.judge.model: provider type %q of %s is not supported", dep.ProviderType, dep.ID)
	}
	// The judge answers within judge.DefaultMaxTokens, whatever
	// max_output_tokens caps the comparison answer at.
	j, err := judge.NewLLM(judge.Options{Adapter: adapter, Deployment: dep, Prices: livePrices(gw, dep)})
	if err != nil {
		return nil, err
	}
	r, err := shadow.NewRunner(shadow.RunnerOptions{
		Adapters: gw.Adapter, Judge: j, JudgeDeployment: dep,
		Budgets: budgets, DailyBudgetUSD: s.DailyBudgetUSD, Store: store,
		Concurrency: s.Concurrency, MaxOutputTokens: s.MaxOutputTokens,
		Registerer: reg, Logger: logger.With("component", "shadow"), Now: now,
	})
	if err != nil {
		return nil, err
	}
	logger.Info("shadow sampling on", "judge", dep.ID, "daily_budget_usd", s.DailyBudgetUSD.String(), "concurrency", s.Concurrency)
	return r, nil
}

// newLedger opens the usage ledger that storage.ledger selects. A Postgres
// ledger connects and runs its migrations now, so an unreachable database
// is an error; the error never includes the DSN.
func newLedger(c config.Ledger, reg prometheus.Registerer, logger *slog.Logger) (ledger, error) {
	if c.Driver != "postgres" {
		return usage.NewMemoryLedger(), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerStartTimeout)
	defer cancel()
	opts := usage.PostgresOptionsFrom(c)
	opts.Registerer, opts.Logger = reg, logger
	l, err := usage.NewPostgresLedger(ctx, c.DSN.Value(), opts)
	if err != nil {
		return nil, fmt.Errorf("app: storage.ledger: cannot start the postgres usage ledger (the database must be reachable at startup to apply its migrations): %w", err)
	}
	logger.Info("usage ledger ready", "driver", "postgres")
	return l, nil
}

// newBudgetStore returns the budget store: Redis, on the state instance,
// behind a budget.Circuit, when storage.mode is redis, and memory
// otherwise. An unreachable Redis is only logged: budgets follow
// budgets.fail_mode until it is back. The client is returned for the
// health checks and for Close.
func newBudgetStore(cfg *config.Config, logger *slog.Logger) (budget.Store, *redis.Client, error) {
	if cfg.Storage.Mode != "redis" {
		return budget.NewMemoryStore(), nil, nil
	}
	opts, err := redis.ParseURL(cfg.Storage.Redis.StateURL.Value())
	if err != nil {
		// go-redis may quote parts of the URL, which can hold a password.
		return nil, nil, errors.New("app: storage.redis.state_url is not a valid Redis URL")
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = redisDialTimeout
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = redisIOTimeout
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = redisIOTimeout
	}
	// One retry at most, of a command and of a dial, so that a Redis that
	// is down costs a request about two timeouts, not ten.
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 1
	}
	if opts.DialerRetries == 0 {
		opts.DialerRetries = 1
	}
	// Honor the context's deadline on every socket operation, so that the
	// budget.Circuit's call timeout bounds a call to a Redis that accepts
	// connections but never answers.
	opts.ContextTimeoutEnabled = true
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Warn("the Redis state instance is unreachable; budgets follow budgets.fail_mode until it is back",
			"fail_mode", cfg.Budgets.FailMode, "error", err)
	}
	return budget.NewCircuit(budget.NewRedisStore(rdb), budget.CircuitOptions{}), rdb, nil
}

// cachingEnabled reports whether any policy, or any of its rules, turns
// on a response cache layer.
func cachingEnabled(cfg *config.Config, semanticOnly bool) bool {
	on := func(p *config.CachePolicy) bool {
		return p.Semantic.Enabled || (!semanticOnly && p.Exact.Enabled)
	}
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		if on(&p.Cache) {
			return true
		}
		for j := range p.Rules {
			if c := p.Rules[j].Set.Cache; c != nil && on(c) {
				return true
			}
		}
	}
	return false
}

// semanticEnabled reports whether any policy or rule turns on the semantic
// cache.
func semanticEnabled(cfg *config.Config) bool { return semantic.Enabled(cfg) }

// newCacheStorage returns the exact cache and the namespace versions. With
// storage.mode redis and caching enabled by some policy, entries go to the
// cache instance, through a client of its own returned for the semantic
// index and Close, and namespace versions to the state instance, through
// state (the budget store's client), which never evicts them. Otherwise
// both are in memory, the store capped at cache.memory_max_entries. An
// unreachable cache Redis is only logged: its lookups are misses.
func newCacheStorage(cfg *config.Config, state *redis.Client, logger *slog.Logger) (gateway.CacheOptions, *redis.Client, error) {
	if cfg.Storage.Mode != "redis" || !cachingEnabled(cfg, false) {
		return gateway.CacheOptions{
			Store:      cache.NewMemoryStore(cfg.Cache.MemoryMaxEntries, time.Now),
			Namespaces: cache.NewMemoryNamespaces(),
		}, nil, nil
	}
	opts, err := redis.ParseURL(cfg.Storage.Redis.CacheURL.Value())
	if err != nil {
		// go-redis may quote parts of the URL, which can hold a password.
		return gateway.CacheOptions{}, nil, errors.New("app: storage.redis.cache_url is not a valid Redis URL")
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = cacheRedisDialTimeout
	}
	for _, d := range []*time.Duration{&opts.ReadTimeout, &opts.WriteTimeout} {
		if *d == 0 {
			*d = cacheRedisIOTimeout(cfg)
		}
	}
	if opts.PoolSize == 0 {
		opts.PoolSize = cacheRedisPoolSize
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = -1 // no retries
	}
	// Honor the context's deadline on every socket operation, so that
	// cache.lookup_timeout bounds a lookup on a Redis that accepts
	// connections but never answers.
	opts.ContextTimeoutEnabled = true
	rdb := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), redisPingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Warn("the Redis cache instance is unreachable; cache lookups are misses until it is back", "error", err)
	}
	return gateway.CacheOptions{Store: cache.NewRedisStore(rdb), Namespaces: cache.NewRedisNamespaces(state)}, rdb, nil
}

// newSemanticCache returns the semantic cache and the deployment it embeds
// with: semantic_cache.embedding_model resolved to one deployment, which
// every policy that enables the semantic cache must permit
// (semantic.ConfiguredDeployment), and called through the gateway's
// adapter, and a vector index named after that deployment and its
// dimensions, in the cache instance when client is set (the query engine
// indexes only database 0, which validation enforces) and in memory
// otherwise. Embeddings are priced at the embedding deployment's
// prices in force (livePrices).
func newSemanticCache(cfg *config.Config, gw *gateway.Gateway, client *redis.Client, logger *slog.Logger) (*semantic.Cache, *catalog.Deployment, error) {
	adapters := make(map[string]providers.Adapter, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if a := gw.Adapter(p.ID); a != nil {
			adapters[p.ID] = a
		}
	}
	dep, emb, err := semantic.ConfiguredDeployment(cfg, gw.Catalog(), adapters)
	if err != nil {
		return nil, nil, err
	}
	timeout := cfg.SemanticCache.LookupTimeout.Std()
	e := semantic.NewEmbedder(emb, dep, dep.Model.Dimensions, timeout).WithPrices(livePrices(gw, dep))
	var idx cache.VectorIndex
	index := "memory"
	if client != nil {
		ri := semantic.NewRedisIndex(client, e.Model(), e.Dimensions())
		idx, index = ri, ri.Name()
	} else {
		idx = semantic.NewMemoryIndex(cfg.Cache.MemoryMaxEntries, time.Now)
	}
	logger.Info("semantic cache on", "embedding_deployment", dep.ID, "dimensions", e.Dimensions(), "index", index)
	return semantic.New(e, idx, semantic.Options{LookupTimeout: timeout}), dep, nil
}

// closeLedger closes l at once, for a failed startup.
func closeLedger(l ledger, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.Close(ctx); err != nil {
		logger.Error("close the usage ledger", "error", err)
	}
}

// closeStorage flushes and closes the ledger, bounded by ctx, then closes
// the Redis client. It returns the ledger's error, which names the events
// that could not be written.
func (a *App) closeStorage(ctx context.Context) error {
	err := a.ledger.Close(ctx)
	if err != nil {
		a.log.Error("usage ledger: events were lost at shutdown", "error", err)
	}
	if a.rlimiter != nil {
		// Gives back the stream leases still held, while the state
		// client is open.
		a.rlimiter.Close()
	}
	if a.redis != nil {
		_ = a.redis.Close()
	}
	if a.cacheRedis != nil {
		_ = a.cacheRedis.Close()
	}
	return err
}

// Logger returns the service logger, which redacts secrets.
func (a *App) Logger() *slog.Logger { return a.log }

// Run serves until ctx is done, then shuts down gracefully: the server
// drains, then the shadow runner finishes the samples it has started
// (within half the shutdown grace period, after which it cancels them and
// drops those still queued), then the cache writes still in flight finish
// (within a quarter of it; each is bounded by its own one-second timeout
// anyway), then the usage ledger writes the events still queued, within
// what is left of the grace period, at least a quarter. The runner stops
// first because it stores its results in the ledger. The queued spans are
// flushed before the ledger (within 5 seconds), the rate limiter's stream
// leases are given back before the Redis clients close, and the log queue
// is drained last (within 5 seconds). Canceling forceCtx
// aborts the requests still in flight, the samples, the cache writes and
// the flush. It returns nil after a clean shutdown,
// server.ErrShutdownTimeout if requests had to be aborted, and an error if
// usage events were lost.
func (a *App) Run(ctx, forceCtx context.Context) error {
	info := version.Get()
	a.log.Info("switchyard starting",
		"version", info.Version,
		"commit", info.Commit,
		"date", info.Date,
		"go_version", info.GoVersion,
	)
	// Dated price changes take effect while the server runs.
	stopPrices := a.watchPrices(ctx)
	err := a.srv.Run(ctx, forceCtx)
	stopPrices()
	// The server has drained: no request records events any more. What
	// follows is bounded by PostDrainTimeout in all, in this order: the
	// shadow samples (which charge budgets and record usage events) and
	// the cache writes, then the usage ledger's flush, sharing one deadline
	// that forceCtx cuts short; then the optimisations that fail open,
	// the span flush and the log drain, each with a bound of its own. Money
	// comes before them, so a dead collector cannot delay the ledger.
	cctx, cancel := context.WithTimeout(context.Background(), storageCloseTimeout)
	defer cancel()
	stop := context.AfterFunc(forceCtx, cancel)
	defer stop()
	if a.shadow != nil {
		sctx, scancel := context.WithTimeout(cctx, shadowCloseTimeout)
		if serr := a.shadow.Close(sctx); serr != nil {
			a.log.Warn("shadow sampling: samples were canceled at shutdown", "error", serr)
		}
		scancel()
	}
	if a.gw != nil {
		wctx, wcancel := context.WithTimeout(cctx, cacheCloseTimeout)
		if werr := a.gw.CloseCache(wctx); werr != nil {
			a.log.Warn("response cache: writes were abandoned at shutdown", "error", werr)
		}
		wcancel()
	}
	if cerr := a.closeStorage(cctx); cerr != nil {
		err = errors.Join(err, fmt.Errorf("app: %w", cerr))
	}
	if a.tracing != nil {
		shutdownTracing(a.tracing, a.log)
	}
	if a.logWriter != nil {
		closeLogWriter(a.logWriter)
	}
	return err
}

// redisLogger passes go-redis's own log lines to the service logger, at
// WARN, since go-redis logs only problems.
type redisLogger struct{ log *slog.Logger }

// Printf implements go-redis's internal.Logging.
func (l redisLogger) Printf(ctx context.Context, format string, v ...any) {
	l.log.WarnContext(ctx, "redis client", "detail", fmt.Sprintf(format, v...))
}
