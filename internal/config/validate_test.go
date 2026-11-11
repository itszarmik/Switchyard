package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// prov returns a valid provider entry with extra fields appended.
func prov(id string, extra ...string) string {
	s := "  - id: " + id + "\n    type: openai\n    base_url: https://" + id + ".example/v1\n"
	for _, e := range extra {
		s += "    " + e + "\n"
	}
	return s
}

// keyHash returns a well-formed key hash made of the hex digit c.
func keyHash(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// tenantsYAML returns a tenants section with one flow-style line per tenant.
func tenantsYAML(tenants ...string) string {
	s := "tenants:\n"
	for _, t := range tenants {
		s += "  - " + t + "\n"
	}
	return s
}

func TestValidationRules(t *testing.T) {
	const v1 = "version: 1\n"
	env := map[string]string{
		"KEY":   "abc123def456ghi789jkl012mno345",
		"PASS":  "hunter2",
		"REDIS": "redis://cache:6379/0",
	}
	tests := []struct {
		name  string
		yaml  string
		wants []want // empty: no problems at all
	}{
		// version
		{"version 1", v1, nil},
		{"version missing", "server: {}\n", []want{errAt("version", "required")}},
		{"version 2", "version: 2\n", []want{errAt("version", "unsupported version 2")}},

		// server addresses
		{"listen defaults are valid", v1 + "server: {}\n", nil},
		{"listen port zero twice", v1 + "server: {listen: ':0', admin_listen: '127.0.0.1:0'}\n", nil},
		{"listen ipv6", v1 + "server: {listen: '[::]:8080', admin_listen: '[::1]:9090'}\n", nil},
		{"listen hostname", v1 + "server: {listen: 'gateway.internal:8080', admin_listen: 'localhost:9090'}\n", nil},
		{"listen max port", v1 + "server: {listen: ':65535'}\n", nil},
		{"listen no port", v1 + "server: {listen: 'localhost'}\n", []want{errAt("server.listen", "must be host:port")}},
		{"listen port too large", v1 + "server: {listen: ':65536'}\n", []want{errAt("server.listen", `got ":65536"`)}},
		{"listen named port", v1 + "server: {listen: ':http'}\n", []want{errAt("server.listen", "must be host:port")}},
		{"listen empty port", v1 + "server: {listen: 'localhost:'}\n", []want{errAt("server.listen", "must be host:port")}},
		{"listen bad host", v1 + "server: {listen: 'bad host:80'}\n", []want{errAt("server.listen", "invalid host")}},
		{"admin_listen invalid", v1 + "server: {admin_listen: '127.0.0.1'}\n", []want{errAt("server.admin_listen", "must be host:port")}},
		{"listen and admin collide", v1 + "server: {listen: '127.0.0.1:8080', admin_listen: '127.0.0.1:8080'}\n",
			[]want{errAt("server.admin_listen", "must not use the same address as server.listen")}},
		{"wildcard collides with loopback", v1 + "server: {listen: ':9090', admin_listen: '127.0.0.1:9090'}\n",
			[]want{errAt("server.admin_listen", "must not use the same address")}},
		{"same port different hosts", v1 + "server: {listen: '10.0.0.1:9090', admin_listen: '127.0.0.1:9090'}\n", nil},

		// admin exposure warning
		{"admin all interfaces", v1 + "server: {admin_listen: ':9090'}\n", []want{warnAt("server.admin_listen", msgAdminExposed)}},
		{"admin 0.0.0.0", v1 + "server: {admin_listen: '0.0.0.0:9090'}\n", []want{warnAt("server.admin_listen", msgAdminExposed)}},
		{"admin [::]", v1 + "server: {admin_listen: '[::]:9090'}\n", []want{warnAt("server.admin_listen", msgAdminExposed)}},
		{"admin public ip", v1 + "server: {admin_listen: '10.1.2.3:9090'}\n", []want{warnAt("server.admin_listen", msgAdminExposed)}},
		{"admin hostname", v1 + "server: {admin_listen: 'admin.internal:9090'}\n", []want{warnAt("server.admin_listen", msgAdminExposed)}},
		{"admin 127.1.2.3", v1 + "server: {admin_listen: '127.1.2.3:9090'}\n", nil},
		{"admin LOCALHOST", v1 + "server: {admin_listen: 'LOCALHOST:9090'}\n", nil},

		// sizes
		{"request bytes min", v1 + "server: {max_request_bytes: 1KiB}\n", nil},
		{"request bytes max", v1 + "server: {max_request_bytes: 1GiB}\n", nil},
		{"request bytes too small", v1 + "server: {max_request_bytes: 1023}\n", []want{errAt("server.max_request_bytes", `must be between 1KiB and 1GiB, got "1023B"`)}},
		{"request bytes too large", v1 + "server: {max_request_bytes: 1073741825}\n", []want{errAt("server.max_request_bytes", "must be between 1KiB and 1GiB")}},
		{"header bytes min", v1 + "server: {max_header_bytes: 4KiB}\n", nil},
		{"header bytes max", v1 + "server: {max_header_bytes: 1MiB}\n", nil},
		{"header bytes too small", v1 + "server: {max_header_bytes: 4095}\n", []want{errAt("server.max_header_bytes", "must be between 4KiB and 1MiB")}},
		{"header bytes too large", v1 + "server: {max_header_bytes: 2MiB}\n", []want{errAt("server.max_header_bytes", "must be between 4KiB and 1MiB")}},

		// durations (zero means default; negatives are rejected when parsed)
		{"timeouts set", v1 + "server: {read_header_timeout: 1s, body_read_timeout: 30s, write_timeout: 2m, idle_timeout: 1m, shutdown_grace: 5s, shutdown_delay: 3s}\n", nil},
		{"zero timeout takes the default", v1 + "server: {idle_timeout: 0s}\n", nil},
		{"negative timeout", v1 + "server: {shutdown_grace: -1s}\n", []want{errAt("server.shutdown_grace", "must not be negative")}},
		{"negative delay", v1 + "server: {shutdown_delay: -1s}\n", []want{errAt("server.shutdown_delay", "must not be negative")}},

		// logging
		{"logging valid", v1 + "logging: {level: debug, format: text}\n", nil},
		{"logging warn and error", v1 + "logging: {level: error}\n", nil},
		{"logging bad level", v1 + "logging: {level: verbose}\n", []want{errAt("logging.level", `must be one of debug, info, warn, error, got "verbose"`)}},
		{"logging level is case-sensitive", v1 + "logging: {level: INFO}\n", []want{errAt("logging.level", "must be one of")}},
		{"logging bad format", v1 + "logging: {format: xml}\n", []want{errAt("logging.format", "must be one of json, text")}},

		// storage
		{"storage memory", v1 + "storage: {mode: memory}\n", nil},
		{"storage bad mode", v1 + "storage: {mode: disk}\n", []want{errAt("storage.mode", "must be one of memory, redis")}},
		{"redis valid", v1 + "storage: {mode: redis, redis: {cache_url: '${REDIS}', state_url: 'rediss://state:6380/0'}}\n", nil},
		{"redis missing urls", v1 + "storage: {mode: redis}\n", []want{
			errAt("storage.redis.cache_url", "required when storage.mode is redis"),
			errAt("storage.redis.state_url", "required when storage.mode is redis"),
		}},
		{"redis wrong scheme", v1 + "storage: {mode: redis, redis: {cache_url: 'http://c:6379', state_url: 'redis://s'}}\n",
			[]want{errAt("storage.redis.cache_url", "must be a redis:// or rediss:// URL with a host")}},
		{"redis no host", v1 + "storage: {mode: redis, redis: {cache_url: 'redis://:6379', state_url: 'redis://s'}}\n",
			[]want{errAt("storage.redis.cache_url", "with a host")}},
		{"redis same instance", v1 + "storage: {mode: redis, redis: {cache_url: '${REDIS}', state_url: '${REDIS}'}}\n",
			[]want{warnAt("storage.redis.state_url", msgSameRedis)}},
		{"ledger postgres", v1 + "storage: {ledger: {driver: postgres, dsn: 'postgres://app:${PASS}@db/x'}}\n", nil},
		{"ledger postgres without dsn", v1 + "storage: {ledger: {driver: postgres}}\n", []want{errAt("storage.ledger.dsn", "required when storage.ledger.driver is postgres")}},
		{"ledger bad driver", v1 + "storage: {ledger: {driver: sqlite}}\n", []want{errAt("storage.ledger.driver", "must be one of memory, postgres")}},

		// providers
		{"provider valid", v1 + "providers:\n" + prov("a-b_9", "api_key: ${KEY}", "tags: [zdr, region_eu]", "max_concurrency: 8", "forward_headers: [anthropic-beta, X-Trace]"), nil},
		{"provider id missing", v1 + "providers:\n  - type: openai\n    base_url: https://x.example\n", []want{errAt("providers[0].id", "required")}},
		{"provider id uppercase", v1 + "providers:\n" + prov("OpenAI"), []want{errAt("providers[0].id", "must match")}},
		{"provider id too long", v1 + "providers:\n" + prov(strings.Repeat("a", 64)), []want{errAt("providers[0].id", "must match")}},
		{"provider id max length", v1 + "providers:\n" + prov(strings.Repeat("a", 63)), nil},
		{"provider id leading dash", v1 + "providers:\n" + prov("-a"), []want{errAt("providers[0].id", "must match")}},
		{"provider id duplicate", v1 + "providers:\n" + prov("a") + prov("a"), []want{errAt("providers[1].id", `duplicate provider id "a"; first defined at cfg.yaml:3`)}},
		{"provider bad type", v1 + "providers:\n  - id: a\n    type: gemini\n    base_url: https://a.example\n", []want{errAt("providers[0].type", "must be one of openai, anthropic")}},
		{"provider type missing", v1 + "providers:\n  - id: a\n    base_url: https://a.example\n", []want{errAt("providers[0].type", "required; must be one of openai, anthropic")}},
		{"misspelled field is not also required", v1 + "providers:\n  - id: a\n    type: openai\n    base_ur: https://a.example\n", []want{errAt("providers[0].base_ur", `did you mean "base_url"?`)}},
		{"provider anthropic", v1 + "providers:\n  - id: a\n    type: anthropic\n    base_url: https://a.example\n", nil},
		{"base_url missing", v1 + "providers:\n  - id: a\n    type: openai\n", []want{errAt("providers[0].base_url", "required")}},
		{"base_url relative", v1 + "providers:\n  - id: a\n    type: openai\n    base_url: /v1\n", []want{errAt("providers[0].base_url", "absolute")}},
		{"base_url ftp", v1 + "providers:\n  - id: a\n    type: openai\n    base_url: ftp://a.example\n", []want{errAt("providers[0].base_url", "http:// or https://")}},
		{"base_url no host", v1 + "providers:\n  - id: a\n    type: openai\n    base_url: 'https:///v1'\n", []want{errAt("providers[0].base_url", "with a host")}},
		{"base_url userinfo", v1 + "providers:\n  - id: a\n    type: openai\n    base_url: https://user@a.example\n", []want{errAt("providers[0].base_url", "must not embed credentials")}},
		{"base_url query", v1 + "providers:\n  - id: a\n    type: openai\n    base_url: https://a.example/v1?x=1\n", []want{errAt("providers[0].base_url", "query")}},
		{"base_url empty query", v1 + "providers:\n  - id: a\n    type: openai\n    base_url: https://a.example/v1?\n", []want{errAt("providers[0].base_url", "query")}},
		{"base_url fragment", v1 + "providers:\n  - id: a\n    type: openai\n    base_url: 'https://a.example/v1#x'\n", []want{errAt("providers[0].base_url", "fragment")}},
		{"tag invalid", v1 + "providers:\n" + prov("a", "tags: [Region-EU]"), []want{errAt("providers[0].tags[0]", "must match")}},
		{"tag duplicate", v1 + "providers:\n" + prov("a", "tags: [zdr, zdr]"), []want{errAt("providers[0].tags[1]", `duplicate tag "zdr"`)}},
		{"max_concurrency negative", v1 + "providers:\n" + prov("a", "max_concurrency: -1"), []want{errAt("providers[0].max_concurrency", "must not be negative")}},
		{"provider timeouts", v1 + "providers:\n" + prov("a", "timeouts: {connect: 1s, first_token: 2s, idle: 3s, total: 4m}"), nil},
		{"provider total timeout negative", v1 + "providers:\n" + prov("a", "timeouts: {total: -1m}"), []want{errAt("providers[0].timeouts.total", "must not be negative")}},
		{"provider timeout negative", v1 + "providers:\n" + prov("a", "timeouts: {idle: -3s}"), []want{errAt("providers[0].timeouts.idle", "must not be negative")}},
		{"forward header invalid", v1 + "providers:\n" + prov("a", "forward_headers: ['x header']"), []want{errAt("providers[0].forward_headers[0]", "valid HTTP header name")}},
		{"forward header empty", v1 + "providers:\n" + prov("a", "forward_headers: ['']"), []want{errAt("providers[0].forward_headers[0]", "valid HTTP header name")}},
		{"forward header duplicate", v1 + "providers:\n" + prov("a", "forward_headers: [X-A, x-a]"), []want{errAt("providers[0].forward_headers[1]", "duplicate header")}},
		{"forward authorization", v1 + "providers:\n" + prov("a", "forward_headers: [Authorization]"), []want{errAt("providers[0].forward_headers[0]", "can never be forwarded")}},
		{"forward proxy-authorization", v1 + "providers:\n" + prov("a", "forward_headers: [PROXY-AUTHORIZATION]"), []want{errAt("providers[0].forward_headers[0]", "can never be forwarded")}},
		{"forward cookie", v1 + "providers:\n" + prov("a", "forward_headers: [cookie]"), []want{errAt("providers[0].forward_headers[0]", "can never be forwarded")}},

		// models
		{"model valid", v1 + "providers:\n" + prov("a") + prov("b") + "models:\n  - id: org/Model-1.5:latest\n    deployments:\n      - {provider: a, upstream_model: m}\n      - {provider: b, upstream_model: m}\n", nil},
		{"model id missing", v1 + "providers:\n" + prov("a") + "models:\n  - deployments: [{provider: a, upstream_model: m}]\n", []want{errAt("models[0].id", "required")}},
		{"model id with @", v1 + "providers:\n" + prov("a") + "models:\n  - id: m@a\n    deployments: [{provider: a, upstream_model: m}]\n", []want{errAt("models[0].id", `must not contain "@"`)}},
		{"model id auto", v1 + "providers:\n" + prov("a") + "models:\n  - id: auto\n    deployments: [{provider: a, upstream_model: m}]\n", []want{errAt("models[0].id", "reserved")}},
		{"model id auto prefix", v1 + "providers:\n" + prov("a") + "models:\n  - id: 'auto:fast'\n    deployments: [{provider: a, upstream_model: m}]\n", []want{errAt("models[0].id", "reserved")}},
		{"model id autopilot is fine", v1 + "providers:\n" + prov("a") + "models:\n  - id: autopilot\n    deployments: [{provider: a, upstream_model: m}]\n", nil},
		{"model id bad chars", v1 + "providers:\n" + prov("a") + "models:\n  - id: 'my model'\n    deployments: [{provider: a, upstream_model: m}]\n", []want{errAt("models[0].id", "must match")}},
		{"model id too long", v1 + "providers:\n" + prov("a") + "models:\n  - id: " + strings.Repeat("m", 129) + "\n    deployments: [{provider: a, upstream_model: m}]\n", []want{errAt("models[0].id", "must match")}},
		{"model id duplicate", v1 + "providers:\n" + prov("a") + "models:\n  - id: m\n    deployments: [{provider: a, upstream_model: m}]\n  - id: m\n    deployments: [{provider: a, upstream_model: m}]\n",
			[]want{errAt("models[1].id", `duplicate model id "m"; first defined at cfg.yaml:7`)}},
		{"model without deployments", v1 + "models:\n  - id: m\n", []want{errAt("models[0].deployments", "at least one deployment")}},
		{"deployment unknown provider with suggestion", v1 + "providers:\n" + prov("openai") + "models:\n  - id: m\n    deployments: [{provider: openia, upstream_model: m}]\n",
			[]want{errAt("models[0].deployments[0].provider", `unknown provider "openia" (did you mean "openai"?)`)}},
		{"deployment unknown provider lists ids", v1 + "providers:\n" + prov("b") + prov("a") + "models:\n  - id: m\n    deployments: [{provider: zzzzzz, upstream_model: m}]\n",
			[]want{errAt("models[0].deployments[0].provider", `unknown provider "zzzzzz" (known providers: "a", "b")`)}},
		{"deployment unknown provider none defined", v1 + "models:\n  - id: m\n    deployments: [{provider: x, upstream_model: m}]\n",
			[]want{errAt("models[0].deployments[0].provider", "no providers are defined")}},
		{"deployment provider missing", v1 + "models:\n  - id: m\n    deployments: [{upstream_model: m}]\n", []want{errAt("models[0].deployments[0].provider", "required")}},
		{"deployment upstream missing", v1 + "providers:\n" + prov("a") + "models:\n  - id: m\n    deployments: [{provider: a}]\n", []want{errAt("models[0].deployments[0].upstream_model", "required")}},
		{"deployment provider twice", v1 + "providers:\n" + prov("a") + "models:\n  - id: m\n    deployments: [{provider: a, upstream_model: m}, {provider: a, upstream_model: n}]\n",
			[]want{errAt("models[0].deployments[1].provider", "only once per model")}},

		// tenants and keys
		{"no tenants", v1, []want{warnAt("tenants", msgNoTenants)}},
		{"empty tenants", v1 + "tenants: []\n", []want{{sev: SeverityWarning, path: "tenants", msg: msgNoTenants, line: 2}}},
		{"tenants valid", v1 + tenantsYAML(
			"{id: acme, keys: [{id: acme-backend, hash: "+keyHash("a")+", tags: [prod, eu]}, {id: acme-ci, hash: "+keyHash("b")+"}]}",
			"{id: beta_2, keys: [{id: beta, hash: "+keyHash("c")+"}]}",
		), nil},
		{"tenant id missing", v1 + tenantsYAML("{keys: [{id: k, hash: "+keyHash("a")+"}]}"), []want{errAt("tenants[0].id", "required")}},
		{"tenant id uppercase", v1 + tenantsYAML("{id: Acme, keys: [{id: k, hash: "+keyHash("a")+"}]}"), []want{errAt("tenants[0].id", `must match ^[a-z0-9][a-z0-9_-]{0,62}$, got "Acme"`)}},
		{"tenant id too long", v1 + tenantsYAML("{id: "+strings.Repeat("t", 64)+", keys: [{id: k, hash: "+keyHash("a")+"}]}"), []want{errAt("tenants[0].id", "must match")}},
		{"tenant id max length", v1 + tenantsYAML("{id: "+strings.Repeat("t", 63)+", keys: [{id: k, hash: "+keyHash("a")+"}]}"), nil},
		{"tenant id duplicate", v1 + tenantsYAML(
			"{id: a, keys: [{id: k1, hash: "+keyHash("a")+"}]}",
			"{id: a, keys: [{id: k2, hash: "+keyHash("b")+"}]}",
		), []want{errAt("tenants[1].id", `duplicate tenant id "a"; first defined at cfg.yaml:3`)}},
		{"tenant without keys", v1 + tenantsYAML("{id: a}"), []want{warnAt("tenants[0].keys", msgTenantNoKeys)}},
		{"tenant with empty keys", v1 + tenantsYAML("{id: a, keys: []}"), []want{warnAt("tenants[0].keys", msgTenantNoKeys)}},
		{"tenant id may equal a key id", v1 + tenantsYAML("{id: a, keys: [{id: a, hash: "+keyHash("a")+"}]}"), nil},
		{"key id missing", v1 + tenantsYAML("{id: a, keys: [{hash: "+keyHash("a")+"}]}"), []want{errAt("tenants[0].keys[0].id", "required")}},
		{"key id invalid", v1 + tenantsYAML("{id: a, keys: [{id: 'my key', hash: "+keyHash("a")+"}]}"), []want{errAt("tenants[0].keys[0].id", `must match ^[a-z0-9][a-z0-9_-]{0,62}$, got "my key"`)}},
		{"key id duplicate in a tenant", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: "+keyHash("a")+"}, {id: k, hash: "+keyHash("b")+"}]}"),
			[]want{errAt("tenants[0].keys[1].id", `duplicate key id "k"; first defined at cfg.yaml:3`)}},
		{"key id duplicate across tenants", v1 + tenantsYAML(
			"{id: a, keys: [{id: k, hash: "+keyHash("a")+"}]}",
			"{id: b, keys: [{id: k, hash: "+keyHash("b")+"}]}",
		), []want{errAt("tenants[1].keys[0].id", `duplicate key id "k"; first defined at cfg.yaml:3`)}},
		{"key hash missing", v1 + tenantsYAML("{id: a, keys: [{id: k}]}"), []want{errAt("tenants[0].keys[0].hash", "required")}},
		{"key hash uppercase", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: "+keyHash("A")+"}]}"), []want{errAt("tenants[0].keys[0].hash", msgBadKeyHash)}},
		{"key hash too short", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: 'sha256:"+strings.Repeat("a", 63)+"'}]}"), []want{errAt("tenants[0].keys[0].hash", msgBadKeyHash)}},
		{"key hash too long", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: 'sha256:"+strings.Repeat("a", 65)+"'}]}"), []want{errAt("tenants[0].keys[0].hash", msgBadKeyHash)}},
		{"key hash without prefix", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: '"+strings.Repeat("a", 64)+"'}]}"), []want{errAt("tenants[0].keys[0].hash", msgBadKeyHash)}},
		{"key hash other algorithm", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: 'sha512:"+strings.Repeat("a", 64)+"'}]}"), []want{errAt("tenants[0].keys[0].hash", msgBadKeyHash)}},
		{"key hash is a plaintext key", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: sy_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG}]}"),
			[]want{errAt("tenants[0].keys[0].hash", msgBadKeyHash+msgPlaintextKey)}},
		{"key hash duplicate across tenants", v1 + tenantsYAML(
			"{id: a, keys: [{id: k1, hash: "+keyHash("a")+"}]}",
			"{id: b, keys: [{id: k2, hash: "+keyHash("a")+"}]}",
		), []want{errAt("tenants[1].keys[0].hash", "duplicate key hash; the same key is already defined at cfg.yaml:3")}},
		{"key hash duplicate in a tenant", v1 + tenantsYAML("{id: a, keys: [{id: k1, hash: "+keyHash("a")+"}, {id: k2, hash: "+keyHash("a")+"}]}"),
			[]want{errAt("tenants[0].keys[1].hash", "duplicate key hash")}},
		{"key tag invalid", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: "+keyHash("a")+", tags: [Prod]}]}"), []want{errAt("tenants[0].keys[0].tags[0]", "must match")}},
		{"key tag duplicate", v1 + tenantsYAML("{id: a, keys: [{id: k, hash: "+keyHash("a")+", tags: [eu, eu]}]}"), []want{errAt("tenants[0].keys[0].tags[1]", `duplicate tag "eu"`)}},
		{"several key problems", v1 + tenantsYAML("{id: a, keys: [{id: K, hash: nope, tags: [x y]}]}"), []want{
			errAt("tenants[0].keys[0].id", "must match"),
			errAt("tenants[0].keys[0].hash", msgBadKeyHash),
			errAt("tenants[0].keys[0].tags[0]", "must match"),
		}},

		// literal credentials
		{"api key from env", v1 + "providers:\n" + prov("a", "api_key: ${KEY}"), nil},
		{"api key from file default", v1 + "providers:\n" + prov("a", "api_key: ${UNSET:-}"), nil},
		{"api key short literal", v1 + "providers:\n" + prov("a", "api_key: local"), nil},
		{"api key sk-", v1 + "providers:\n" + prov("a", "api_key: sk-abc"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key sk-ant-", v1 + "providers:\n" + prov("a", "api_key: sk-ant-x"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key sy_", v1 + "providers:\n" + prov("a", "api_key: sy_x"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key AIza", v1 + "providers:\n" + prov("a", "api_key: AIzaX"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key gsk_", v1 + "providers:\n" + prov("a", "api_key: gsk_x"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key xai-", v1 + "providers:\n" + prov("a", "api_key: xai-x"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key long token", v1 + "providers:\n" + prov("a", "api_key: Bearer abcdefghij0123456789ABCD"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key long letters only", v1 + "providers:\n" + prov("a", "api_key: abcdefghijabcdefghijabcdefgh"), nil},
		{"api key 23 char token", v1 + "providers:\n" + prov("a", "api_key: abcdefghij0123456789abc"), nil},
		{"api key prefix before reference", v1 + "providers:\n" + prov("a", "api_key: sk-${KEY}"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"redis literal password", v1 + "storage: {redis: {cache_url: 'redis://:hunter2@c:6379/0'}}\n", []want{warnAt("storage.redis.cache_url", msgLiteralPassword)}},
		{"redis referenced password", v1 + "storage: {redis: {cache_url: 'redis://:${PASS}@c:6379/0'}}\n", nil},
		{"redis user without password", v1 + "storage: {redis: {state_url: 'redis://user@c:6379/0'}}\n", nil},
		{"dsn literal password", v1 + "storage: {ledger: {dsn: 'postgres://app:hunter2@db:5432/x'}}\n", []want{warnAt("storage.ledger.dsn", msgLiteralPassword)}},
		{"dsn keyword literal password", v1 + "storage: {ledger: {dsn: 'host=db user=app password=hunter2 dbname=x'}}\n", []want{warnAt("storage.ledger.dsn", msgLiteralPassword)}},
		{"dsn keyword quoted password", v1 + "storage: {ledger: {dsn: \"host=db password='a b'\"}}\n", []want{warnAt("storage.ledger.dsn", msgLiteralPassword)}},
		{"dsn keyword referenced password", v1 + "storage: {ledger: {dsn: 'host=db password=${PASS} dbname=x'}}\n", nil},
		{"dsn from file", v1 + "storage: {ledger: {dsn: '${file:dsn.txt}'}}\n", nil},
		{"api key literal in a default", v1 + "providers:\n" + prov("a", "api_key: ${UNSET_KEY:-sk-proj-abcdefghij0123456789}"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key token run in a default", v1 + "providers:\n" + prov("a", "api_key: ${UNSET_KEY:-abcdefghij0123456789ABCD}"), []want{warnAt("providers[0].api_key", msgLiteralCredential)}},
		{"api key default not taken", v1 + "providers:\n" + prov("a", "api_key: ${KEY:-sk-proj-abcdefghij0123456789}"), nil},
		{"redis literal password in a default", v1 + "storage: {redis: {cache_url: '${UNSET_URL:-redis://:hunter2@c:6379/0}'}}\n", []want{warnAt("storage.redis.cache_url", msgLiteralPassword)}},
		{"redis default without a password", v1 + "storage: {redis: {cache_url: '${UNSET_URL:-redis://c:6379/0}'}}\n", nil},
		{"dsn password from a default inside a URL", v1 + "storage: {ledger: {dsn: 'postgres://app:${UNSET_PW:-hunter2}@db/x'}}\n", []want{warnAt("storage.ledger.dsn", msgLiteralPassword)}},
	}
	files := fakeFS{"dsn.txt": "postgres://app:literal-in-file@db/x\n"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, env, files), tt.wants...)
		})
	}
}

// TestValidateUnreachableRules covers checks that a file cannot trigger
// because zero values take defaults and negative durations are rejected when
// parsed, so they are exercised on a Config directly.
func TestValidateUnreachableRules(t *testing.T) {
	cfg := &Config{Version: 1, Providers: []Provider{{ID: "a", Type: "openai", BaseURL: "https://a.example"}}}
	applyDefaults(cfg, nil)
	cfg.Server.ReadHeaderTimeout = 0
	cfg.Server.BodyReadTimeout = 0
	cfg.Server.WriteTimeout = 0
	cfg.Server.IdleTimeout = 0
	cfg.Server.ShutdownGrace = 0
	cfg.Server.ShutdownDelay = -1
	cfg.Providers[0].Timeouts.Connect = 0
	cfg.Providers[0].Timeouts.FirstToken = 0
	cfg.Providers[0].Timeouts.Idle = 0
	cfg.Providers[0].Timeouts.Total = 0

	st := newLoadState(rootFile, LoadOptions{})
	st.validate(cfg)
	checkProblems(t, st.problems,
		errAt("server.read_header_timeout", "greater than 0"),
		errAt("server.body_read_timeout", "greater than 0"),
		errAt("server.write_timeout", "greater than 0"),
		errAt("server.idle_timeout", "greater than 0"),
		errAt("server.shutdown_grace", "greater than 0"),
		errAt("server.shutdown_delay", "must not be negative"),
		errAt("providers[0].timeouts.connect", "greater than 0"),
		errAt("providers[0].timeouts.first_token", "greater than 0"),
		errAt("providers[0].timeouts.idle", "greater than 0"),
		errAt("providers[0].timeouts.total", "greater than 0"),
		warnAt("tenants", msgNoTenants),
	)
}

func TestProblemLocationFallsBackToParent(t *testing.T) {
	// A missing field is reported at the line of its enclosing mapping.
	ve := mustFail(t, "version: 1\nproviders:\n  - type: openai\n    base_url: https://x.example\n", nil, nil)
	checkProblems(t, ve.Problems, want{sev: SeverityError, file: rootFile, line: 3, path: "providers[0].id", msg: "required"})
}

func TestTimeoutDefaults(t *testing.T) {
	cfg := mustLoad(t, "version: 1\nproviders:\n"+prov("a")+prov("b", "timeouts: {total: 90s}"), nil, nil).Config()
	want := []ProviderTimeouts{
		{Connect: Duration(5 * time.Second), FirstToken: Duration(30 * time.Second), Idle: Duration(60 * time.Second), Total: Duration(10 * time.Minute)},
		{Connect: Duration(5 * time.Second), FirstToken: Duration(30 * time.Second), Idle: Duration(60 * time.Second), Total: Duration(90 * time.Second)},
	}
	for i, w := range want {
		if got := cfg.Providers[i].Timeouts; got != w {
			t.Errorf("providers[%d].timeouts = %+v, want %+v", i, got, w)
		}
	}
}

// TestNoTenantsWarning checks the warning directly, since checkProblems
// ignores it unless asked.
func TestNoTenantsWarning(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantWarn bool
		wantLine int
	}{
		{"tenants absent", "version: 1\n", true, 0},
		{"tenants null", "version: 1\ntenants:\n", true, 2},
		{"tenants empty", "version: 1\ntenants: []\n", true, 2},
		{"one tenant", "version: 1\n" + tenantsYAML("{id: a, keys: [{id: k, hash: "+keyHash("a")+"}]}"), false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []Problem
			for _, p := range mustLoad(t, tt.yaml, nil, nil).Warnings() {
				if p.Path == "tenants" {
					got = append(got, p)
				}
			}
			if !tt.wantWarn {
				if len(got) != 0 {
					t.Errorf("unexpected warnings: %v", got)
				}
				return
			}
			want := []Problem{{Severity: SeverityWarning, File: rootFile, Line: tt.wantLine, Path: "tenants", Message: msgNoTenants}}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
}

// A key hash is never echoed, whatever is wrong with it: it may be a
// plaintext key pasted into the wrong field.
func TestKeyHashesNeverEchoed(t *testing.T) {
	bad := []string{
		"sy_Zq8pL2xW9vN4mK7jR3tYabcdefghijklmnopqrstu",
		"sha256:" + strings.Repeat("A", 64),
		"sha256:" + strings.Repeat("ab", 31) + "a",
		"hunter2hunter2",
	}
	var keys []string
	for i, h := range bad {
		keys = append(keys, fmt.Sprintf("{id: k%d, hash: '%s'}", i, h))
	}
	dup := keyHash("d")
	keys = append(keys, "{id: d1, hash: "+dup+"}", "{id: d2, hash: "+dup+"}")
	root := "version: 1\n" + tenantsYAML("{id: a, keys: ["+strings.Join(keys, ", ")+"]}")
	ve := mustFail(t, root, nil, nil)
	if got := len(ve.Errors()); got != len(bad)+1 {
		t.Errorf("%d errors, want %d:\n%s", got, len(bad)+1, ve)
	}
	for _, h := range append(bad, dup, strings.TrimPrefix(dup, "sha256:")) {
		if strings.Contains(ve.Error(), h) {
			t.Errorf("the error echoes %q:\n%s", h, ve)
		}
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"a": true, "default": true, "key-4be1c0": true, "a_b-9": true, strings.Repeat("a", 63): true,
		"": false, "A": false, "-a": false, "_a": false, "a b": false, "a.b": false, strings.Repeat("a", 64): false,
	} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestBudgetValidation(t *testing.T) {
	const v1 = "version: 1\n"
	key := "keys: [{id: k, hash: " + keyHash("a") + "}]"
	// tenant returns a tenant with one key and the given budgets.
	tenant := func(budgets ...string) string {
		return v1 + tenantsYAML("{id: acme, "+key+", budgets: ["+strings.Join(budgets, ", ")+"]}")
	}
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"no budgets", tenant(), nil},
		{"every field", tenant(
			"{scope: tenant, window: month, limit_usd: '2000', soft_at: 0.8, on_exhausted: block}",
			"{scope: tenant, window: day, limit_usd: 100.5, on_exhausted: notify}",
			"{scope: key, window: day, limit_usd: 0.000000001}",
			"{scope: key, window: month, limit_usd: 1000000000}",
		), nil},
		{"scope missing", tenant("{window: day, limit_usd: 1}"),
			[]want{errAt("tenants[0].budgets[0].scope", "required; must be one of tenant, key")}},
		{"scope unknown", tenant("{scope: team, window: day, limit_usd: 1}"),
			[]want{errAt("tenants[0].budgets[0].scope", `must be one of tenant, key, got "team"`)}},
		{"window missing", tenant("{scope: tenant, limit_usd: 1}"),
			[]want{errAt("tenants[0].budgets[0].window", "required; must be one of day, month")}},
		{"window unknown", tenant("{scope: tenant, window: week, limit_usd: 1}"),
			[]want{errAt("tenants[0].budgets[0].window", `must be one of day, month, got "week"`)}},
		{"limit missing", tenant("{scope: tenant, window: day}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", "required")}},
		{"limit null", tenant("{scope: tenant, window: day, limit_usd: ~}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", "required")}},
		{"limit zero", tenant("{scope: tenant, window: day, limit_usd: 0}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", "must be greater than 0")}},
		{"limit zero decimal", tenant("{scope: tenant, window: day, limit_usd: '0.000'}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", "must be greater than 0")}},
		{"limit negative", tenant("{scope: tenant, window: day, limit_usd: -5}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", "must not be negative")}},
		{"limit not a number", tenant("{scope: tenant, window: day, limit_usd: lots}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", `expected an amount in USD, such as 2000 or "12.50", got "lots"`)}},
		{"limit with exponent", tenant("{scope: tenant, window: day, limit_usd: 1e3}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", "an amount in USD must be written as a plain decimal")}},
		{"limit too large", tenant("{scope: tenant, window: day, limit_usd: '1000000000.000000001'}"),
			[]want{errAt("tenants[0].budgets[0].limit_usd", "must be at most 1000000000")}},
		{"soft_at half", tenant("{scope: tenant, window: day, limit_usd: 1, soft_at: 0.5}"), nil},
		{"soft_at one", tenant("{scope: tenant, window: day, limit_usd: 1, soft_at: 1}"),
			[]want{errAt("tenants[0].budgets[0].soft_at", "must be greater than 0 and less than 1, got 1")}},
		{"soft_at negative", tenant("{scope: tenant, window: day, limit_usd: 1, soft_at: -0.1}"),
			[]want{errAt("tenants[0].budgets[0].soft_at", "got -0.1")}},
		{"soft_at nan", tenant("{scope: tenant, window: day, limit_usd: 1, soft_at: .nan}"),
			[]want{errAt("tenants[0].budgets[0].soft_at", "expected a finite number")}},
		{"on_exhausted unknown", tenant("{scope: tenant, window: day, limit_usd: 1, on_exhausted: warn}"),
			[]want{errAt("tenants[0].budgets[0].on_exhausted", `must be one of block, notify, got "warn"`)}},
		{"duplicate pair", tenant(
			"{scope: tenant, window: day, limit_usd: 1}",
			"{scope: key, window: day, limit_usd: 1}",
			"{scope: tenant, window: day, limit_usd: 2, on_exhausted: notify}",
		), []want{errAt("tenants[0].budgets[2]", "duplicate budget for scope tenant and window day; first defined at cfg.yaml:3")}},
		{"same pair in two tenants", v1 + tenantsYAML(
			"{id: a, keys: [{id: k1, hash: "+keyHash("a")+"}], budgets: [{scope: tenant, window: day, limit_usd: 1}]}",
			"{id: b, keys: [{id: k2, hash: "+keyHash("b")+"}], budgets: [{scope: tenant, window: day, limit_usd: 1}]}",
		), nil},
		{"key budget without keys", v1 + tenantsYAML("{id: a, budgets: [{scope: key, window: day, limit_usd: 1}, {scope: tenant, window: day, limit_usd: 1}]}"),
			[]want{warnAt("tenants[0].keys", msgTenantNoKeys), warnAt("tenants[0].budgets[0].scope", msgKeyBudgetNoKeys)}},
		{"fail_mode open", v1 + "budgets: {fail_mode: open}\n", nil},
		{"fail_mode closed", v1 + "budgets: {fail_mode: closed}\n", nil},
		{"fail_mode unknown", v1 + "budgets: {fail_mode: strict}\n",
			[]want{errAt("budgets.fail_mode", `must be one of open, closed, got "strict"`)}},
		{"unknown budget field", tenant("{scope: tenant, window: day, limit_usd: 1, soft_profile: cheap}"),
			[]want{errAt("tenants[0].budgets[0].soft_profile", "unknown field")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, nil, nil), tt.wants...)
		})
	}
}

func TestBudgetDefaults(t *testing.T) {
	root := "version: 1\n" + tenantsYAML("{id: acme, keys: [{id: k, hash: "+keyHash("a")+"}], budgets: ["+
		"{scope: tenant, window: month, limit_usd: '2000.50', soft_at: 0.8}, "+
		"{scope: key, window: day, limit_usd: 5, on_exhausted: notify}]}")
	cfg := mustLoad(t, root, nil, nil).Config()
	want := []Budget{
		{Scope: BudgetScopeTenant, Window: BudgetWindowMonth, LimitUSD: mustPrice(t, "2000.50"), SoftAt: 0.8, OnExhausted: BudgetBlock},
		{Scope: BudgetScopeKey, Window: BudgetWindowDay, LimitUSD: mustPrice(t, "5"), OnExhausted: BudgetNotify},
	}
	if diff := cmp.Diff(want, cfg.Tenants[0].Budgets); diff != "" {
		t.Errorf("budgets (-want +got):\n%s", diff)
	}
	if got := cfg.Budgets.FailMode; got != BudgetFailOpen {
		t.Errorf("budgets.fail_mode = %q, want %q", got, BudgetFailOpen)
	}
	cfg = mustLoad(t, "version: 1\nbudgets: {fail_mode: closed}\n", nil, nil).Config()
	if got := cfg.Budgets.FailMode; got != BudgetFailClosed {
		t.Errorf("explicit budgets.fail_mode = %q, want %q", got, BudgetFailClosed)
	}
}

func TestLedgerValidation(t *testing.T) {
	const v1 = "version: 1\n"
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"defaults", v1, nil},
		{"every field", v1 + "storage: {ledger: {driver: memory, batch_size: 10, flush_interval: 250ms, queue_size: 20}}\n", nil},
		{"batch_size negative", v1 + "storage: {ledger: {batch_size: -1}}\n",
			[]want{errAt("storage.ledger.batch_size", "must be greater than 0")}},
		{"flush_interval negative", v1 + "storage: {ledger: {flush_interval: -1s}}\n",
			[]want{errAt("storage.ledger.flush_interval", "duration must not be negative")}},
		{"queue_size negative", v1 + "storage: {ledger: {queue_size: -5}}\n",
			[]want{errAt("storage.ledger.queue_size", "must be greater than 0")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, nil, nil), tt.wants...)
		})
	}
}

func TestLedgerDefaults(t *testing.T) {
	cfg := mustLoad(t, "version: 1\n", nil, nil).Config()
	l := cfg.Storage.Ledger
	if l.Driver != "memory" || l.BatchSize != 500 || l.FlushInterval != Duration(time.Second) || l.QueueSize != 100_000 {
		t.Errorf("ledger = driver %q, batch_size %d, flush_interval %s, queue_size %d; want memory, 500, 1s, 100000",
			l.Driver, l.BatchSize, l.FlushInterval, l.QueueSize)
	}
}
