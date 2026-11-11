package harness_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"switchyard/test/conformance/harness"
)

func TestGenerateKey(t *testing.T) {
	keyRE := regexp.MustCompile(`^sy_[A-Za-z0-9_-]{43}$`)
	hashRE := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for range 3 {
		key, hash, err := harness.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if !keyRE.MatchString(key) || !hashRE.MatchString(hash) {
			t.Fatalf("key %q, hash %q", key, hash)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "sy_"))
		if err != nil || len(raw) != 32 {
			t.Errorf("key %q does not encode 32 bytes: %v", key, err)
		}
		sum := sha256.Sum256([]byte(key))
		if want := "sha256:" + hex.EncodeToString(sum[:]); hash != want || harness.HashKey(key) != want {
			t.Errorf("hash %q, want %q", hash, want)
		}
		if seen[key] {
			t.Errorf("duplicate key %q", key)
		}
		seen[key] = true
	}
}

func TestConfigYAML(t *testing.T) {
	cfg := harness.Config{
		ProviderBaseURL: "http://127.0.0.1:1/v1",
		ProviderAPIKey:  "${OPENAI_API_KEY}",
		ForwardHeaders:  []string{"X-Forward-Me"},
		Timeouts:        harness.Timeouts{FirstToken: time.Second, Total: 30 * time.Second},
		Models:          []harness.Model{{ID: "cat", UpstreamModel: "up"}},
		KeyHash:         harness.HashKey("sy_x"),
	}
	b, err := cfg.YAML()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	want := map[string]any{
		"version": 1,
		"server": map[string]any{"listen": "127.0.0.1:0", "admin_listen": "127.0.0.1:0",
			"shutdown_delay": "0s", "shutdown_grace": "5s"},
		"logging": map[string]any{"level": "info", "format": "json"},
		"providers": []any{map[string]any{
			"id": "fake", "type": "openai", "base_url": "http://127.0.0.1:1/v1", "api_key": "${OPENAI_API_KEY}",
			"forward_headers": []any{"X-Forward-Me"},
			"timeouts":        map[string]any{"first_token": "1s", "total": "30s"},
		}},
		"models": []any{map[string]any{"id": "cat", "deployments": []any{
			map[string]any{"provider": "fake", "upstream_model": "up"}}}},
		"tenants": []any{map[string]any{"id": "t1", "keys": []any{
			map[string]any{"id": "k1", "hash": harness.HashKey("sy_x")}}}},
	}
	gotY, _ := yaml.Marshal(got)
	wantY, _ := yaml.Marshal(want)
	if string(gotY) != string(wantY) {
		t.Errorf("config:\n%s\nwant:\n%s", gotY, wantY)
	}

	b, err = harness.Config{ProviderBaseURL: "http://h/v1"}.YAML()
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"tenants", "timeouts", "api_key", "forward_headers"} {
		if strings.Contains(string(b), absent) {
			t.Errorf("minimal config has %s:\n%s", absent, b)
		}
	}
}

// TestGatewayLifecycle builds the real binary, checks that `config validate`
// accepts the rendered config, then starts and stops the gateway.
func TestGatewayLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	cfg := harness.Config{
		ProviderBaseURL: "http://127.0.0.1:1/v1",
		ProviderAPIKey:  "sk-lifecycle",
		Models:          []harness.Model{{ID: "cat", UpstreamModel: "up"}},
		KeyHash:         harness.HashKey("sy_lifecycle"),
		Timeouts:        harness.Timeouts{Connect: time.Second, FirstToken: 2 * time.Second, Idle: 3 * time.Second},
	}

	t.Run("config validate accepts the config", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		bin, err := harness.Build(ctx, dir, false)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "switchyard.yaml")
		if err := harness.WriteConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("config file mode %v (%v), want 0600", info.Mode().Perm(), err)
		}
		out, err := exec.CommandContext(ctx, bin, "config", "validate", "-c", path).CombinedOutput()
		if err != nil {
			t.Fatalf("config validate: %v\n%s", err, out)
		}
	})

	t.Run("start and stop", func(t *testing.T) {
		g := harness.Run(t, cfg, harness.RunOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, url := range []string{g.PublicURL + "/healthz", g.AdminURL + "/metrics"} {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d", url, resp.StatusCode)
			}
		}
		if err := g.Stop(10 * time.Second); err != nil {
			t.Errorf("Stop: %v", err)
		}
		if err := g.Stop(time.Second); err != nil {
			t.Errorf("second Stop: %v", err)
		}
		select {
		case <-g.Exited():
		default:
			t.Error("Exited not closed after Stop")
		}
		logs := strings.Join(g.Logs(), "\n")
		for _, msg := range []string{`"msg":"listening"`, `"msg":"shutdown complete"`} {
			if !strings.Contains(logs, msg) {
				t.Errorf("logs have no %s:\n%s", msg, logs)
			}
		}
	})

	t.Run("start fails on a bad config", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		bin, err := harness.Build(ctx, dir, false)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(path, []byte("version: 1\nnope: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if g, err := harness.Start(ctx, bin, path, harness.StartOptions{}); err == nil {
			_ = g.Stop(time.Second)
			t.Fatal("Start succeeded with a bad config")
		} else if !strings.Contains(err.Error(), "exited before listening") {
			t.Errorf("Start error %v", err)
		}
	})
}
