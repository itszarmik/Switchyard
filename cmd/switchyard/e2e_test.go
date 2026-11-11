package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// e2eConfig is the config the end-to-end test serves. Logs are at debug so
// the probe and scrape requests get access-log lines too.
const e2eConfig = `version: 1
server:
  listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:0"
  shutdown_delay: 1s
  shutdown_grace: 5s
logging:
  level: debug
  format: json
providers:
  - id: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY:-}
models:
  - id: gpt-5-mini
    deployments:
      - provider: openai
        upstream_model: gpt-5-mini
`

const (
	shutdownDelay = time.Second
	// e2eTimeout bounds every wait for the subprocess. It is generous so
	// that a slow, race-instrumented CI machine does not make the test flaky.
	e2eTimeout = 15 * time.Second
)

// logStream collects the subprocess's JSON log lines.
type logStream struct {
	mu    sync.Mutex
	lines []map[string]any
	raw   []string
	added chan struct{} // closed and replaced whenever a line is added
	done  chan struct{} // closed at EOF
}

func newLogStream(r io.Reader) *logStream {
	ls := &logStream{added: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(ls.done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			var m map[string]any
			_ = json.Unmarshal([]byte(line), &m) // non-JSON lines are kept raw and fail the test later
			ls.mu.Lock()
			ls.raw = append(ls.raw, line)
			ls.lines = append(ls.lines, m)
			close(ls.added)
			ls.added = make(chan struct{})
			ls.mu.Unlock()
		}
	}()
	return ls
}

func (ls *logStream) snapshot() (lines []map[string]any, raw []string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return append([]map[string]any(nil), ls.lines...), append([]string(nil), ls.raw...)
}

// waitFor returns the first line whose msg is msg. It returns false if the
// stream ends or timeout passes without one.
func (ls *logStream) waitFor(msg string, timeout time.Duration) (map[string]any, bool) {
	deadline := time.After(timeout)
	for {
		ls.mu.Lock()
		for _, m := range ls.lines {
			if m != nil && m["msg"] == msg {
				ls.mu.Unlock()
				return m, true
			}
		}
		added := ls.added
		ls.mu.Unlock()
		select {
		case <-added:
		case <-ls.done:
			// The last lines may have arrived after the check above.
			ls.mu.Lock()
			defer ls.mu.Unlock()
			for _, m := range ls.lines {
				if m != nil && m["msg"] == msg {
					return m, true
				}
			}
			return nil, false
		case <-deadline:
			return nil, false
		}
	}
}

// serveProcess is `switchyard serve` running in a subprocess: the test
// binary re-executed as the CLI.
type serveProcess struct {
	cmd    *exec.Cmd
	logs   *logStream
	stderr *strings.Builder
	exited chan error
	reaped bool // set once the test has received from exited
	// public and admin are the base URLs of the bound listeners.
	public, admin string
}

// startServe runs serve -c cfgPath and waits for its listening log line. The
// process is killed when the test ends, unless the test waited for it.
func startServe(t *testing.T, cfgPath string) *serveProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "serve", "-c", cfgPath)
	// Only what the CLI needs, so the developer's own SWITCHYARD_* or
	// OPENAI_API_KEY variables cannot change the result.
	cmd.Env = []string{reexecEnv + "=1", "PATH=" + os.Getenv("PATH")}
	if testing.CoverMode() != "" {
		// A coverage-instrumented binary warns on stderr without it.
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+t.TempDir())
	}
	p := &serveProcess{cmd: cmd, stderr: &strings.Builder{}, exited: make(chan error, 1)}
	cmd.Stderr = p.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.logs = newLogStream(stdout)
	go func() {
		<-p.logs.done // Wait must not run before the pipe is drained
		p.exited <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if !p.reaped {
			_ = cmd.Process.Kill()
			<-p.exited
		}
	})

	m, ok := p.logs.waitFor("listening", e2eTimeout)
	if !ok {
		select {
		case err := <-p.exited:
			p.reaped = true
			t.Fatalf("serve exited before listening: %v\nstderr: %s", err, p.stderr.String())
		default:
			t.Fatalf("no listening log line within %s", e2eTimeout)
		}
	}
	public, _ := m["public_addr"].(string)
	admin, _ := m["admin_addr"].(string)
	if public == "" || admin == "" {
		t.Fatalf("listening line has public_addr %q and admin_addr %q", public, admin)
	}
	p.public, p.admin = "http://"+public, "http://"+admin
	return p
}

// signal sends sig to the process.
func (p *serveProcess) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// wait waits up to timeout for the process to exit and returns its error.
func (p *serveProcess) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-p.exited:
		p.reaped = true
		return err
	case <-time.After(timeout):
		t.Fatalf("serve did not exit within %s", timeout)
		return nil
	}
}

// httpResult is a fully read HTTP response.
type httpResult struct {
	status int
	header http.Header
	body   string
}

func get(t *testing.T, c *http.Client, url string) (httpResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return httpResult{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return httpResult{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, header: resp.Header, body: string(b)}, err
}

func mustGet(t *testing.T, c *http.Client, url string) httpResult {
	t.Helper()
	r, err := get(t, c, url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return r
}

// TestServeEndToEnd runs the real CLI in a subprocess: it serves, answers
// the built-in routes, and shuts down gracefully on SIGTERM.
func TestServeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a subprocess; skipped with -short")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(cfgPath, []byte(e2eConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	wantVersion := validateVersion(t, env{}, "-c", cfgPath)

	p := startServe(t, cfgPath)
	public, admin := p.public, p.admin

	// A fresh connection per request, so no idle keep-alive connection is
	// left to the server when it shuts down.
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	t.Run("healthz", func(t *testing.T) {
		if r := mustGet(t, c, public+"/healthz"); r.status != http.StatusOK {
			t.Errorf("GET /healthz = %d %q, want 200", r.status, r.body)
		}
	})
	t.Run("readyz reports the config version", func(t *testing.T) {
		r := mustGet(t, c, public+"/readyz")
		var body struct {
			Status        string `json:"status"`
			ConfigVersion string `json:"config_version"`
		}
		if err := json.Unmarshal([]byte(r.body), &body); err != nil {
			t.Fatalf("GET /readyz body %q: %v", r.body, err)
		}
		if r.status != http.StatusOK || body.Status != "ready" || body.ConfigVersion != wantVersion {
			t.Errorf("GET /readyz = %d %q, want 200 ready with config_version %s (from config validate)",
				r.status, r.body, wantVersion)
		}
	})
	t.Run("metrics on the admin listener", func(t *testing.T) {
		r := mustGet(t, c, admin+"/metrics")
		if r.status != http.StatusOK {
			t.Fatalf("admin GET /metrics = %d", r.status)
		}
		for _, name := range []string{"switchyard_build_info", "switchyard_http_requests_total"} {
			if !strings.Contains(r.body, name) {
				t.Errorf("admin /metrics has no %s", name)
			}
		}
	})
	t.Run("no metrics on the public listener", func(t *testing.T) {
		r := mustGet(t, c, public+"/metrics")
		var envelope struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(r.body), &envelope); err != nil {
			t.Fatalf("public GET /metrics body %q: %v", r.body, err)
		}
		if r.status != http.StatusNotFound || envelope.Error.Type != "invalid_request_error" ||
			!strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
			t.Errorf("public GET /metrics = %d %q (%s), want a 404 JSON error envelope",
				r.status, r.body, r.header.Get("Content-Type"))
		}
	})

	// SIGTERM: readiness must fail at once, while the server keeps serving
	// for shutdown_delay.
	sigSent := time.Now()
	p.signal(t, syscall.SIGTERM)
	var notReady httpResult
	for {
		r, err := get(t, c, public+"/readyz")
		if err != nil {
			t.Fatalf("GET /readyz after SIGTERM failed before it returned 503: %v", err)
		}
		if r.status == http.StatusServiceUnavailable {
			notReady = r
			break
		}
		if time.Since(sigSent) > shutdownDelay {
			t.Fatalf("/readyz still %d %s after SIGTERM, past the shutdown delay", r.status, time.Since(sigSent))
		}
		time.Sleep(10 * time.Millisecond) // polling another process; there is no signal to wait on
	}
	if failedAfter := time.Since(sigSent); failedAfter >= shutdownDelay {
		t.Errorf("/readyz began failing %s after SIGTERM, not within the %s delay", failedAfter, shutdownDelay)
	}
	if !strings.Contains(notReady.body, `"reason":"shutting_down"`) {
		t.Errorf("/readyz during shutdown = %q, want reason shutting_down", notReady.body)
	}

	if err := p.wait(t, shutdownDelay+e2eTimeout); err != nil {
		t.Fatalf("serve exited with %v, want exit code 0\nstderr: %s", err, p.stderr.String())
	}
	if elapsed := time.Since(sigSent); elapsed < shutdownDelay {
		t.Errorf("serve exited %s after SIGTERM, before the %s shutdown delay", elapsed, shutdownDelay)
	}
	if p.stderr.Len() > 0 {
		t.Errorf("stderr = %q, want nothing (logs go to stdout)", p.stderr.String())
	}

	lines, raw := p.logs.snapshot()
	msgs := map[string]int{}
	requests := 0
	for i, m := range lines {
		if m == nil {
			t.Errorf("log line %d is not JSON: %q", i, raw[i])
			continue
		}
		msg, _ := m["msg"].(string)
		msgs[msg]++
		if msg == "request" {
			requests++
			if id, _ := m["request_id"].(string); id == "" {
				t.Errorf("request log line without request_id: %s", raw[i])
			}
		}
	}
	// healthz, readyz, admin metrics, public metrics, then at least one
	// readyz poll during shutdown.
	if requests < 5 {
		t.Errorf("%d request log lines, want at least 5:\n%s", requests, strings.Join(raw, "\n"))
	}
	for _, msg := range []string{"config loaded", "switchyard starting", "listening",
		"signal received; shutting down", "shutdown started", "shutdown complete"} {
		if msgs[msg] != 1 {
			t.Errorf("%d %q log lines, want 1", msgs[msg], msg)
		}
	}
}

// TestServeSecondSignalForcesShutdown checks that a second signal cuts a
// long shutdown delay short: with shutdown_delay at 1h, SIGTERM then SIGINT
// must make serve exit promptly, and cleanly since nothing is in flight.
func TestServeSecondSignalForcesShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a subprocess; skipped with -short")
	}
	cfgPath := filepath.Join(t.TempDir(), "switchyard.yaml")
	cfg := strings.Replace(e2eConfig, "shutdown_delay: 1s", "shutdown_delay: 1h", 1)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startServe(t, cfgPath)

	p.signal(t, syscall.SIGTERM)
	if _, ok := p.logs.waitFor("shutdown started", e2eTimeout); !ok {
		t.Fatalf("no shutdown started line after SIGTERM\nstderr: %s", p.stderr.String())
	}
	p.signal(t, syscall.SIGINT)
	if err := p.wait(t, e2eTimeout); err != nil {
		t.Fatalf("serve exited with %v, want exit code 0\nstderr: %s", err, p.stderr.String())
	}
	if p.stderr.Len() > 0 {
		t.Errorf("stderr = %q, want nothing", p.stderr.String())
	}
	for _, msg := range []string{"signal received; shutting down", "second signal received; aborting in-flight requests", "shutdown complete"} {
		if _, ok := p.logs.waitFor(msg, 0); !ok {
			_, raw := p.logs.snapshot()
			t.Errorf("no %q log line:\n%s", msg, strings.Join(raw, "\n"))
		}
	}
}
