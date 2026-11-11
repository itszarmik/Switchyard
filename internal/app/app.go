// Package app is Switchyard's composition root. It turns a validated
// configuration snapshot into the running service: the redacting logger, the
// metrics registry and the HTTP server, wired together.
//
// The public listener serves the built-in health routes without
// authentication and the OpenAI-compatible /v1 routes behind the API key
// middleware. The admin listener adds GET /admin/v1/providers, the
// deployments' health, to its built-in routes.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"switchyard/internal/api/openaiapi"
	"switchyard/internal/auth"
	"switchyard/internal/config"
	"switchyard/internal/gateway"
	"switchyard/internal/server"
	"switchyard/internal/telemetry"
	"switchyard/internal/version"
)

// App is the wired service. Create it with New and start it with Run.
type App struct {
	log *slog.Logger
	srv *server.Server
}

// New builds the service from snap, writing logs to logOutput.
//
// It also installs the service logger as slog's default logger, and logs the
// loaded configuration: "config loaded" at INFO, each snapshot warning at
// WARN and the effective configuration at DEBUG.
func New(snap *config.Snapshot, logOutput io.Writer) (*App, error) {
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

	gw, err := gateway.New(gateway.Options{Config: cfg, Logger: logger, Registry: registry})
	if err != nil {
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
	srv.HandleAdmin("GET /admin/v1/providers", gw.ProvidersHandler())

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

	return &App{log: logger, srv: srv}, nil
}

// Logger returns the service logger, which redacts secrets.
func (a *App) Logger() *slog.Logger { return a.log }

// Run serves until ctx is done, then shuts down gracefully. Canceling
// forceCtx aborts the requests still in flight. It returns nil after a clean
// shutdown and server.ErrShutdownTimeout if requests had to be aborted.
func (a *App) Run(ctx, forceCtx context.Context) error {
	info := version.Get()
	a.log.Info("switchyard starting",
		"version", info.Version,
		"commit", info.Commit,
		"date", info.Date,
		"go_version", info.GoVersion,
	)
	return a.srv.Run(ctx, forceCtx)
}
