package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/apierror"
	"switchyard/internal/auth"
	"switchyard/internal/cache/semantic"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/eval"
	"switchyard/internal/gateway"
	"switchyard/internal/providers"
	"switchyard/internal/ratelimit"
	"switchyard/internal/server"
	"switchyard/internal/telemetry"
)

// Hot reload (DESIGN §14.8, §16.6). A reload, on SIGHUP or POST
// /admin/v1/reload, loads and validates the configuration again from the
// files and overrides the process started with, builds the gateway's new
// snapshot and the new API key store, and swaps both in. On any error the
// configuration in force keeps serving. In-flight requests finish on the
// snapshot they started with. Settings that only take effect at startup
// are not applied, but reported as restart_required.
//
// A request authenticates against the key store, then loads the gateway's
// snapshot, and a reload swaps the gateway first and the key store
// second. So a request admitted by the new key store always meets the new
// snapshot, and a request admitted by the old one may meet either. The
// gateway checks that the snapshot it loads knows the request's tenant and
// key, and answers 401 invalid_api_key, before routing or charging, when
// it does not: a key or tenant a reload removed is never served under a
// snapshot without it (its budgets and policy), not even by a request
// that authenticated just before the reload.

// Loader loads and validates the configuration again, with the same code
// and from the same sources as at startup, for a reload.
type Loader func() (*config.Snapshot, error)

// reloadEndpoint is the admin route that reloads the configuration.
const reloadEndpoint = "POST /admin/v1/reload"

// auditReload is the audit action of a reload.
const auditReload = "config.reload"

// Reload results, the metric's labels.
const (
	reloadOK    = "ok"
	reloadError = "error"
)

// ReloadResult is the outcome of a successful reload.
type ReloadResult struct {
	// Fingerprint is the version of the configuration now in force.
	Fingerprint string `json:"fingerprint"`
	// RestartRequired lists the config paths whose changes, compared with
	// the configuration the process started with, are not applied until a
	// restart. It is never nil, so it encodes as [].
	RestartRequired []string `json:"restart_required"`
	// Unchanged is set when the configuration was identical to the one in
	// force, which was then kept. It is not set when a dated price change
	// had taken effect: the snapshot was then rebuilt with the new prices.
	Unchanged bool `json:"unchanged,omitempty"`
}

// reloader is the App's reload state.
type reloader struct {
	// mu serializes reloads.
	mu   sync.Mutex
	load Loader
	// start is the configuration the process started with, for the
	// restart-required diff; cfg is the configuration in force, which a
	// price switch builds the snapshot from again, running its version,
	// and secrets the secret values of the configuration in force,
	// scrubbed from reload errors.
	start   *config.Config
	cfg     *config.Config
	running string
	secrets []string
	// stage stages a gateway snapshot with the prices in force at a time
	// (gateway.Gateway.StageAt); tests replace it.
	stage func(cfg *config.Config, configVersion string, at time.Time) (*gateway.Staged, error)
	// keys is the API key store the auth middleware reads.
	keys *auth.Keyring
	// hasShadow and hasSemantic are whether the shadow runner and the
	// semantic cache were built at startup.
	hasShadow, hasSemantic bool

	now         func() time.Time
	total       *prometheus.CounterVec
	lastSuccess prometheus.Gauge
	// priceSwitches counts the price switches (see switchPrices).
	priceSwitches *prometheus.CounterVec
}

// newReloadMetrics registers the reload metrics.
func newReloadMetrics(reg prometheus.Registerer) (*prometheus.CounterVec, prometheus.Gauge, error) {
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_config_reloads_total",
		Help: "Configuration reloads, by result (ok or error).",
	}, []string{"result"})
	last := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "switchyard_config_last_reload_success_timestamp_seconds",
		Help: "Unix time of the last successful configuration load: startup or reload.",
	})
	for _, c := range []prometheus.Collector{total, last} {
		if err := reg.Register(c); err != nil {
			return nil, nil, fmt.Errorf("app: register the reload metrics: %w", err)
		}
	}
	total.WithLabelValues(reloadOK)
	total.WithLabelValues(reloadError)
	return total, last, nil
}

// errNoLoader is returned by a reload of an App built without a Loader.
var errNoLoader = errors.New("reload is not available: the process was started without a config file to reload")

// Reload loads the configuration again and swaps it in (see the package
// documentation and ReloadResult). Concurrent reloads run one at a time.
// The error, if any, is safe to show to an admin: it names config paths
// and problems, never secret values.
func (a *App) Reload() (ReloadResult, error) {
	r := &a.reload
	r.mu.Lock()
	defer r.mu.Unlock()
	res, err := a.doReload()
	if err != nil {
		r.total.WithLabelValues(reloadError).Inc()
		return ReloadResult{}, errors.New(scrub(err.Error(), r.secrets))
	}
	r.total.WithLabelValues(reloadOK).Inc()
	r.lastSuccess.Set(float64(r.now().UnixNano()) / 1e9)
	return res, nil
}

// doReload is Reload, with a.reload.mu held.
func (a *App) doReload() (ReloadResult, error) {
	r := &a.reload
	if r.load == nil {
		return ReloadResult{}, errNoLoader
	}
	snap, err := r.load()
	if err != nil {
		return ReloadResult{}, err
	}
	cfg := snap.Config()
	// The new configuration's secrets are scrubbed from the logs from now
	// on, whether or not it is applied: a rejected reload's error and
	// any later line may hold them. Secrets are only ever added.
	telemetry.AddSecrets(a.log, cfg.SecretValues())
	// The admin listener keeps its startup address until a restart, so a
	// token that guards an exposed one cannot be removed by a reload.
	if !cfg.Server.AdminToken.IsSet() && !isLoopbackAddr(r.start.Server.AdminListen) {
		return ReloadResult{}, errors.New("server.admin_token: required while the admin listener is bound to a non-loopback address (a change of server.admin_listen needs a restart)")
	}
	res := ReloadResult{Fingerprint: snap.Version(), RestartRequired: a.restartRequired(cfg)}
	// The same configuration is kept, unless a dated price change has
	// taken effect since its snapshot was built: then the snapshot is
	// rebuilt, as the next price check would. The clock is read once, so
	// the change counted as due is the one the staged catalog applies.
	now := a.clock.now()
	pricesDue := a.priceChangeDue(now)
	if snap.Version() == r.running && !pricesDue {
		res.Unchanged = true
		return res, nil
	}
	previous, previousCfg := a.gw.Catalog(), r.cfg
	staged, err := r.stage(cfg, snap.Version(), now)
	if err != nil {
		return ReloadResult{}, err
	}
	if err := a.checkStaged(staged, cfg); err != nil {
		return ReloadResult{}, err
	}
	keys := auth.NewStore(cfg.Tenants)

	// Swap: the gateway first, so a key that the new store admits finds
	// its tenant's new policy. A request the old store admitted with a
	// key the new snapshot lacks is refused by the gateway (see the
	// package documentation).
	staged.Commit()
	r.keys.Swap(keys)
	a.limits.Store(ratelimit.NewLimits(cfg))
	a.budgetFailClosed.Store(cfg.Budgets.FailMode == config.BudgetFailClosed)
	a.srv.SetConfigVersion(snap.Version())
	a.srv.SetAdminToken(cfg.Server.AdminToken.Value())
	r.cfg = cfg
	r.running = snap.Version()
	r.secrets = cfg.SecretValues()
	if pricesDue {
		// The switch the reload made: the configuration in force before
		// it at now, so that prices the reload itself edits are not
		// reported as the dated change (they are the reload's).
		switched := staged.Catalog()
		if c, err := catalog.NewAt(previousCfg, now); err == nil {
			switched = c
		}
		a.logPriceSwitch(previous, switched, priceTriggerReload)
	}
	a.rearmPrices()
	return res, nil
}

// checkStaged makes the checks startup makes with the catalog on a staged
// snapshot of cfg, before a reload or a price switch commits it: the
// judge must suit every sampling policy, and the embedding deployment
// every policy that enables the semantic cache.
func (a *App) checkStaged(staged *gateway.Staged, cfg *config.Config) error {
	if _, err := eval.ShadowJudge(staged.Catalog(), cfg); err != nil {
		return err
	}
	if semantic.Enabled(cfg) {
		adapters := make(map[string]providers.Adapter, len(cfg.Providers))
		for _, p := range cfg.Providers {
			if ad := staged.Adapter(p.ID); ad != nil {
				adapters[p.ID] = ad
			}
		}
		if _, _, err := semantic.ConfiguredDeployment(cfg, staged.Catalog(), adapters); err != nil {
			return err
		}
	}
	return nil
}

// reloadAndAudit runs a reload, then logs its outcome and writes its audit
// line through audit.
func (a *App) reloadAndAudit(audit func(result string, attrs ...slog.Attr)) (ReloadResult, error) {
	previous := a.gw.ConfigVersion()
	res, err := a.Reload()
	if err != nil {
		// The configuration in force, config_version, keeps serving.
		a.log.Error("config reload failed", "config_version", previous, "error", err.Error())
		audit(server.AuditError, slog.String("fingerprint", previous), slog.String("error", err.Error()))
		return res, err
	}
	a.log.Info("config reloaded", "config_version", res.Fingerprint, "previous_config_version", previous,
		"unchanged", res.Unchanged, "restart_required", res.RestartRequired)
	audit(server.AuditOK, slog.String("fingerprint", res.Fingerprint), slog.String("previous_fingerprint", previous),
		slog.Any("restart_required", res.RestartRequired))
	return res, nil
}

// ReloadOnSignal reloads for a SIGHUP, logging the outcome and the audit
// line; it never stops the process.
func (a *App) ReloadOnSignal() {
	_, _ = a.reloadAndAudit(func(result string, attrs ...slog.Attr) {
		server.AuditWithout(a.log, auditReload, result, append(attrs, slog.String("trigger", "signal"))...)
	})
}

// reloadResponse is the body of POST /admin/v1/reload.
type reloadResponse struct {
	Result string `json:"result"`
	ReloadResult
	Error string `json:"error,omitempty"`
}

// reloadHandler serves POST /admin/v1/reload. It reads no body. It
// answers 200 {"result":"ok","fingerprint":…,"restart_required":[…]}, or
// 400 {"result":"error","error":"…"} when the configuration was rejected
// and the old one keeps serving.
func (a *App) reloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, err := a.reloadAndAudit(func(result string, attrs ...slog.Attr) {
			server.Audit(r, auditReload, result, append(attrs, slog.String("trigger", "admin"))...)
		})
		status, body := http.StatusOK, reloadResponse{Result: reloadOK, ReloadResult: res}
		if err != nil {
			status, body = http.StatusBadRequest, reloadResponse{Result: reloadError, Error: err.Error()}
		}
		data, merr := json.Marshal(body)
		if merr != nil {
			apierror.Write(w, http.StatusInternalServerError, apierror.TypeServer, "internal_error", "Could not encode the reload result.")
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write(append(data, '\n'))
	})
}

// scrub replaces every secret value in s.
func scrub(s string, secrets []string) string {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}

// restartRequired returns the paths of next's changes, compared with the
// configuration the process started with, that need a restart: each leaf
// of config.RestartOnly that differs, and the features whose machinery is only
// built at startup when next turns them on.
func (a *App) restartRequired(next *config.Config) []string {
	start := a.reload.start
	out := []string{}
	for _, p := range config.RestartOnly() {
		diffPaths(p, lookupPath(start, p), lookupPath(next, p), &out)
	}
	if !a.reload.hasSemantic && semantic.Enabled(next) {
		out = append(out, "policies[].cache.semantic")
	}
	if !a.reload.hasShadow && slices.ContainsFunc(next.Policies, func(p config.Policy) bool { return p.Shadow.SampleRate > 0 }) {
		out = append(out, "policies[].shadow.sample_rate")
	}
	// With storage.mode redis, the cache Redis client exists only if some
	// policy cached at startup; otherwise entries stay in process.
	if start.Storage.Mode == "redis" && !cachingEnabled(start, false) && cachingEnabled(next, false) {
		out = append(out, "policies[].cache")
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// lookupPath returns the field of c at the dotted yaml path p.
func lookupPath(c *config.Config, p string) reflect.Value {
	v := reflect.ValueOf(c).Elem()
	for name := range strings.SplitSeq(p, ".") {
		v = fieldByYAML(v, name)
		if !v.IsValid() {
			panic("app: no config field " + p)
		}
	}
	return v
}

// fieldByYAML returns the field of struct v whose yaml name is name.
func fieldByYAML(v reflect.Value, name string) reflect.Value {
	t := v.Type()
	for i := range t.NumField() {
		if tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); tag == name {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// diffPaths appends to out the path of every leaf where a and b differ.
// A struct with yaml-tagged fields is walked; anything else (scalars,
// lists, maps and opaque structs such as Secret) is compared whole.
func diffPaths(path string, a, b reflect.Value, out *[]string) {
	if a.Kind() == reflect.Struct && hasYAMLFields(a.Type()) {
		t := a.Type()
		for i := range t.NumField() {
			tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			diffPaths(path+"."+tag, a.Field(i), b.Field(i), out)
		}
		return
	}
	if !reflect.DeepEqual(a.Interface(), b.Interface()) {
		*out = append(*out, path)
	}
}

// hasYAMLFields reports whether struct type t has a yaml-tagged field.
func hasYAMLFields(t reflect.Type) bool {
	for i := range t.NumField() {
		if t.Field(i).IsExported() && t.Field(i).Tag.Get("yaml") != "" {
			return true
		}
	}
	return false
}

// isLoopbackAddr reports whether the listen address addr (host:port) binds
// only a loopback interface: 127.0.0.0/8, ::1 or localhost. The empty host
// and 0.0.0.0 bind every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}
