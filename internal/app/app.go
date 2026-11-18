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
// POST /admin/v1/cache/purge, which purges a tenant's cache entries, to
// its built-in routes.
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
	"switchyard/internal/server"
	"switchyard/internal/shadow"
	"switchyard/internal/telemetry"
	"switchyard/internal/usage"
	"switchyard/internal/version"
)

// App is the wired service. Create it with New and start it with Run.
type App struct {
	log *slog.Logger
	srv *server.Server
	// ledger is the usage ledger, flushed by Run after the server has
	// drained. redis is the state instance's client, if any.
	ledger        ledger
	redis         *redis.Client
	shutdownGrace time.Duration
	// shadow is the shadow sampling runner, or nil when no policy samples.
	shadow *shadow.Runner
	// gw is the gateway, whose cache writes drain at shutdown; cacheRedis
	// is the cache instance's client, if any.
	gw         *gateway.Gateway
	cacheRedis *redis.Client
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

// New builds the service from snap, writing logs to logOutput.
//
// It also installs the service logger as slog's default logger, and logs the
// loaded configuration: "config loaded" at INFO, each snapshot warning at
// WARN and the effective configuration at DEBUG.
func New(snap *config.Snapshot, logOutput io.Writer) (*App, error) {
	return newApp(snap, logOutput, openStorage)
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

// newApp is New with the storage opened by open.
func newApp(snap *config.Snapshot, logOutput io.Writer, open storageOpener) (*App, error) {
	if snap == nil {
		return nil, errors.New("app: nil config snapshot")
	}
	if logOutput == nil {
		return nil, errors.New("app: nil log output")
	}
	cfg := snap.Config()

	level, err := telemetry.ParseLevel(cfg.Logging.Level)
	if err != nil {
		return nil, fmt.Errorf("app: logging.level: %w", err)
	}
	logger := telemetry.NewLogger(logOutput, telemetry.LogOptions{
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
		Logger:            logger,
		Registry:          registry,
	})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	st, err := open(cfg, registry, logger)
	if err != nil {
		return nil, err
	}
	led, rdb := st.ledger, st.redis
	a := &App{log: logger, srv: srv, ledger: led, redis: rdb, cacheRedis: st.cacheRedis, shutdownGrace: cfg.Server.ShutdownGrace.Std()}
	var budgetCheck func(ctx context.Context) error
	if rdb != nil {
		budgetCheck = func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
		// With fail_mode open, requests are served while Redis is down, so
		// the replica stays ready; GET /admin/v1/providers and
		// budget_store_errors_total show the outage.
		if cfg.Budgets.FailMode == config.BudgetFailClosed {
			srv.AddReadinessCheck("budget_store", budgetCheck)
		}
	}

	gw, err := gateway.New(gateway.Options{
		Config: cfg, Logger: logger, Registry: registry,
		Ledger: led, Budgets: st.budgets, BudgetStoreCheck: budgetCheck, ConfigVersion: snap.Version(),
		Cache: st.cache,
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
	if a.shadow, err = newShadowRunner(cfg, gw, st.budgets, led, registry, logger); err != nil {
		_ = a.closeStorage(context.Background())
		return nil, fmt.Errorf("app: %w", err)
	}
	if a.shadow != nil {
		gw.SetShadow(a.shadow)
	}
	store := auth.NewStore(cfg.Tenants)
	authenticate := auth.Middleware(store)
	routes := openaiapi.Routes(gw, openaiapi.Options{
		BodyReadTimeout: cfg.Server.BodyReadTimeout.Std(),
		WriteTimeout:    cfg.Server.WriteTimeout.Std(),
	})
	for _, r := range routes {
		// Instrument goes outside authentication, so that rejected
		// requests count in the API metrics too.
		srv.Handle(r.Pattern, gw.Instrument(r.Endpoint, authenticate(r.Handler)))
	}
	usageOpts := usageapi.Options{Verification: usage.Verification{
		MinSamples:  cfg.Evaluation.Shadow.VerifiedMinSamples,
		MinPassRate: cfg.Evaluation.Shadow.VerifiedMinPassRate,
	}}
	srv.Handle("GET "+usageEndpoint, gw.Instrument(usageEndpoint, authenticate(usageapi.TenantHandler(led, usageOpts))))
	srv.Handle("POST "+feedbackEndpoint, gw.Instrument(feedbackEndpoint, authenticate(usageapi.FeedbackHandler(led, usageOpts))))
	srv.HandleAdmin("GET /admin/v1/providers", gw.ProvidersHandler())
	srv.HandleAdmin("GET /admin/v1/usage", usageapi.AdminHandler(led, usageOpts))
	srv.HandleAdmin("POST /admin/v1/cache/purge", gw.CachePurgeHandler())

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

	return a, nil
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
// adapter; the comparison calls use the gateway's adapters too. Shadow
// spend is charged to the budget store (through its circuit, with Redis),
// and the results go to the usage ledger.
func newShadowRunner(cfg *config.Config, gw *gateway.Gateway, budgets budget.Store, store usage.ShadowStore,
	reg prometheus.Registerer, logger *slog.Logger) (*shadow.Runner, error) {
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
	j, err := judge.NewLLM(judge.Options{Adapter: adapter, Deployment: dep})
	if err != nil {
		return nil, err
	}
	r, err := shadow.NewRunner(shadow.RunnerOptions{
		Adapters: gw.Adapter, Judge: j, JudgeDeployment: dep,
		Budgets: budgets, DailyBudgetUSD: s.DailyBudgetUSD, Store: store,
		Concurrency: s.Concurrency, MaxOutputTokens: s.MaxOutputTokens,
		Registerer: reg, Logger: logger.With("component", "shadow"),
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
// otherwise.
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
	e := semantic.NewEmbedder(emb, dep, dep.Model.Dimensions, timeout)
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
// first because it stores its results in the ledger. Canceling forceCtx
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
	err := a.srv.Run(ctx, forceCtx)
	// The server has drained: no request records events any more. The
	// ledger gets the shutdown grace period to write what is queued, cut
	// short by forceCtx.
	cctx, cancel := context.WithTimeout(context.Background(), a.shutdownGrace)
	defer cancel()
	stop := context.AfterFunc(forceCtx, cancel)
	defer stop()
	if a.shadow != nil {
		sctx, scancel := context.WithTimeout(cctx, a.shutdownGrace/2)
		if serr := a.shadow.Close(sctx); serr != nil {
			a.log.Warn("shadow sampling: samples were canceled at shutdown", "error", serr)
		}
		scancel()
	}
	if a.gw != nil {
		wctx, wcancel := context.WithTimeout(cctx, a.shutdownGrace/4)
		if werr := a.gw.CloseCache(wctx); werr != nil {
			a.log.Warn("response cache: writes were abandoned at shutdown", "error", werr)
		}
		wcancel()
	}
	if cerr := a.closeStorage(cctx); cerr != nil {
		err = errors.Join(err, fmt.Errorf("app: %w", cerr))
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
