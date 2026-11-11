package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"switchyard/internal/config"
	"switchyard/internal/version"
)

// reexecEnv makes the test binary act as the switchyard CLI, so the
// end-to-end test can run the real main() in a subprocess.
const reexecEnv = "SWITCHYARD_TEST_REEXEC_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(reexecEnv) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// env is a fake environment for run's lookupEnv.
type env map[string]string

func (e env) lookup(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}

// runCLI calls run in-process and returns its exit code and output.
func runCLI(t *testing.T, e env, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut, e.lookup)
	return code, out.String(), errOut.String()
}

// The P0 acceptance case: an unknown key, a deployment that references an
// unknown provider, and a missing environment variable.
func TestConfigValidateReportsAllThreeMistakes(t *testing.T) {
	const file = "testdata/three-mistakes.yaml"
	code, stdout, stderr := runCLI(t, env{}, "config", "validate", "-c", file)
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	want := []string{
		file + `:7: error: server.admin_lisen: unknown field (did you mean "admin_listen"?)`,
		file + `:13: error: providers[0].api_key: environment variable OPENAI_API_KEY is not set`,
		file + `:18: error: models[0].deployments[0].provider: unknown provider "openia" (did you mean "openai"?)`,
		"3 errors, 0 warnings",
	}
	if got := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n"); !equalLines(got, want) {
		t.Errorf("stderr =\n%s\nwant\n%s", stderr, strings.Join(want, "\n"))
	}
	if !strings.Contains(stderr, "3 error") {
		t.Errorf("stderr has no %q summary", "3 error")
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// okLine matches the success line of config validate.
var okLine = regexp.MustCompile(`^(\S+): OK \(config version ([0-9a-f]{12}); (\d+) providers?, (\d+) models?; (\d+) warnings?\)$`)

func TestRunCommands(t *testing.T) {
	versionLine := version.Get().String() + "\n"
	tests := []struct {
		name       string
		env        env
		args       []string
		wantCode   int
		wantStdout string // exact, unless wantOutRE is set
		wantOutRE  string
		wantErrRE  string // stderr must match; "" means stderr must be empty
	}{
		{name: "version", args: []string{"version"}, wantStdout: versionLine},
		{name: "--version", args: []string{"--version"}, wantStdout: versionLine},
		{name: "help", args: []string{"help"}, wantStdout: usage},
		{name: "-h", args: []string{"-h"}, wantStdout: usage},
		{name: "--help", args: []string{"--help"}, wantStdout: usage},
		{name: "no arguments", args: nil, wantCode: exitUsage, wantErrRE: `(?s)^Switchyard .*Usage:`},
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: exitUsage,
			wantErrRE: `(?s)^error: unknown command "frobnicate"\n.*Usage:`},
		{name: "config without subcommand", args: []string{"config"}, wantCode: exitUsage,
			wantErrRE: `(?s)^error: config: missing subcommand\n.*Usage:`},
		{name: "unknown config subcommand", args: []string{"config", "check"}, wantCode: exitUsage,
			wantErrRE: `(?s)^error: config: unknown subcommand "check"\n.*Usage:`},
		{name: "unknown flag", args: []string{"config", "validate", "--bogus"}, wantCode: exitUsage,
			wantErrRE: `flag provided but not defined: -bogus`},
		{name: "extra argument", args: []string{"config", "validate", "-c", "testdata/valid.yaml", "extra"},
			wantCode: exitUsage, wantErrRE: `unexpected argument "extra"`},
		{
			name: "valid config with -c", args: []string{"config", "validate", "-c", "testdata/valid.yaml"},
			wantOutRE: `^testdata/valid.yaml:6: warning: server.admin_listen: admin listener is reachable from other hosts; never expose it publicly\n` +
				`testdata/valid.yaml: OK \(config version [0-9a-f]{12}; 2 providers, 1 model; 1 warning\)\n$`,
		},
		{
			name: "valid config with --config", args: []string{"config", "validate", "--config", "testdata/level-info.yaml"},
			wantOutRE: `^testdata/level-info.yaml: OK \(config version [0-9a-f]{12}; 1 provider, 1 model; 0 warnings\)\n$`,
		},
		{
			name: "SWITCHYARD_CONFIG", env: env{"SWITCHYARD_CONFIG": "testdata/level-info.yaml"},
			args:      []string{"config", "validate"},
			wantOutRE: `^testdata/level-info.yaml: OK `,
		},
		{
			name: "flag beats SWITCHYARD_CONFIG", env: env{"SWITCHYARD_CONFIG": "testdata/three-mistakes.yaml"},
			args:      []string{"config", "validate", "-c", "testdata/level-info.yaml"},
			wantOutRE: `^testdata/level-info.yaml: OK `,
		},
		{
			name: "missing file", args: []string{"config", "validate", "-c", "testdata/nope.yaml"},
			wantCode: exitError, wantErrRE: `^error: testdata/nope.yaml: no such file\n$`,
		},
		{
			name: "default path", args: []string{"config", "validate"},
			wantCode: exitError, wantErrRE: `^error: switchyard.yaml: no such file\n$`,
		},
		{
			name: "serve with a missing file", args: []string{"serve", "-c", "testdata/nope.yaml"},
			wantCode: exitError, wantErrRE: `^error: testdata/nope.yaml: no such file\n$`,
		},
		{
			name: "serve with an invalid config", args: []string{"serve", "-c", "testdata/three-mistakes.yaml"},
			wantCode: exitError, wantErrRE: `(?s)server.admin_lisen: unknown field.*\n3 errors, 0 warnings\n$`,
		},
		{
			name: "invalid --log-level", args: []string{"config", "validate", "-c", "testdata/level-info.yaml", "--log-level", "loud"},
			wantCode: exitError, wantErrRE: `^error: logging.level: .*\n1 error, 0 warnings\n$`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == nil {
				tt.env = env{}
			}
			code, stdout, stderr := runCLI(t, tt.env, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout, stderr)
			}
			if tt.wantOutRE != "" {
				if !regexp.MustCompile(tt.wantOutRE).MatchString(stdout) {
					t.Errorf("stdout = %q, want a match for %q", stdout, tt.wantOutRE)
				}
			} else if stdout != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			if tt.wantErrRE == "" {
				if stderr != "" {
					t.Errorf("stderr = %q, want nothing", stderr)
				}
			} else if !regexp.MustCompile(tt.wantErrRE).MatchString(stderr) {
				t.Errorf("stderr = %q, want a match for %q", stderr, tt.wantErrRE)
			}
		})
	}
}

// validateVersion runs config validate, requires success and returns the
// reported config version.
func validateVersion(t *testing.T, e env, args ...string) string {
	t.Helper()
	code, stdout, stderr := runCLI(t, e, append([]string{"config", "validate"}, args...)...)
	if code != exitOK {
		t.Fatalf("config validate %v: exit code %d\nstderr: %s", args, code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	m := okLine.FindStringSubmatch(lines[len(lines)-1])
	if m == nil {
		t.Fatalf("config validate %v: no OK line in %q", args, stdout)
	}
	return m[2]
}

// The two testdata files differ only in logging.level, so equal config
// versions prove which source set the effective level.
func TestOverridePrecedence(t *testing.T) {
	const infoFile, warnFile = "testdata/level-info.yaml", "testdata/level-warn.yaml"
	fileInfo := validateVersion(t, env{}, "-c", infoFile)
	fileWarn := validateVersion(t, env{}, "-c", warnFile)
	if fileInfo == fileWarn {
		t.Fatalf("both files have config version %s; the test needs them to differ", fileInfo)
	}

	t.Run("env beats file", func(t *testing.T) {
		got := validateVersion(t, env{"SWITCHYARD_LOG_LEVEL": "warn"}, "-c", infoFile)
		if got != fileWarn {
			t.Errorf("info file + SWITCHYARD_LOG_LEVEL=warn: version %s, want %s (the warn file's)", got, fileWarn)
		}
	})
	t.Run("flag beats env", func(t *testing.T) {
		got := validateVersion(t, env{"SWITCHYARD_LOG_LEVEL": "warn"}, "-c", warnFile, "--log-level", "info")
		if got != fileInfo {
			t.Errorf("warn file + env warn + --log-level info: version %s, want %s (the info file's)", got, fileInfo)
		}
	})
	t.Run("flag beats an invalid env value", func(t *testing.T) {
		code, _, stderr := runCLI(t, env{"SWITCHYARD_LOG_LEVEL": "loud"}, "config", "validate", "-c", infoFile)
		if code != exitError || !strings.Contains(stderr, "logging.level") {
			t.Fatalf("SWITCHYARD_LOG_LEVEL=loud: exit %d, stderr %q; want 1 naming logging.level", code, stderr)
		}
		got := validateVersion(t, env{"SWITCHYARD_LOG_LEVEL": "loud"}, "-c", infoFile, "--log-level", "warn")
		if got != fileWarn {
			t.Errorf("version %s, want %s", got, fileWarn)
		}
	})
	t.Run("listen overrides", func(t *testing.T) {
		e := env{"SWITCHYARD_LISTEN": "127.0.0.1:18080", "SWITCHYARD_ADMIN_LISTEN": "127.0.0.1:19090"}
		if got := validateVersion(t, e, "-c", infoFile); got == fileInfo {
			t.Errorf("SWITCHYARD_LISTEN and SWITCHYARD_ADMIN_LISTEN did not change the config version")
		}
		snap, err := config.Load(infoFile, config.LoadOptions{
			LookupEnv: env{}.lookup,
			Overrides: map[string]string{"server.listen": "127.0.0.1:18080", "server.admin_listen": "127.0.0.1:19090"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := validateVersion(t, e, "-c", infoFile); got != snap.Version() {
			t.Errorf("CLI version %s, want %s from Load with the same overrides", got, snap.Version())
		}
	})
	t.Run("empty env values are ignored", func(t *testing.T) {
		e := env{"SWITCHYARD_LISTEN": "", "SWITCHYARD_ADMIN_LISTEN": "", "SWITCHYARD_LOG_LEVEL": ""}
		if got := validateVersion(t, e, "-c", infoFile); got != fileInfo {
			t.Errorf("version %s, want %s", got, fileInfo)
		}
	})
}

// testSecrets are the secret values the secret tests use. The Anthropic key
// matches no credential pattern, so only exact-value scrubbing catches it.
var testSecrets = env{
	"OPENAI_API_KEY":    "sk-test-4f9a8b7c6d5e4f3a2b1c0d9e",
	"ANTHROPIC_API_KEY": "Zq8pL2xW9vN4mK7jR3tY",
	"REDIS_CACHE_URL":   "redis://:cachePassw0rd77@localhost:6379/0",
	"REDIS_STATE_URL":   "redis://:statePassw0rd77@localhost:6380/0",
	"DATABASE_URL":      "postgres://switchyard:dbPassw0rd77@localhost:5432/switchyard?sslmode=disable",
}

// secretFragments returns every secret value plus the passwords embedded in
// the URLs, none of which may ever be printed.
func secretFragments() []string {
	out := []string{"cachePassw0rd77", "statePassw0rd77", "dbPassw0rd77"}
	for _, v := range testSecrets {
		out = append(out, v)
	}
	return out
}

func TestConfigValidateNeverPrintsSecrets(t *testing.T) {
	code, stdout, stderr := runCLI(t, testSecrets, "config", "validate", "-c", "testdata/secret-type-error.yaml")
	if code != exitError {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, exitError, stderr)
	}
	for _, ref := range []string{"${REDIS_CACHE_URL}", "${DATABASE_URL}", "${OPENAI_API_KEY}", "${ANTHROPIC_API_KEY}"} {
		if !strings.Contains(stderr, ref) {
			t.Errorf("stderr does not name %s:\n%s", ref, stderr)
		}
	}
	if !strings.Contains(stderr, "4 errors") {
		t.Errorf("stderr has no 4 errors summary:\n%s", stderr)
	}
	for _, s := range secretFragments() {
		if strings.Contains(stdout+stderr, s) {
			t.Errorf("output contains the secret %q:\n%s%s", s, stdout, stderr)
		}
	}
}

func TestShippedConfigs(t *testing.T) {
	configs := filepath.Join("..", "..", "configs")
	tests := []struct {
		name string
		file string
		env  env
		// wantWarnings lists the expected warnings.
		wantWarnings []string
		wantProvs    int
		wantModels   int
		wantSources  int
	}{
		{
			// An empty environment: no variables at all.
			// It defines no API keys until the operator adds one.
			name: "quickstart", file: "quickstart.yaml", env: env{},
			wantWarnings: []string{"../../configs/quickstart.yaml: warning: tenants: no API keys configured; " +
				"every /v1 request will be rejected with 401 (create one with `switchyard keys generate`)"},
			wantProvs: 1, wantModels: 1, wantSources: 1,
		},
		{
			// Every variable the example and its include reference, set to a
			// dummy value. REDIS_CACHE_URL and REDIS_STATE_URL have defaults.
			name: "example", file: "switchyard.example.yaml",
			env: env{
				"DATABASE_URL":      "postgres://localhost:5433/switchyard",
				"OPENAI_API_KEY":    "dummy-openai",
				"ANTHROPIC_API_KEY": "dummy-anthropic",
				"GEMINI_API_KEY":    "dummy-gemini",
				"TOGETHER_API_KEY":  "dummy-together",
			},
			wantProvs: 5, wantModels: 5, wantSources: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(configs, tt.file)
			snap, err := config.Load(path, config.LoadOptions{LookupEnv: tt.env.lookup})
			if err != nil {
				t.Fatalf("Load(%s): %v", path, err)
			}
			var warnings []string
			for _, w := range snap.Warnings() {
				warnings = append(warnings, w.String())
			}
			if !equalLines(warnings, tt.wantWarnings) {
				t.Errorf("warnings = %q, want %q", warnings, tt.wantWarnings)
			}
			cfg := snap.Config()
			if len(cfg.Providers) != tt.wantProvs || len(cfg.Models) != tt.wantModels {
				t.Errorf("%d providers and %d models, want %d and %d",
					len(cfg.Providers), len(cfg.Models), tt.wantProvs, tt.wantModels)
			}
			if got := len(snap.Sources()); got != tt.wantSources {
				t.Errorf("sources = %q, want %d files", snap.Sources(), tt.wantSources)
			}

			// The CLI agrees.
			code, stdout, stderr := runCLI(t, tt.env, "config", "validate", "-c", path)
			if code != exitOK || stderr != "" {
				t.Fatalf("config validate: exit %d, stderr %q", code, stderr)
			}
			lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
			if !equalLines(lines[:len(lines)-1], warnings) {
				t.Errorf("config validate printed warnings %q, want %q", lines[:len(lines)-1], warnings)
			}
			if m := okLine.FindStringSubmatch(lines[len(lines)-1]); m == nil || m[2] != snap.Version() {
				t.Errorf("config validate stdout = %q, want an OK line with version %s", stdout, snap.Version())
			}
		})
	}
}
