package config

import (
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	// idRE is the pattern for provider, tenant and API key ids.
	idRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	tagRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	keyHashRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	modelIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
)

// Messages shared by several checks.
const (
	msgRequired          = "required"
	msgAdminExposed      = "admin listener is reachable from other hosts; never expose it publicly"
	msgSameRedis         = "both Redis roles point at the same instance; eviction policy is per instance, so this is unsafe for production"
	msgLiteralCredential = "looks like a literal credential; use ${ENV_VAR} or ${file:...}"
	msgLiteralPassword   = "embeds a literal password; use ${ENV_VAR} or ${file:...} for it" //nolint:gosec // G101: a warning message, not a credential
	msgNoTenants         = "no API keys configured; every /v1 request will be rejected with 401 (create one with `switchyard keys generate`)"
	msgTenantNoKeys      = "tenant has no keys; no client can authenticate as it"
	msgBadKeyHash        = "must be sha256: followed by 64 lowercase hex characters"
	msgPlaintextKey      = "; this looks like a plaintext API key: never store the key itself, only the hash printed by `switchyard keys generate`"
)

// ValidID reports whether id is a valid provider, tenant or API key id: 1 to
// 63 characters from [a-z0-9_-], starting with a letter or digit.
func ValidID(id string) bool { return idRE.MatchString(id) }

// forbiddenForwardHeaders can never be forwarded upstream.
var forbiddenForwardHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
}

// credentialPrefixes are the prefixes of well-known credential formats.
var credentialPrefixes = []string{"sk-", "sk-ant-", "sy_", "AIza", "gsk_", "xai-"}

// credentialRE matches the credential shapes the log redactor scrubs
// anywhere in a string: bearer tokens and sk-…, sy_… and AIza… keys.
var credentialRE = regexp.MustCompile(`(?i:\bbearer\s+\S{8,})|sk-[A-Za-z0-9_-]{16,}|sy_[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{30,}`)

// maxQuoted is the longest value or key a problem message quotes.
const maxQuoted = 64

// hiddenKey stands in, in config paths, for a mapping key that looks like a
// credential.
const hiddenKey = "<hidden key>"

// mayQuote reports whether s, a value or key written in a config file or an
// override, may be quoted in a problem message: it is short, printable
// ASCII, and does not look like a credential.
func mayQuote(s string) bool {
	if len(s) > maxQuoted {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c < ' ' || c > '~' {
			return false
		}
	}
	return !looksLikeCredential(s)
}

// looksLikeCredential reports whether s looks like a credential: it has a
// well-known key prefix or a long token run (the literal-credential
// heuristics), contains a credential shape the log redactor scrubs, or is a
// URL with userinfo.
func looksLikeCredential(s string) bool {
	if looksLikeLiteralCredential([]segment{{text: s}}) || credentialRE.MatchString(s) {
		return true
	}
	if i := strings.Index(s, "://"); i >= 0 {
		auth := s[i+3:]
		if j := strings.IndexAny(auth, "/?#"); j >= 0 {
			auth = auth[:j]
		}
		return strings.Contains(auth, "@")
	}
	return false
}

// shownKey returns key for use in a config path, or hiddenKey if key looks
// like a credential.
func shownKey(key string) string {
	if mayQuote(key) {
		return key
	}
	return hiddenKey
}

// validate checks the effective configuration and records every problem.
func (st *loadState) validate(c *Config) {
	switch {
	case c.Version == 0:
		st.errorAt("version", "required; set version: 1")
	case c.Version != 1:
		st.errorAt("version", fmt.Sprintf("unsupported version %d; only version 1 is defined", c.Version))
	}
	st.validateServer(&c.Server)
	st.validateLogging(&c.Logging)
	st.validateStorage(&c.Storage)
	providers := st.validateProviders(c.Providers)
	models, modelPaths := st.validateModels(c.Models, providers)
	st.validateAliases(c.Aliases, models, modelPaths)
	st.validateTenants(c.Tenants)
	st.validateBudgets(&c.Budgets)
	st.validateRouting(&c.Routing)
	st.checkLiteralSecrets(c)
}

// Upper bounds of the routing settings.
const (
	maxPlanDeployments       = 10
	maxRetriesPerDeployment  = 5
	maxRetryBudgetMin        = 10000
	maxBreakerCount          = 1000000
	maxHalfOpenProbes        = 1000
	maxRoutingIntervalLength = 24 * time.Hour
)

func (st *loadState) validateRouting(r *Routing) {
	st.checkDurationRange("routing.request_timeout", r.RequestTimeout, maxRoutingIntervalLength)
	f := &r.Fallback
	st.checkIntRange("routing.fallback.max_deployments", f.MaxDeployments, 1, maxPlanDeployments)
	st.checkIntRange("routing.fallback.retries_per_deployment", f.RetriesPerDeployment, 0, maxRetriesPerDeployment)
	st.checkDurationRange("routing.fallback.backoff_base", f.BackoffBase, maxRoutingIntervalLength)
	st.checkDurationRange("routing.fallback.backoff_max", f.BackoffMax, maxRoutingIntervalLength)
	if f.BackoffBase > 0 && f.BackoffMax > 0 && f.BackoffMax < f.BackoffBase {
		st.errorAt("routing.fallback.backoff_max", "must not be less than routing.fallback.backoff_base")
	}
	st.checkRatio("routing.fallback.retry_budget_ratio", f.RetryBudgetRatio)
	st.checkIntRange("routing.fallback.retry_budget_min", f.RetryBudgetMin, 0, maxRetryBudgetMin)

	b := &r.Breaker
	st.checkIntRange("routing.breaker.consecutive_failures", b.ConsecutiveFailures, 1, maxBreakerCount)
	st.checkRatio("routing.breaker.failure_ratio", b.FailureRatio)
	st.checkIntRange("routing.breaker.min_requests", b.MinRequests, 1, maxBreakerCount)
	st.checkDurationRange("routing.breaker.window", b.Window, maxRoutingIntervalLength)
	st.checkDurationRange("routing.breaker.open_for", b.OpenFor, maxRoutingIntervalLength)
	st.checkIntRange("routing.breaker.half_open_probes", b.HalfOpenProbes, 1, maxHalfOpenProbes)

	c := &r.RateLimitCooldown
	st.checkDurationRange("routing.rate_limit_cooldown.default", c.Default, maxRoutingIntervalLength)
	st.checkDurationRange("routing.rate_limit_cooldown.max", c.Max, maxRoutingIntervalLength)
	if c.Default > 0 && c.Max > 0 && c.Default > c.Max {
		st.errorAt("routing.rate_limit_cooldown.default", "must not be greater than routing.rate_limit_cooldown.max")
	}
	switch {
	case r.StreamKeepalive < 0:
		st.errorAt("routing.stream_keepalive", "must not be negative; use 0s to turn keep-alives off")
	case r.StreamKeepalive > 0 && r.StreamKeepalive < Duration(minStreamKeepalive):
		st.errorAt("routing.stream_keepalive", fmt.Sprintf("must be 0s (off) or at least %s", minStreamKeepalive))
	case time.Duration(r.StreamKeepalive) > maxRoutingIntervalLength:
		st.errorAt("routing.stream_keepalive", fmt.Sprintf("must be at most %s", maxRoutingIntervalLength))
	}
}

// minStreamKeepalive keeps keep-alives from flooding the client.
const minStreamKeepalive = 10 * time.Millisecond

// checkIntRange reports an error unless lo <= v <= hi.
func (st *loadState) checkIntRange(path string, v, lo, hi int) {
	if v < lo || v > hi {
		st.errorAt(path, fmt.Sprintf("must be between %d and %d, got %d", lo, hi, v))
	}
}

// checkDurationRange reports an error unless 0 < d <= hi.
func (st *loadState) checkDurationRange(path string, d Duration, hi time.Duration) {
	switch {
	case d <= 0:
		st.errorAt(path, "must be greater than 0")
	case time.Duration(d) > hi:
		st.errorAt(path, fmt.Sprintf("must be at most %s", hi))
	}
}

// checkRatio reports an error unless 0 < v <= 1.
func (st *loadState) checkRatio(path string, v float64) {
	if v <= 0 || v > 1 {
		st.errorAt(path, fmt.Sprintf("must be greater than 0 and at most 1, got %s", strconv.FormatFloat(v, 'g', -1, 64)))
	}
}

func (st *loadState) validateServer(s *Server) {
	lHost, lPort, lOK := st.checkHostPort("server.listen", s.Listen)
	aHost, aPort, aOK := st.checkHostPort("server.admin_listen", s.AdminListen)
	if lOK && aOK && lPort == aPort && lPort != 0 &&
		(lHost == aHost || isUnspecifiedHost(lHost) || isUnspecifiedHost(aHost)) {
		st.errorAt("server.admin_listen", "must not use the same address as server.listen")
	}
	if aOK && !isLoopbackHost(aHost) {
		st.warnAt("server.admin_listen", msgAdminExposed)
	}

	st.checkByteRange("server.max_request_bytes", s.MaxRequestBytes, kib, gib)
	st.checkByteRange("server.max_header_bytes", s.MaxHeaderBytes, 4*kib, mib)
	st.checkPositive("server.read_header_timeout", s.ReadHeaderTimeout)
	st.checkPositive("server.body_read_timeout", s.BodyReadTimeout)
	st.checkPositive("server.write_timeout", s.WriteTimeout)
	st.checkPositive("server.idle_timeout", s.IdleTimeout)
	st.checkPositive("server.shutdown_grace", s.ShutdownGrace)
	if s.ShutdownDelay < 0 {
		st.errorAt("server.shutdown_delay", "must not be negative")
	}
}

// checkHostPort validates a listen address and returns its host and port.
func (st *loadState) checkHostPort(path, addr string) (host string, port uint64, ok bool) {
	msg := "must be host:port with a port from 0 to 65535, got " + st.describe(path, addr)
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		st.errorAt(path, msg)
		return "", 0, false
	}
	port, err = strconv.ParseUint(p, 10, 16)
	if err != nil {
		st.errorAt(path, msg)
		return "", 0, false
	}
	if h != "" {
		if _, err := netip.ParseAddr(h); err != nil && !hostnameRE.MatchString(h) {
			st.errorAt(path, "has an invalid host, got "+st.describe(path, addr))
			return "", 0, false
		}
	}
	return h, port, true
}

// isUnspecifiedHost reports whether host binds every interface.
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsUnspecified()
}

// isLoopbackHost reports whether host is reachable only from this machine:
// localhost, 127.0.0.0/8 or ::1.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

func (st *loadState) checkByteRange(path string, v ByteSize, lo, hi ByteSize) {
	if v < lo || v > hi {
		st.errorAt(path, fmt.Sprintf("must be between %s and %s, got %s", lo, hi, st.describe(path, v.String())))
	}
}

func (st *loadState) checkPositive(path string, d Duration) {
	if d <= 0 {
		st.errorAt(path, "must be greater than 0")
	}
}

// checkOneOf reports an error unless v is one of allowed.
func (st *loadState) checkOneOf(path, v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	if v == "" {
		st.errorAt(path, "required; must be one of "+strings.Join(allowed, ", "))
		return false
	}
	st.errorAt(path, fmt.Sprintf("must be one of %s, got %s", strings.Join(allowed, ", "), st.describe(path, v)))
	return false
}

func (st *loadState) validateLogging(l *Logging) {
	st.checkOneOf("logging.level", l.Level, "debug", "info", "warn", "error")
	st.checkOneOf("logging.format", l.Format, "json", "text")
}

func (st *loadState) validateStorage(s *Storage) {
	if st.checkOneOf("storage.mode", s.Mode, "memory", "redis") && s.Mode == "redis" {
		cacheOK := st.checkRedisURL("storage.redis.cache_url", s.Redis.CacheURL)
		stateOK := st.checkRedisURL("storage.redis.state_url", s.Redis.StateURL)
		if cacheOK && stateOK && s.Redis.CacheURL.Value() == s.Redis.StateURL.Value() {
			st.warnAt("storage.redis.state_url", msgSameRedis)
		}
	}
	if st.checkOneOf("storage.ledger.driver", s.Ledger.Driver, "memory", "postgres") &&
		s.Ledger.Driver == "postgres" && !s.Ledger.DSN.IsSet() {
		st.errorAt("storage.ledger.dsn", "required when storage.ledger.driver is postgres")
	}
	if s.Ledger.BatchSize <= 0 {
		st.errorAt("storage.ledger.batch_size", "must be greater than 0")
	}
	if s.Ledger.FlushInterval <= 0 {
		st.errorAt("storage.ledger.flush_interval", "must be greater than 0")
	}
	if s.Ledger.QueueSize <= 0 {
		st.errorAt("storage.ledger.queue_size", "must be greater than 0")
	}
}

// checkRedisURL validates a Redis URL without ever echoing it, since it may
// embed a password.
func (st *loadState) checkRedisURL(path string, s Secret) bool {
	if !s.IsSet() {
		st.errorAt(path, "required when storage.mode is redis")
		return false
	}
	u, err := url.Parse(s.Value())
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" {
		st.errorAt(path, "must be a redis:// or rediss:// URL with a host")
		return false
	}
	return true
}

// validateProviders checks every provider and returns the defined ids, each
// with the path of its first definition.
func (st *loadState) validateProviders(ps []Provider) map[string]string {
	first := make(map[string]string) // id -> path of its first definition
	for i := range ps {
		p := &ps[i]
		base := indexPath("providers", i)
		st.checkID(base+".id", "provider", p.ID, first)
		st.checkOneOf(base+".type", p.Type, "openai", "anthropic")
		st.checkBaseURL(base+".base_url", p.BaseURL)
		st.checkTags(base+".tags", p.Tags)

		if p.MaxConcurrency < 0 {
			st.errorAt(base+".max_concurrency", "must not be negative; use 0 for unlimited")
		}
		st.checkPositive(base+".timeouts.connect", p.Timeouts.Connect)
		st.checkPositive(base+".timeouts.first_token", p.Timeouts.FirstToken)
		st.checkPositive(base+".timeouts.idle", p.Timeouts.Idle)
		st.checkPositive(base+".timeouts.total", p.Timeouts.Total)

		seenHeaders := make(map[string]bool)
		for j, h := range p.ForwardHeaders {
			hp := indexPath(base+".forward_headers", j)
			lower := strings.ToLower(h)
			switch {
			case !validHeaderName(h):
				st.errorAt(hp, "must be a valid HTTP header name, got "+st.describe(hp, h))
			case forbiddenForwardHeaders[lower]:
				st.errorAt(hp, st.describe(hp, h)+" can never be forwarded upstream")
			case seenHeaders[lower]:
				st.errorAt(hp, "duplicate header "+st.describe(hp, h))
			}
			seenHeaders[lower] = true
		}
	}
	return first
}

// checkID validates the id at path of an entry of the given kind (provider,
// tenant or key) and checks that it is unique among the ids in first, which
// maps each id seen so far to the path of its first definition.
func (st *loadState) checkID(path, kind, id string, first map[string]string) {
	switch {
	case id == "":
		st.errorAt(path, msgRequired)
		return
	case !idRE.MatchString(id):
		st.errorAt(path, fmt.Sprintf("must match %s, got %s", idRE, st.describe(path, id)))
	}
	if prev, dup := first[id]; dup {
		st.errorAt(path, fmt.Sprintf("duplicate %s id %s; first defined at %s", kind, st.describe(path, id), st.where(prev)))
	} else {
		first[id] = path
	}
}

// checkTags validates the tag list at path: each tag must match tagRE and
// appear once.
func (st *loadState) checkTags(path string, tags []string) {
	seen := make(map[string]bool)
	for j, tag := range tags {
		tp := indexPath(path, j)
		switch {
		case !tagRE.MatchString(tag):
			st.errorAt(tp, fmt.Sprintf("must match %s, got %s", tagRE, st.describe(tp, tag)))
		case seen[tag]:
			st.errorAt(tp, "duplicate tag "+st.describe(tp, tag))
		}
		seen[tag] = true
	}
}

// checkBaseURL validates a provider base URL. The URL is never echoed: a bad
// one may embed credentials.
func (st *loadState) checkBaseURL(path, raw string) {
	if raw == "" {
		st.errorAt(path, msgRequired)
		return
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		st.errorAt(path, "must be an absolute http:// or https:// URL with a host")
	case u.User != nil:
		st.errorAt(path, "must not embed credentials; use api_key")
	case u.RawQuery != "" || u.ForceQuery:
		st.errorAt(path, "must not have a query string")
	case strings.Contains(raw, "#"):
		st.errorAt(path, "must not have a fragment")
	}
}

// validHeaderName reports whether s is an RFC 9110 field name (a token).
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validateModels checks every model against the defined providers, given
// as id -> path of the provider's id. It returns the first definition of
// each model id, and the path of that id.
func (st *loadState) validateModels(ms []Model, providers map[string]string) (models map[string]*Model, paths map[string]string) {
	models = make(map[string]*Model)
	known := make([]string, 0, len(providers))
	for id := range providers {
		known = append(known, id)
	}
	slices.Sort(known)

	first := make(map[string]string)
	for i := range ms {
		m := &ms[i]
		base := indexPath("models", i)
		idPath := base + ".id"
		if m.ID == "" {
			st.errorAt(idPath, msgRequired)
		}
		st.checkModelID(idPath, m.ID)
		if m.ID != "" {
			if prev, dup := first[m.ID]; dup {
				st.errorAt(idPath, fmt.Sprintf("duplicate model id %s; first defined at %s", st.describe(idPath, m.ID), st.where(prev)))
			} else {
				first[m.ID] = idPath
				models[m.ID] = m
			}
		}
		st.validateModelCatalog(m, base)

		if len(m.Deployments) == 0 {
			st.errorAt(base+".deployments", "at least one deployment is required")
		}
		seen := make(map[string]bool)
		for j, d := range m.Deployments {
			dp := indexPath(base+".deployments", j)
			pp := dp + ".provider"
			_, defined := providers[d.Provider]
			switch {
			case d.Provider == "":
				st.errorAt(pp, msgRequired)
			case !defined:
				st.errorAt(pp, st.unknownProvider(pp, d.Provider, providers, known))
			case seen[d.Provider]:
				st.errorAt(pp, "provider "+st.describe(pp, d.Provider)+" already serves this model; a provider may appear only once per model")
			}
			seen[d.Provider] = true
			if d.UpstreamModel == "" {
				st.errorAt(dp+".upstream_model", msgRequired)
			}
			st.validateDeploymentCatalog(m, &m.Deployments[j], dp)
		}
	}
	return models, first
}

// validateTenants checks the tenants and their API keys. Key ids and key
// hashes must be unique across all tenants. A hash is never echoed: a bad
// one may be a plaintext key pasted into the wrong field.
func (st *loadState) validateTenants(ts []Tenant) {
	if len(ts) == 0 {
		st.warnAt("tenants", msgNoTenants)
		return
	}
	tenantIDs := make(map[string]string) // id -> path of its first definition
	keyIDs := make(map[string]string)
	hashes := make(map[string]string)
	for i := range ts {
		t := &ts[i]
		base := indexPath("tenants", i)
		st.checkID(base+".id", "tenant", t.ID, tenantIDs)
		if len(t.Keys) == 0 {
			st.warnAt(base+".keys", msgTenantNoKeys)
		}
		for j := range t.Keys {
			k := &t.Keys[j]
			kb := indexPath(base+".keys", j)
			st.checkID(kb+".id", "key", k.ID, keyIDs)
			st.checkKeyHash(kb+".hash", k.Hash, hashes)
			st.checkTags(kb+".tags", k.Tags)
		}
		st.validateTenantBudgets(t, base)
	}
}

// Budget settings (DESIGN §12.3).
const (
	// BudgetScopeTenant caps a tenant's total spend.
	BudgetScopeTenant = "tenant"
	// BudgetScopeKey caps the spend of each of a tenant's keys separately.
	BudgetScopeKey = "key"
	// BudgetWindowDay is a calendar day in UTC.
	BudgetWindowDay = "day"
	// BudgetWindowMonth is a calendar month in UTC.
	BudgetWindowMonth = "month"
	// BudgetBlock rejects requests that would exceed the limit.
	BudgetBlock = "block"
	// BudgetNotify serves requests over the limit, reporting them.
	BudgetNotify = "notify"
	// BudgetFailOpen serves requests while the budget store is unavailable.
	BudgetFailOpen = "open"
	// BudgetFailClosed rejects requests while the budget store is
	// unavailable.
	BudgetFailClosed = "closed"
)

// MaxBudgetLimitUSD is the largest limit_usd a budget may have. It keeps
// every counter, in micro-USD, well inside the range where the budget
// store's Lua arithmetic (IEEE doubles) is exact.
const MaxBudgetLimitUSD = 1_000_000_000

// msgKeyBudgetNoKeys warns about a key budget that can never apply.
const msgKeyBudgetNoKeys = "budget with scope key on a tenant with no keys; it applies to no request"

// validateTenantBudgets checks the budgets of tenant t, whose path is base.
// Each scope and window pair may appear once per tenant.
func (st *loadState) validateTenantBudgets(t *Tenant, base string) {
	first := make(map[[2]string]string) // {scope, window} -> path of its first definition
	for j := range t.Budgets {
		b := &t.Budgets[j]
		bp := indexPath(base+".budgets", j)
		scopeOK := st.checkOneOf(bp+".scope", b.Scope, BudgetScopeTenant, BudgetScopeKey)
		windowOK := st.checkOneOf(bp+".window", b.Window, BudgetWindowDay, BudgetWindowMonth)
		st.checkBudgetLimit(bp+".limit_usd", b.LimitUSD)
		if b.SoftAt != 0 && !(b.SoftAt > 0 && b.SoftAt < 1) {
			st.errorAt(bp+".soft_at", fmt.Sprintf("must be greater than 0 and less than 1, got %s", strconv.FormatFloat(b.SoftAt, 'g', -1, 64)))
		}
		st.checkOneOf(bp+".on_exhausted", b.OnExhausted, BudgetBlock, BudgetNotify)
		if !scopeOK || !windowOK {
			continue
		}
		pair := [2]string{b.Scope, b.Window}
		if prev, dup := first[pair]; dup {
			st.errorAt(bp, fmt.Sprintf("duplicate budget for scope %s and window %s; first defined at %s", b.Scope, b.Window, st.where(prev)))
			continue
		}
		first[pair] = bp
		if b.Scope == BudgetScopeKey && len(t.Keys) == 0 {
			st.warnAt(bp+".scope", msgKeyBudgetNoKeys)
		}
	}
}

// checkBudgetLimit reports an error unless p is set and
// 0 < p <= MaxBudgetLimitUSD.
func (st *loadState) checkBudgetLimit(path string, p Price) {
	switch {
	case !p.IsSet():
		st.errorAt(path, msgRequired)
	case p.Float64() <= 0:
		st.errorAt(path, "must be greater than 0")
	case new(big.Rat).SetFrac64(MaxBudgetLimitUSD, 1).Cmp(priceRat(p)) < 0:
		st.errorAt(path, fmt.Sprintf("must be at most %d", MaxBudgetLimitUSD))
	}
}

// priceRat returns the exact value of a set Price.
func priceRat(p Price) *big.Rat {
	r, ok := new(big.Rat).SetString(p.String())
	if !ok { // unreachable: ParsePrice accepts only plain decimals
		return new(big.Rat)
	}
	return r
}

func (st *loadState) validateBudgets(b *Budgets) {
	st.checkOneOf("budgets.fail_mode", b.FailMode, BudgetFailOpen, BudgetFailClosed)
}

// checkKeyHash validates the API key hash at path and checks that it is
// unique among hashes, which maps each hash seen so far to its path. The
// hash itself never appears in a message.
func (st *loadState) checkKeyHash(path, hash string, hashes map[string]string) {
	switch {
	case hash == "":
		st.errorAt(path, msgRequired)
		return
	case !keyHashRE.MatchString(hash):
		msg := msgBadKeyHash
		if strings.HasPrefix(hash, "sy_") {
			msg += msgPlaintextKey
		}
		st.errorAt(path, msg)
		return
	}
	if prev, dup := hashes[hash]; dup {
		st.errorAt(path, "duplicate key hash; the same key is already defined at "+st.where(prev))
		return
	}
	hashes[hash] = path
}

// unknownProvider formats the error for the deployment at path that names
// the undefined provider id, with a suggestion or the list of known ids.
// providers maps each defined id to the path of its definition, and known
// lists the ids sorted.
func (st *loadState) unknownProvider(path, id string, providers map[string]string, known []string) string {
	msg := "unknown provider " + st.describe(path, id)
	if s := suggest(id, known); s != "" {
		return msg + " (did you mean " + st.describe(providers[s], s) + "?)"
	}
	if len(known) == 0 {
		return msg + " (no providers are defined)"
	}
	described := make([]string, len(known))
	for i, k := range known {
		described[i] = st.describe(providers[k], k)
	}
	return msg + " (known providers: " + strings.Join(described, ", ") + ")"
}

// urlSecretPaths are the secrets that hold URLs or DSNs, which are checked
// for an embedded literal password.
var urlSecretPaths = map[string]bool{
	"storage.redis.cache_url": true,
	"storage.redis.state_url": true,
	"storage.ledger.dsn":      true,
}

// checkLiteralSecrets warns about secrets whose value, or part of it, was
// written literally in a file rather than through a reference. The warning
// never includes the value.
func (st *loadState) checkLiteralSecrets(c *Config) {
	walkSecrets(reflect.ValueOf(c).Elem(), "", func(path string, s Secret) {
		pos, ok := st.pos[path]
		if !s.IsSet() || !ok || pos.node == nil {
			return
		}
		segs := []segment{{text: s.Value()}}
		if e := st.expanded[pos.node]; e != nil {
			segs = e.segs
		}
		if urlSecretPaths[path] && hasLiteralPassword(segs) {
			st.warnAt(path, msgLiteralPassword)
			return
		}
		if looksLikeLiteralCredential(segs) {
			st.warnAt(path, msgLiteralCredential)
		}
	})
}

// looksLikeLiteralCredential applies the credential heuristics to the
// literal parts of a value, including defaults of references that were used:
// a well-known key prefix at the start, or a run of 24 or more
// [A-Za-z0-9_-] characters mixing letters and digits.
func looksLikeLiteralCredential(segs []segment) bool {
	if len(segs) > 0 && segs[0].literal() {
		for _, p := range credentialPrefixes {
			if strings.HasPrefix(segs[0].text, p) {
				return true
			}
		}
	}
	for _, sg := range segs {
		if sg.literal() && hasTokenRun(sg.text) {
			return true
		}
	}
	return false
}

// hasTokenRun reports whether s contains a run of at least 24
// [A-Za-z0-9_-] characters that includes both a letter and a digit.
func hasTokenRun(s string) bool {
	const minRun = 24
	n, letter, digit := 0, false, false
	for i := 0; i <= len(s); i++ {
		var c byte
		if i < len(s) {
			c = s[i]
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			n, letter = n+1, true
		case c >= '0' && c <= '9':
			n, digit = n+1, true
		case c == '_' || c == '-':
			n++
		default:
			if n >= minRun && letter && digit {
				return true
			}
			n, letter, digit = 0, false, false
		}
	}
	return false
}

// hasLiteralPassword reports whether a URL or key/value DSN embeds a password
// any part of which was written literally rather than through a reference,
// counting a used ${VAR:-default} default as literal.
func hasLiteralPassword(segs []segment) bool {
	var b strings.Builder
	var literal []bool
	for _, sg := range segs {
		b.WriteString(sg.text)
		for range len(sg.text) {
			literal = append(literal, sg.literal())
		}
	}
	s := b.String()
	start, end := passwordRange(s)
	for i := start; i < end; i++ {
		if literal[i] {
			return true
		}
	}
	return false
}

// passwordRange returns the byte range of the password in a URL such as
// redis://user:pass@host, or in a key/value DSN such as "password=pass", or
// an empty range if there is none.
func passwordRange(s string) (start, end int) {
	if i := strings.Index(s, "://"); i >= 0 {
		auth := i + 3
		authEnd := len(s)
		if j := strings.IndexAny(s[auth:], "/?#"); j >= 0 {
			authEnd = auth + j
		}
		at := strings.LastIndexByte(s[auth:authEnd], '@')
		if at < 0 {
			return 0, 0
		}
		colon := strings.IndexByte(s[auth:auth+at], ':')
		if colon < 0 {
			return 0, 0
		}
		return auth + colon + 1, auth + at
	}
	lower := strings.ToLower(s)
	for off := 0; ; {
		i := strings.Index(lower[off:], "password=")
		if i < 0 {
			return 0, 0
		}
		i += off
		if i == 0 || lower[i-1] == ' ' || lower[i-1] == '\t' {
			start = i + len("password=")
			end = start
			if end < len(s) && s[end] == '\'' {
				start++
				end++
				for end < len(s) && s[end] != '\'' {
					end++
				}
				return start, end
			}
			for end < len(s) && s[end] != ' ' && s[end] != '\t' {
				end++
			}
			return start, end
		}
		off = i + 1
	}
}

// errorAt records an error for path, unless path or one of its ancestors
// already failed to decode or expand.
func (st *loadState) errorAt(path, msg string) {
	if st.suppressed(path) {
		return
	}
	st.atPath(SeverityError, path, msg)
}

// warnAt records a warning for path.
func (st *loadState) warnAt(path, msg string) {
	if st.suppressed(path) {
		return
	}
	st.atPath(SeverityWarning, path, msg)
}

func (st *loadState) atPath(sev Severity, path, msg string) {
	file, line := st.locate(path)
	st.problems = append(st.problems, Problem{Severity: sev, File: file, Line: line, Path: path, Message: msg})
}

// suppressed reports whether path or an ancestor has already been reported.
func (st *loadState) suppressed(path string) bool {
	for p := path; ; p = parentPath(p) {
		if st.failed[p] {
			return true
		}
		if p == "" {
			return false
		}
	}
}

// locate returns the file and line for a config path: the node it was
// decoded from, or else the nearest ancestor that appeared in a file, or else
// the root file. Values set by an override have no file.
func (st *loadState) locate(path string) (file string, line int) {
	if st.overridden[path] {
		return "", 0
	}
	for p := path; p != ""; p = parentPath(p) {
		if pos, ok := st.pos[p]; ok {
			return pos.file, pos.line
		}
	}
	return st.root, 0
}

// where formats the location of a config path as FILE:LINE.
func (st *loadState) where(path string) string {
	file, line := st.locate(path)
	if line > 0 {
		return file + ":" + strconv.Itoa(line)
	}
	return file
}

// describe quotes a non-secret value for an error message, or names its
// reference if it was expanded from one. A value that may not be quoted
// (see mayQuote) is described as "a string value".
func (st *loadState) describe(path, v string) string {
	if !st.overridden[path] {
		if pos, ok := st.pos[path]; ok && pos.node != nil {
			if e := st.expanded[pos.node]; e != nil {
				return e.describe()
			}
		}
	}
	if !mayQuote(v) {
		return "a string value"
	}
	return strconv.Quote(v)
}

// parentPath strips the last key or index from a config path.
func parentPath(p string) string {
	i := strings.LastIndexAny(p, ".[")
	if i < 0 {
		return ""
	}
	return p[:i]
}
