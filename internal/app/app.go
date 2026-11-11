// Package app is Switchyard's composition root. It turns a validated
// configuration snapshot into the running service: the redacting logger, the
// metrics registry and the HTTP server, wired together.
//
// The public listener serves the built-in health routes without
// authentication, and the OpenAI-compatible /v1 routes and the tenant's
// usage report, GET /switchyard/v1/usage, behind the API key middleware.
// The admin listener adds GET /admin/v1/providers, the deployments'
// health, and GET /admin/v1/usage, the usage report of any tenant, to its
// built-in routes.
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
	"switchyard/internal/config"
	"switchyard/internal/gateway"
	"switchyard/internal/server"
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
}

// ledger is a usage ledger: where events go, and what reports read.
type ledger interface {
	usage.Sink
	usage.Reporter
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

// New builds the service from snap, writing logs to logOutput.
//
// It also installs the service logger as slog's default logger, and logs the
// loaded configuration: "config loaded" at INFO, each snapshot warning at
// WARN and the effective configuration at DEBUG.
func New(snap *config.Snapshot, logOutput io.Writer) (*App, error) {
	return newApp(snap, logOutput, openStorage)
}

// storage is what the storage settings open: the usage ledger, the budget
// store and, with storage.mode redis, the client of the state instance.
type storage struct {
	ledger  ledger
	budgets budget.Store
	redis   *redis.Client
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
	return storage{ledger: led, budgets: budgets, redis: rdb}, nil
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
	a := &App{log: logger, srv: srv, ledger: led, redis: rdb, shutdownGrace: cfg.Server.ShutdownGrace.Std()}
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
	})
	if err != nil {
		_ = a.closeStorage(context.Background())
		return nil, fmt.Errorf("app: %w", err)
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
	srv.Handle("GET "+usageEndpoint, gw.Instrument(usageEndpoint, authenticate(usageapi.TenantHandler(led, usageapi.Options{}))))
	srv.HandleAdmin("GET /admin/v1/providers", gw.ProvidersHandler())
	srv.HandleAdmin("GET /admin/v1/usage", usageapi.AdminHandler(led, usageapi.Options{}))

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

// usageEndpoint is the public usage report's path.
const usageEndpoint = "/switchyard/v1/usage"

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
	return err
}

// Logger returns the service logger, which redacts secrets.
func (a *App) Logger() *slog.Logger { return a.log }

// Run serves until ctx is done, then shuts down gracefully: the server
// drains, then the usage ledger writes the events still queued, within
// the shutdown grace period. Canceling forceCtx aborts the requests still
// in flight and the flush. It returns nil after a clean shutdown,
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
