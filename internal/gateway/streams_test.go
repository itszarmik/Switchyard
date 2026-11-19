package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"switchyard/internal/ratelimit"
)

// fakeStreams is a StreamLimiter with a cap of max streams in all; while
// one is held it records how many the pipeline holds.
type fakeStreams struct {
	mu       sync.Mutex
	max      int
	held     int
	acquired int
}

func (f *fakeStreams) AcquireStream(context.Context) (func(), *ratelimit.Rejection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held >= f.max {
		return func() {}, ratelimit.StreamLimited(f.max)
	}
	f.held++
	f.acquired++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.held--
			f.mu.Unlock()
		})
	}, nil
}

func (f *fakeStreams) counts() (held, acquired int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held, f.acquired
}

// TestStreamCap: a streaming chat or legacy completion request takes a
// stream slot once decoded and gives it back when its response ends, a
// cached replay included; a non-streaming request takes none, and a
// refusal is the limiter's 429 with Retry-After, before any upstream call.
func TestStreamCap(t *testing.T) {
	const legacyStream = `{"model":"m","prompt":"hi","stream":true}`
	tests := []struct {
		name         string
		endpoint     string
		body         string
		max          int
		wantStatus   int
		wantAcquired int
		wantCalls    int
	}{
		{"chat stream", EndpointChatCompletions, streamReq, 1, http.StatusOK, 1, 1},
		{"legacy stream", EndpointCompletions, legacyStream, 1, http.StatusOK, 1, 1},
		{"non-streaming", EndpointChatCompletions, chatReq, 0, http.StatusOK, 0, 1},
		{"refused", EndpointChatCompletions, streamReq, 0, http.StatusTooManyRequests, 0, 0},
		{"legacy refused", EndpointCompletions, legacyStream, 0, http.StatusTooManyRequests, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCacheEnv(t, exactOn, nil, CacheOptions{}, nil)
			fs := &fakeStreams{max: tt.max}
			e.g.streams = fs
			r := e.do(tt.endpoint, tt.body, nil)
			e.wait()
			if r.status != tt.wantStatus {
				t.Fatalf("status %d, want %d (%v)", r.status, tt.wantStatus, r.err)
			}
			held, acquired := fs.counts()
			if held != 0 || acquired != tt.wantAcquired {
				t.Errorf("held %d, acquired %d; want 0, %d", held, acquired, tt.wantAcquired)
			}
			if got := e.a.count(); got != tt.wantCalls {
				t.Errorf("upstream calls %d, want %d", got, tt.wantCalls)
			}
			if tt.wantStatus == http.StatusTooManyRequests {
				if r.err == nil || r.err.Code != ratelimit.CodeRateLimited || r.err.RetryAfter != time.Second {
					t.Errorf("error %+v, want rate_limit_exceeded with Retry-After 1s", r.err)
				}
				if r.ann["error_code"] != ratelimit.CodeRateLimited {
					t.Errorf("access log error_code %v", r.ann["error_code"])
				}
			}
		})
	}

	t.Run("a cached replay takes a slot", func(t *testing.T) {
		e := newCacheEnv(t, exactOn, nil, CacheOptions{}, nil)
		fs := &fakeStreams{max: 1}
		e.g.streams = fs
		for i, want := range []string{"miss", "exact"} {
			r := e.do(EndpointChatCompletions, streamReq, nil)
			e.wait()
			if r.status != http.StatusOK || r.info.Cache != want {
				t.Fatalf("request %d: %d, cache %q; want 200, %q", i, r.status, r.info.Cache, want)
			}
		}
		if held, acquired := fs.counts(); held != 0 || acquired != 2 {
			t.Errorf("held %d, acquired %d; want 0, 2", held, acquired)
		}
		// With the slot taken, the hit is refused too.
		release, _ := fs.AcquireStream(context.Background())
		defer release()
		if r := e.do(EndpointChatCompletions, streamReq, nil); r.status != http.StatusTooManyRequests {
			t.Errorf("replay with the cap reached: %d, want 429", r.status)
		}
	})
}
