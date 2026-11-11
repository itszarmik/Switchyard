// Package server runs Switchyard's two HTTP listeners: the public API
// listener and the admin listener that serves /metrics.
//
// Both share one middleware chain (request ID, access log and metrics, panic
// recovery, body limit) and the graceful shutdown sequence the deployment
// relies on: readiness fails first, the server keeps serving for
// ShutdownDelay so load balancers can stop routing to it, then in-flight
// requests get up to ShutdownGrace to finish.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Listener names, used in logs and metric labels.
const (
	listenerPublic = "public"
	listenerAdmin  = "admin"
)

// ErrShutdownTimeout is returned when in-flight requests had to be aborted.
// Connections that had not sent a request yet are closed without it.
var ErrShutdownTimeout = errors.New("server: shutdown grace period exceeded; in-flight requests were aborted")

// Options configures a Server.
type Options struct {
	// Listen and AdminListen are the public and admin addresses (host:port).
	// Port 0 picks a free port; PublicAddr and AdminAddr report it.
	Listen, AdminListen string
	// MaxRequestBytes caps request bodies; larger ones get a 413.
	MaxRequestBytes int64
	// MaxHeaderBytes is http.Server.MaxHeaderBytes; larger headers get a 431.
	MaxHeaderBytes int
	// ReadHeaderTimeout bounds how long a client may take to send headers.
	ReadHeaderTimeout time.Duration
	// IdleTimeout closes idle keep-alive connections.
	IdleTimeout time.Duration
	// ShutdownDelay is how long the server keeps serving, with /readyz
	// failing, after shutdown starts. Zero means no delay.
	ShutdownDelay time.Duration
	// ShutdownGrace is the longest Shutdown waits for in-flight requests
	// after the listeners close.
	ShutdownGrace time.Duration
	// ConfigVersion is reported by /readyz.
	ConfigVersion string
	// Logger receives the access log and the server's own logs. Required.
	Logger *slog.Logger
	// Registry receives the HTTP metrics and is served at admin /metrics.
	// Required.
	Registry *prometheus.Registry
}

// validate reports every problem with o.
func (o Options) validate() error {
	var errs []error
	if o.Listen == "" {
		errs = append(errs, errors.New("missing Listen address"))
	}
	if o.AdminListen == "" {
		errs = append(errs, errors.New("missing AdminListen address"))
	}
	if o.MaxRequestBytes <= 0 {
		errs = append(errs, fmt.Errorf("invalid MaxRequestBytes %d: must be positive", o.MaxRequestBytes))
	}
	if o.MaxHeaderBytes <= 0 {
		errs = append(errs, fmt.Errorf("invalid MaxHeaderBytes %d: must be positive", o.MaxHeaderBytes))
	}
	if o.ReadHeaderTimeout <= 0 {
		errs = append(errs, fmt.Errorf("invalid ReadHeaderTimeout %s: must be positive", o.ReadHeaderTimeout))
	}
	if o.IdleTimeout <= 0 {
		errs = append(errs, fmt.Errorf("invalid IdleTimeout %s: must be positive", o.IdleTimeout))
	}
	if o.ShutdownDelay < 0 {
		errs = append(errs, fmt.Errorf("invalid ShutdownDelay %s: must not be negative", o.ShutdownDelay))
	}
	if o.ShutdownGrace <= 0 {
		errs = append(errs, fmt.Errorf("invalid ShutdownGrace %s: must be positive", o.ShutdownGrace))
	}
	if o.Logger == nil {
		errs = append(errs, errors.New("missing Logger"))
	}
	if o.Registry == nil {
		errs = append(errs, errors.New("missing Registry"))
	}
	return errors.Join(errs...)
}

// Server is the pair of HTTP listeners. Create it with New, register routes
// and readiness checks, then call Run (or Start and Shutdown).
type Server struct {
	opts    Options
	log     *slog.Logger
	metrics *httpMetrics

	publicMux, adminMux *http.ServeMux
	public, admin       *http.Server
	// publicHandler and adminHandler are the full middleware chains.
	publicHandler, adminHandler http.Handler

	// Seams for tests; New sets the production values.
	listen       func(network, address string) (net.Listener, error)
	sleep        func(ctx context.Context, d time.Duration)
	checkTimeout time.Duration

	inFlight     atomic.Int64
	shuttingDown atomic.Bool

	mu                    sync.Mutex
	started               bool
	checks                []readinessCheck
	publicAddr, adminAddr string

	serveErr chan error // unexpected Serve failures
	serving  sync.WaitGroup

	shutdownOnce sync.Once
	shutdownErr  error
}

// New validates opts, builds both muxes and the middleware, and registers
// the built-in routes and the HTTP metrics.
func New(opts Options) (*Server, error) {
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("server: invalid options: %w", err)
	}
	metrics, err := newHTTPMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}

	s := &Server{
		opts:         opts,
		log:          opts.Logger,
		metrics:      metrics,
		publicMux:    http.NewServeMux(),
		adminMux:     http.NewServeMux(),
		listen:       net.Listen,
		sleep:        sleepContext,
		checkTimeout: defaultCheckTimeout,
		serveErr:     make(chan error, 2),
	}

	s.publicMux.HandleFunc(healthzPattern, s.handleHealthz)
	s.publicMux.HandleFunc(readyzPattern, s.handleReadyz)
	s.publicMux.HandleFunc(catchAllPattern, handleNotFound)
	s.adminMux.Handle(metricsPattern, promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{Registry: opts.Registry}))
	s.adminMux.HandleFunc(catchAllPattern, handleNotFound)

	s.publicHandler = s.chain(listenerPublic, s.publicMux)
	s.adminHandler = s.chain(listenerAdmin, s.adminMux)
	s.public = s.newHTTPServer(listenerPublic, s.publicHandler)
	s.admin = s.newHTTPServer(listenerAdmin, s.adminHandler)
	return s, nil
}

func (s *Server) newHTTPServer(listener string, h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		MaxHeaderBytes:    s.opts.MaxHeaderBytes,
		ReadHeaderTimeout: s.opts.ReadHeaderTimeout,
		IdleTimeout:       s.opts.IdleTimeout,
		// ReadTimeout and WriteTimeout are deliberately left at 0. They
		// bound the whole request body read and the whole response write,
		// which would cut off long uploads and long-running streamed (SSE)
		// completions. Slow clients are bounded by ReadHeaderTimeout and
		// IdleTimeout here, and streaming handlers set per-write deadlines
		// through http.ResponseController.
		ErrorLog: slog.NewLogLogger(s.log.With("listener", listener).Handler(), slog.LevelWarn),
	}
}

// Handle registers h on the public mux. pattern uses Go 1.22 ServeMux syntax,
// such as "POST /v1/chat/completions". It must be called before Start; it
// panics afterwards, or if the pattern is invalid or conflicts with another.
func (s *Server) Handle(pattern string, h http.Handler) {
	s.mustNotBeStarted("Handle")
	s.publicMux.Handle(pattern, h)
}

// HandleAdmin registers h on the admin mux, with the same rules as Handle.
func (s *Server) HandleAdmin(pattern string, h http.Handler) {
	s.mustNotBeStarted("HandleAdmin")
	s.adminMux.Handle(pattern, h)
}

// AddReadinessCheck adds a check that /readyz runs. Checks run concurrently
// with a 2s timeout each, and any failure makes /readyz return 503 naming the
// check. It must be called before Start; it panics afterwards, or if name is
// empty or check is nil.
func (s *Server) AddReadinessCheck(name string, check func(ctx context.Context) error) {
	if name == "" || check == nil {
		panic("server: AddReadinessCheck needs a name and a check")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		panic("server: AddReadinessCheck called after Start")
	}
	s.checks = append(s.checks, readinessCheck{name: name, check: check})
}

func (s *Server) mustNotBeStarted(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		panic("server: " + method + " called after Start")
	}
}

// Start binds both listeners and serves them in background goroutines. If
// either address cannot be bound, the other listener is closed and the error
// returned. It logs "listening" with the bound public_addr and admin_addr.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("server: already started")
	}
	if s.shuttingDown.Load() {
		return errors.New("server: already shut down")
	}

	pub, err := s.listen("tcp", s.opts.Listen)
	if err != nil {
		return fmt.Errorf("server: listen on public address %s: %w", s.opts.Listen, err)
	}
	adm, err := s.listen("tcp", s.opts.AdminListen)
	if err != nil {
		_ = pub.Close()
		return fmt.Errorf("server: listen on admin address %s: %w", s.opts.AdminListen, err)
	}

	s.started = true
	s.publicAddr = pub.Addr().String()
	s.adminAddr = adm.Addr().String()
	s.log.Info("listening", "public_addr", s.publicAddr, "admin_addr", s.adminAddr)
	s.serve(listenerPublic, s.public, pub)
	s.serve(listenerAdmin, s.admin, adm)
	return nil
}

// serve runs srv on ln in a goroutine, reporting unexpected failures on
// s.serveErr.
func (s *Server) serve(listener string, srv *http.Server, ln net.Listener) {
	s.serving.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- fmt.Errorf("server: %s listener failed: %w", listener, err)
		}
	})
}

// PublicAddr returns the bound public address, or "" before Start.
func (s *Server) PublicAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publicAddr
}

// AdminAddr returns the bound admin address, or "" before Start.
func (s *Server) AdminAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adminAddr
}

// Run starts the server and blocks until ctx is done or a listener fails.
// When ctx is done it returns the result of Shutdown(forceCtx). When a
// listener fails it shuts down, skipping ShutdownDelay since the server can
// no longer serve normally, and returns the listener's error (joined with
// ErrShutdownTimeout if the drain also timed out). forceCtx is typically
// canceled by a second SIGTERM or SIGINT.
func (s *Server) Run(ctx, forceCtx context.Context) error {
	if err := s.Start(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return s.Shutdown(forceCtx)
	case err := <-s.serveErr:
		s.log.Error("listener failed; shutting down", "error", err)
		if serr := s.shutdown(forceCtx, false); serr != nil {
			return errors.Join(err, serr)
		}
		return err
	}
}

// Shutdown stops the server gracefully:
//
//  1. /readyz starts returning 503 at once, and "shutdown started" is logged
//     with the number of in-flight requests.
//  2. Every route keeps being served for ShutdownDelay, cut short if
//     forceCtx is done.
//  3. Both listeners close, and in-flight requests get up to ShutdownGrace
//     to finish; canceling forceCtx ends the wait early.
//  4. Any connections still open at that point are closed. If requests were
//     still running on them, they are aborted and Shutdown returns
//     ErrShutdownTimeout. Otherwise, including when only connections that
//     never sent a request were left, it logs "shutdown complete" and
//     returns nil.
//
// Shutdown is idempotent and safe to call concurrently: later callers wait
// for the first to finish and get the same result.
func (s *Server) Shutdown(forceCtx context.Context) error {
	return s.shutdown(forceCtx, true)
}

func (s *Server) shutdown(forceCtx context.Context, withDelay bool) error {
	s.shutdownOnce.Do(func() {
		s.shutdownErr = s.doShutdown(forceCtx, withDelay)
	})
	return s.shutdownErr
}

func (s *Server) doShutdown(forceCtx context.Context, withDelay bool) error {
	if forceCtx == nil {
		forceCtx = context.Background()
	}
	// Under mu, so a concurrent Start either finished first or sees the flag
	// and does not start serving.
	s.mu.Lock()
	s.shuttingDown.Store(true)
	s.mu.Unlock()

	s.log.Info("shutdown started",
		"in_flight", s.inFlight.Load(),
		"delay", s.opts.ShutdownDelay.String(),
		"grace", s.opts.ShutdownGrace.String(),
	)

	if withDelay && s.opts.ShutdownDelay > 0 {
		s.sleep(forceCtx, s.opts.ShutdownDelay)
	}

	graceCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownGrace)
	defer cancel()
	stop := context.AfterFunc(forceCtx, cancel)
	defer stop()

	var publicErr, adminErr error
	var wg sync.WaitGroup
	wg.Go(func() { publicErr = s.public.Shutdown(graceCtx) })
	wg.Go(func() { adminErr = s.admin.Shutdown(graceCtx) })
	wg.Wait()

	if isContextError(publicErr) || isContextError(adminErr) {
		// net/http also waits for connections that have not sent a request
		// yet, such as a load balancer's pre-connect, so the deadline can
		// pass with no request in flight. Close the remaining connections
		// either way, but only report requests that were really aborted.
		aborted := s.inFlight.Load()
		_ = s.public.Close()
		_ = s.admin.Close()
		s.serving.Wait()
		if aborted > 0 {
			reason := "grace_period_exceeded"
			if forceCtx.Err() != nil {
				reason = "forced"
			}
			s.log.Warn("shutdown aborted in-flight requests", "aborted", aborted, "reason", reason)
			return ErrShutdownTimeout
		}
		s.log.Info("shutdown complete")
		return nil
	}

	s.serving.Wait()
	if err := errors.Join(publicErr, adminErr); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}
	s.log.Info("shutdown complete")
	return nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// sleepContext waits for d or until ctx is done, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
