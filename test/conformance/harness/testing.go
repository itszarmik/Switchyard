package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTimeout bounds go build; a race build with a cold cache is slow.
const buildTimeout = 5 * time.Minute

// stopTimeout bounds the graceful shutdown at the end of a test.
const stopTimeout = 15 * time.Second

// RunOptions configure Run.
type RunOptions struct {
	// Race builds the gateway with the race detector, so a data race in it
	// fails the test.
	Race bool
	// Env holds extra KEY=VALUE environment variables for the gateway.
	Env []string
}

// Run builds the gateway into a temporary directory, writes cfg, starts the
// gateway and stops it when the test ends. At the end of the test it reports
// a failed shutdown and any data race the gateway printed and, if the test
// failed, logs the gateway's output.
func Run(t testing.TB, cfg Config, opts RunOptions) *Gateway {
	t.Helper()
	b, err := cfg.YAML()
	if err != nil {
		t.Fatal(err)
	}
	return RunYAML(t, b, opts)
}

// RunYAML is Run with a config file written by the caller, for gateways
// that Config cannot describe (several providers, aliases, prices,
// routing settings). The file should bind both listeners to 127.0.0.1:0
// and log JSON, as Config.YAML does.
func RunYAML(t testing.TB, config []byte, opts RunOptions) *Gateway {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	bin, err := Build(ctx, dir, opts.Race)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(cfgPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := Start(ctx, bin, cfgPath, StartOptions{Env: opts.Env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Stop(stopTimeout); err != nil {
			t.Errorf("stopping the gateway: %v", err)
		}
		stderr := g.Stderr()
		if strings.Contains(stderr, "WARNING: DATA RACE") {
			t.Errorf("the gateway reported a data race:\n%s", stderr)
		}
		if t.Failed() {
			t.Logf("gateway stderr:\n%s", stderr)
			t.Logf("gateway logs:\n%s", strings.Join(g.Logs(), "\n"))
		}
	})
	return g
}
