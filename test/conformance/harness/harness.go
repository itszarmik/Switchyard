// Package harness runs the switchyard gateway as a black box for the
// conformance, load and smoke tests. It builds the binary with go build,
// writes a config, starts `switchyard serve` and finds the listeners from the
// JSON "listening" log line. It talks to the gateway only through its CLI and
// HTTP API and never imports switchyard/internal/....
package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// Tenant and key IDs of the key a Config authorizes.
const (
	TenantID = "t1"
	KeyID    = "k1"
	// ProviderID is the ID of the single provider a Config defines.
	ProviderID = "fake"
)

// GenerateKey returns a new gateway API key and its config hash, in the
// contract's format: the key is "sy_" plus the unpadded base64url encoding
// of 32 random bytes, and the hash is "sha256:" plus the lowercase hex
// SHA-256 of the key.
func GenerateKey() (key, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("harness: generating a key: %w", err)
	}
	key = "sy_" + base64.RawURLEncoding.EncodeToString(b[:])
	return key, HashKey(key), nil
}

// HashKey returns the config hash of key: "sha256:" plus the lowercase hex
// SHA-256 of the key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Model maps a catalog model ID to the provider's model name.
type Model struct {
	ID            string
	UpstreamModel string
	// Capabilities is the model's capability list. A request that needs a
	// capability the model lacks is never sent upstream, so a model that
	// must stream needs at least "streaming". Nil writes no list.
	Capabilities []string
}

// Timeouts are the provider timeouts. Zero fields keep the defaults.
type Timeouts struct {
	Connect, FirstToken, Idle, Total time.Duration
}

// Config describes a gateway with one provider, ProviderID, that serves
// every model, and one tenant, TenantID, with one key, KeyID.
type Config struct {
	// ProviderBaseURL is the provider's base_url, such as
	// https://api.openai.com/v1.
	ProviderBaseURL string
	// ProviderAPIKey is written to the config as is, so it may be a ${VAR}
	// reference that the gateway expands from its environment.
	ProviderAPIKey string
	// ProviderType is the provider's wire protocol, openai (the default)
	// or anthropic.
	ProviderType string
	// ForwardHeaders lists the client headers the provider may receive.
	ForwardHeaders []string
	Timeouts       Timeouts
	Models         []Model
	// KeyHash is the hash of the tenant's key. When empty, no tenant is
	// configured and every /v1 request is rejected.
	KeyHash string
	// LogLevel is the gateway's log level; default info.
	LogLevel string
}

// YAML renders c as a switchyard config file. Both listeners bind
// 127.0.0.1:0, and logs are JSON so the listening line can be parsed.
func (c Config) YAML() ([]byte, error) {
	type (
		timeouts struct {
			Connect    string `yaml:"connect,omitempty"`
			FirstToken string `yaml:"first_token,omitempty"`
			Idle       string `yaml:"idle,omitempty"`
			Total      string `yaml:"total,omitempty"`
		}
		provider struct {
			ID             string    `yaml:"id"`
			Type           string    `yaml:"type"`
			BaseURL        string    `yaml:"base_url"`
			APIKey         string    `yaml:"api_key,omitempty"`
			ForwardHeaders []string  `yaml:"forward_headers,omitempty"`
			Timeouts       *timeouts `yaml:"timeouts,omitempty"`
		}
		deployment struct {
			Provider      string `yaml:"provider"`
			UpstreamModel string `yaml:"upstream_model"`
		}
		model struct {
			ID           string       `yaml:"id"`
			Capabilities []string     `yaml:"capabilities,omitempty"`
			Deployments  []deployment `yaml:"deployments"`
		}
		key struct {
			ID   string `yaml:"id"`
			Hash string `yaml:"hash"`
		}
		tenant struct {
			ID   string `yaml:"id"`
			Keys []key  `yaml:"keys"`
		}
		file struct {
			Version int               `yaml:"version"`
			Server  map[string]string `yaml:"server"`
			Logging map[string]string `yaml:"logging"`
			Provs   []provider        `yaml:"providers"`
			Models  []model           `yaml:"models"`
			Tenants []tenant          `yaml:"tenants,omitempty"`
		}
	)
	dur := func(d time.Duration) string {
		if d == 0 {
			return ""
		}
		return d.String()
	}
	level := c.LogLevel
	if level == "" {
		level = "info"
	}
	typ := c.ProviderType
	if typ == "" {
		typ = "openai"
	}
	p := provider{
		ID: ProviderID, Type: typ, BaseURL: c.ProviderBaseURL, APIKey: c.ProviderAPIKey,
		ForwardHeaders: c.ForwardHeaders,
	}
	if c.Timeouts != (Timeouts{}) {
		p.Timeouts = &timeouts{
			Connect: dur(c.Timeouts.Connect), FirstToken: dur(c.Timeouts.FirstToken),
			Idle: dur(c.Timeouts.Idle), Total: dur(c.Timeouts.Total),
		}
	}
	f := file{
		Version: 1,
		Server: map[string]string{
			"listen": "127.0.0.1:0", "admin_listen": "127.0.0.1:0",
			"shutdown_delay": "0s", "shutdown_grace": "5s",
		},
		Logging: map[string]string{"level": level, "format": "json"},
		Provs:   []provider{p},
	}
	for _, m := range c.Models {
		f.Models = append(f.Models, model{ID: m.ID, Capabilities: m.Capabilities,
			Deployments: []deployment{{Provider: ProviderID, UpstreamModel: m.UpstreamModel}}})
	}
	if c.KeyHash != "" {
		f.Tenants = []tenant{{ID: TenantID, Keys: []key{{ID: KeyID, Hash: c.KeyHash}}}}
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("harness: rendering the config: %w", err)
	}
	return b, nil
}

// WriteConfig writes c to path, readable only by its owner.
func WriteConfig(path string, c Config) error {
	b, err := c.YAML()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("harness: writing the config: %w", err)
	}
	return nil
}

// Build compiles switchyard/cmd/switchyard into dir with the go command on
// PATH and returns the binary's path. It must run inside the switchyard
// module. With race, the binary is built with the race detector.
func Build(ctx context.Context, dir string, race bool) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("harness: finding the go command: %w", err)
	}
	out := filepath.Join(dir, "switchyard")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	args := []string{"build", "-o", out}
	if race {
		args = append(args, "-race")
	}
	args = append(args, "switchyard/cmd/switchyard")
	cmd := exec.CommandContext(ctx, goBin, args...) // #nosec G204 -- fixed arguments
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("harness: go build switchyard/cmd/switchyard: %w\n%s", err, output)
	}
	return out, nil
}

// StartOptions configure Start.
type StartOptions struct {
	// Env holds extra KEY=VALUE environment variables. The gateway gets only
	// these and PATH, so the caller's own SWITCHYARD_* or OPENAI_* variables
	// cannot change its behavior.
	Env []string
	// DiscardLogs drops the log lines after the listening line instead of
	// keeping them, for long load runs.
	DiscardLogs bool
	// StartTimeout bounds the wait for the listening line; default 30s.
	StartTimeout time.Duration
}

// Gateway is a running `switchyard serve` process.
type Gateway struct {
	// PublicURL and AdminURL are the base URLs of the bound listeners, such
	// as http://127.0.0.1:1234.
	PublicURL, AdminURL string

	cmd      *exec.Cmd
	stderr   lockedBuffer
	exited   chan struct{} // closed once the process has been waited for
	waitErr  error         // set before exited is closed
	stopOnce sync.Once

	mu    sync.Mutex
	lines []string
}

// Start runs `bin serve -c cfgPath` and waits for its listening line. On
// success, the caller must call Stop.
func Start(ctx context.Context, bin, cfgPath string, opts StartOptions) (*Gateway, error) {
	timeout := opts.StartTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	g := &Gateway{exited: make(chan struct{})}
	// ctx bounds the start only; Stop ends the process gracefully.
	g.cmd = exec.CommandContext(context.WithoutCancel(ctx), bin, "serve", "-c", cfgPath) // #nosec G204 -- the binary this package built
	g.cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, opts.Env...)
	g.cmd.Stderr = &g.stderr
	stdout, err := g.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("harness: %w", err)
	}
	if err := g.cmd.Start(); err != nil {
		return nil, fmt.Errorf("harness: starting %s: %w", bin, err)
	}

	listening := make(chan [2]string, 1)
	go func() {
		g.readLogs(stdout, listening, opts.DiscardLogs)
		// Wait only once the pipe is drained, as os/exec requires.
		g.waitErr = g.cmd.Wait()
		close(g.exited)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var fail error
	select {
	case addrs := <-listening:
		g.PublicURL, g.AdminURL = "http://"+addrs[0], "http://"+addrs[1]
		return g, nil
	case <-g.exited:
		return nil, fmt.Errorf("harness: switchyard serve exited before listening: %w\n%s", g.waitErr, g.diagnostics())
	case <-ctx.Done():
		fail = fmt.Errorf("harness: waiting for the listening line: %w", ctx.Err())
	case <-timer.C:
		fail = fmt.Errorf("harness: no listening line within %s", timeout)
	}
	_ = g.cmd.Process.Kill()
	<-g.exited
	return nil, fmt.Errorf("%w\n%s", fail, g.diagnostics())
}

// readLogs reads the process's stdout line by line until EOF. It sends the
// listening addresses once, and keeps the lines unless discard is set.
func (g *Gateway) readLogs(stdout io.Reader, listening chan<- [2]string, discard bool) {
	r := bufio.NewReaderSize(stdout, 64<<10)
	found := false
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			line = strings.TrimSuffix(line, "\n")
			if !found {
				var m struct {
					Msg        string `json:"msg"`
					PublicAddr string `json:"public_addr"`
					AdminAddr  string `json:"admin_addr"`
				}
				if json.Unmarshal([]byte(line), &m) == nil && m.Msg == "listening" && m.PublicAddr != "" && m.AdminAddr != "" {
					found = true
					listening <- [2]string{m.PublicAddr, m.AdminAddr}
				}
			}
			if !found || !discard {
				g.mu.Lock()
				g.lines = append(g.lines, line)
				g.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// Logs returns the log lines read so far.
func (g *Gateway) Logs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.lines...)
}

// Stderr returns what the process wrote to stderr so far.
func (g *Gateway) Stderr() string { return g.stderr.String() }

// Exited is closed once the process has exited.
func (g *Gateway) Exited() <-chan struct{} { return g.exited }

// Stop sends SIGTERM and waits up to timeout for the process to exit, then
// kills it. It returns the process's exit error, or an error if it had to be
// killed. Calling it again returns the same result.
func (g *Gateway) Stop(timeout time.Duration) error {
	var killed bool
	g.stopOnce.Do(func() {
		select {
		case <-g.exited:
			return
		default:
		}
		if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			_ = g.cmd.Process.Kill()
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-g.exited:
		case <-timer.C:
			killed = true
			_ = g.cmd.Process.Kill()
			<-g.exited
		}
	})
	<-g.exited
	if killed {
		return fmt.Errorf("harness: switchyard serve did not exit within %s of SIGTERM and was killed", timeout)
	}
	if g.waitErr != nil {
		return fmt.Errorf("harness: switchyard serve: %w", g.waitErr)
	}
	return nil
}

// diagnostics summarizes stderr and the last log lines for error messages.
func (g *Gateway) diagnostics() string {
	lines := g.Logs()
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	return fmt.Sprintf("stderr:\n%s\nlast log lines:\n%s", g.Stderr(), strings.Join(lines, "\n"))
}

// lockedBuffer is a bytes.Buffer safe for one writer and concurrent readers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("harness: buffering stderr: %w", err)
	}
	return n, nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
