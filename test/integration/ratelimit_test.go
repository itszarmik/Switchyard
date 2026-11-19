//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// TestRateLimitsTwoReplicas checks PLAN P7's criterion through two gateway
// binaries sharing the real Redis state instance: one key's request rate
// and another key's concurrent-stream cap are enforced across both
// replicas together, not per replica, and the 429's Retry-After is the
// wait the shared rate implies.
func TestRateLimitsTwoReplicas(t *testing.T) {
	state, cacheURL := env(t, redisEnv), env(t, cacheRedisEnv)
	hold := make(chan struct{})
	up := fakeupstream.New(fakeupstream.Options{})
	t.Cleanup(up.Close)
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})

	// Random key IDs: the limiter's Redis keys are named after them, and
	// the server is shared with the other tests.
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	rateID, streamID := "rate-"+hex.EncodeToString(suffix), "stream-"+hex.EncodeToString(suffix)
	rateKey, rateHash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	streamKey, streamHash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// 6 a minute with a burst of 2: T = 10 s, τ = 20 s, so three requests
	// at once, then one every 10 s.
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 10s}
logging: {level: info, format: json}
storage:
  mode: redis
  redis: {cache_url: %q, state_url: %q}
providers:
  - {id: up, type: openai, base_url: %q}
models:
  - id: m
    capabilities: [streaming, stream_usage]
    deployments:
      - {provider: up, upstream_model: m-up, pricing: {input: "1", output: "2"}}
tenants:
  - id: limits
    keys:
      - {id: %s, hash: %q, rate_limit: {requests_per_minute: 6, burst: 2}}
      - {id: %s, hash: %q, rate_limit: {max_concurrent_streams: 1}}
`, cacheURL, state, up.BaseURL(), rateID, rateHash, streamID, streamHash)
	replicas := []*harness.Gateway{
		harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled}),
		harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled}),
	}
	client := &http.Client{Timeout: 30 * time.Second}
	post := func(ctx context.Context, g *harness.Gateway, key string, stream bool) (*http.Response, error) {
		body := fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.PublicURL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		return client.Do(req)
	}

	// The request rate: alternating replicas, three are allowed in all.
	type refusal struct {
		retryAfter string
		elapsed    time.Duration // from sending the first request to this answer
	}
	var statuses []int
	var refusals []refusal
	start := time.Now()
	for i := range 6 {
		resp, err := post(context.Background(), replicas[i%2], rateKey, false)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests {
			refusals = append(refusals, refusal{resp.Header.Get("Retry-After"), time.Since(start)})
			if !strings.Contains(string(b), `"code":"rate_limit_exceeded"`) {
				t.Errorf("429 body %s", b)
			}
		}
	}
	if want := []int{200, 200, 200, 429, 429, 429}; !slices.Equal(statuses, want) {
		t.Errorf("statuses %v, want %v: the rate is shared, not per replica", statuses, want)
	}
	for _, r := range refusals {
		// The next request is allowed one interval (10 s) after the
		// first was admitted, so Retry-After is ceil(10 s minus the time
		// since then), which is at most the time measured here: within
		// 1 s of 10 s less the elapsed time, and never above 10.
		want := 10 - r.elapsed.Seconds()
		if n, err := strconv.Atoi(r.retryAfter); err != nil || float64(n) < want-1 || n > 10 {
			t.Errorf("Retry-After %q %s after the first request, want %.1f s (-1 s) to 10 s", r.retryAfter, r.elapsed, want)
		}
	}

	// The stream cap: a stream held open on one replica fills the key's
	// only slot on the other too.
	up.Script(fakeupstream.Behavior{Hold: hold})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() {
		resp, err := post(ctx, replicas[0], streamKey, true)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if err == nil && resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		first <- err
	}()
	wctx, wcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer wcancel()
	if err := up.WaitReceived(wctx, 4); err != nil { // the three allowed requests, then the stream
		t.Fatalf("the held stream did not reach the upstream: %v", err)
	}
	resp, err := post(context.Background(), replicas[1], streamKey, true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" ||
		!strings.Contains(string(b), "concurrent streams") {
		t.Errorf("second stream on the other replica: %d, Retry-After %q, %s; want 429 with Retry-After 1",
			resp.StatusCode, resp.Header.Get("Retry-After"), b)
	}
	// Once the first stream ends, its slot is free on both replicas.
	close(hold)
	if err := <-first; err != nil {
		t.Fatalf("the held stream: %v", err)
	}
	up.Script(fakeupstream.Behavior{})
	resp, err = post(context.Background(), replicas[1], streamKey, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stream after the release: %d, want 200", resp.StatusCode)
	}
	for _, g := range replicas {
		if got := metric(t, g, `switchyard_ratelimit_decisions_total{kind="request",result="error"}`); got != 0 {
			t.Errorf("rate-limit store errors on a replica: %v", got)
		}
	}
}
