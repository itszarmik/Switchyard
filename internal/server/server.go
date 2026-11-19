// Package server runs Switchyard's two HTTP listeners: the public API
// listener, optionally over TLS, and the admin listener that serves
// /metrics, whose /admin/v1/* routes can require an admin token.
//
// Both share one middleware chain (tracing, request ID, access log and
// metrics, panic recovery, body limit) and the graceful shutdown sequence the deployment
// relies on: readiness fails first, the server keeps serving for
// ShutdownDelay so load balancers can stop routing to it, then in-flight
// requests get up to ShutdownGrace to finish.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
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

	"switchyard/internal/tracing"
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
	// ConfigVersion is reported by /readyz; SetConfigVersion changes it.
	ConfigVersion string
	// TLSCertFile and TLSKeyFile, when both set, serve the public listener
	// over HTTPS with that PEM certificate chain and key, read by New.
	// TLSMinVersion is the lowest TLS version accepted, tls.VersionTLS12
	// when zero. The admin listener is always plain HTTP.
	TLSCertFile, TLSKeyFile string
	TLSMinVersion           uint16
	// AdminToken, when set, must be sent as "Authorization: Bearer
	// <token>" on every /admin/v1/* route of the admin listener (see
	// HandleAdmin); SetAdminToken changes it.
	AdminToken string
	// Logger receives the access log and the server's own logs. Required.
	Logger *slog.Logger
	// Registry receives the HTTP metrics and is served at admin /metrics.
	// Required.
	Registry *prometheus.Registry
	// Tracing, when set, starts the root span of every request on both
	// listeners (tracing.Provider.Middleware), outermost in the chain, so
	// the request logger and the access log carry its trace_id. Nil
	// leaves requests untraced and an incoming traceparent unread.
	Tracing *tracing.Provider
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
	if (o.TLSCertFile == "") != (o.TLSKeyFile == "") {
		errs = append(errs, errors.New("TLSCertFile and TLSKeyFile must be set together"))
	}
	switch o.TLSMinVersion {
	case 0, tls.VersionTLS12, tls.VersionTLS13:
	default:
		errs = append(errs, fmt.Errorf("invalid TLSMinVersion %#x: must be TLS 1.2 or 1.3", o.TLSMinVersion))
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
	// configVersion is what /readyz reports. adminToken is the SHA-256 of
	// the admin token, or nil when /admin/v1/* is open.
	configVersion atomic.Pointer[string]
	adminToken    atomic.Pointer[[sha256.Size]byte]
	// adminGuard, when set, limits failed admin authentications.
	adminGuard AuthGuard
	// tls, when set, is the public listener's TLS configuration.
	tls *tls.Config

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
	s.SetConfigVersion(opts.ConfigVersion)
	s.SetAdminToken(opts.AdminToken)
	if opts.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("server: load the TLS certificate and key: %w", err)
		}
		minVersion := opts.TLSMinVersion
		if minVersion == 0 {
			minVersion = tls.VersionTLS12
		}
		s.tls = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: minVersion}
	}

	s.publicMux.HandleFunc(healthzPattern, s.handleHealthz)
	s.publicMux.HandleFunc(readyzPattern, s.handleReadyz)
	s.publicMux.HandleFunc(catchAllPattern, handleNotFound)
	// The admin listener serves the probes too, open like /metrics: it is
	// plain HTTP even when the public listener has TLS, so a container's
	// own healthcheck can always reach it.
	s.adminMux.HandleFunc(healthzPattern, s.handleHealthz)
	s.adminMux.HandleFunc(readyzPattern, s.handleReadyz)
	s.adminMux.Handle(metricsPattern, promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{Registry: opts.Registry}))
	s.adminMux.HandleFunc(catchAllPattern, handleNotFound)

	s.publicHandler = s.chain(listenerPublic, s.publicMux)
	s.adminHandler = s.chain(listenerAdmin, s.adminMux)
	s.public = s.newHTTPServer(listenerPublic, s.publicHandler)
	s.public.TLSConfig = s.tls
	s.admin = s.newHTTPServer(listenerAdmin, s.adminHandler)
	return s, nil
}

// SetConfigVersion sets the config version /readyz reports, after a
// reload.
func (s *Server) SetConfigVersion(v string) { s.configVersion.Store(&v) }

// ConfigVersion returns the config version /readyz reports.
func (s *Server) ConfigVersion() string { return *s.configVersion.Load() }

// SetAdminToken sets the token the /admin/v1/* routes require, after a
// reload; "" leaves them open. Only the token's SHA-256 is kept.
func (s *Server) SetAdminToken(token string) {
	if token == "" {
		s.adminToken.Store(nil)
		return
	}
	sum := sha256.Sum256([]byte(token))
	s.adminToken.Store(&sum)
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
// A route under /admin/v1/ requires the admin token, when one is set (see
// requireAdminToken), with the guard set by SetAdminGuard, if any.
func (s *Server) HandleAdmin(pattern string, h http.Handler) {
	s.mustNotBeStarted("HandleAdmin")
	if isAdminAPI(pattern) {
		h = s.requireAdminToken(h)
	}
	s.adminMux.Handle(pattern, h)
}

// AuthGuard limits failed authentications per client address;
// *ratelimit.AuthFailures implements it.
type AuthGuard interface {
	// Check is called with every request once its credential has been
	// checked, before anything is written, with whether it was valid. It
	// counts a failure, and when the address has too many it answers the
	// request itself (429) and returns false.
	Check(w http.ResponseWriter, r *http.Request, authenticated bool) bool
}

// SetAdminGuard puts the /admin/v1/* routes under g: a client address with
// too many failed admin authentications gets 429, whatever token it sends,
// and logs no line for it. It applies to the routes HandleAdmin registers
// afterwards, so it must be called before them, and before Start.
func (s *Server) SetAdminGuard(g AuthGuard) {
	s.mustNotBeStarted("SetAdminGuard")
	s.adminGuard = g
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
	s.log.Info("listening", "public_addr", s.publicAddr, "admin_addr", s.adminAddr, "public_tls", s.tls != nil)
	s.serve(listenerPublic, s.public, pub)
	s.serve(listenerAdmin, s.admin, adm)
	return nil
}

// serve runs srv on ln in a goroutine, over TLS when srv has a TLS
// configuration, reporting unexpected failures on s.serveErr.
func (s *Server) serve(listener string, srv *http.Server, ln net.Listener) {
	s.serving.Go(func() {
		var err error
		if srv.TLSConfig != nil {
			// The certificate is in TLSConfig, loaded by New.
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
