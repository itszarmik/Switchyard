package loadkit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func post(t *testing.T, u *Upstream, body string, seq int) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.BaseURL()+"/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SeqHeader, strconv.Itoa(seq))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestUpstreamAnswers(t *testing.T) {
	u := NewUpstream(UpstreamOptions{Latency: 2 * time.Millisecond, Chunks: 4, RecordTimings: true})
	defer u.Close()
	tests := []struct {
		name      string
		body      string
		wantParts []string
	}{
		{
			name:      "non-stream",
			body:      `{"model":"m1","messages":[]}`,
			wantParts: []string{`"object":"chat.completion"`, `"model":"m1"`, `"content":"` + Content + `"`, `"prompt_tokens":12`},
		},
		{
			name:      "stream without usage",
			body:      `{"model":"m2","stream":true}`,
			wantParts: []string{`"object":"chat.completion.chunk"`, `"finish_reason":"stop"`, "data: [DONE]\n\n"},
		},
		{
			name:      "stream with usage",
			body:      `{"model":"m3","stream":true,"stream_options":{"include_usage":true}}`,
			wantParts: []string{`"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":24`, "data: [DONE]\n\n"},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(t, u, tt.body, i)
			if status != http.StatusOK {
				t.Fatalf("status %d: %s", status, body)
			}
			for _, want := range tt.wantParts {
				if !strings.Contains(body, want) {
					t.Errorf("body lacks %q:\n%s", want, body)
				}
			}
			if !strings.Contains(tt.body, `"stream":true`) {
				var v map[string]any
				if err := json.Unmarshal([]byte(body), &v); err != nil {
					t.Errorf("non-stream body is not JSON: %v", err)
				}
			} else {
				var text strings.Builder
				for _, line := range strings.Split(body, "\n") {
					data, ok := strings.CutPrefix(line, "data: ")
					if !ok || data == "[DONE]" {
						continue
					}
					var c struct {
						Choices []struct {
							Delta struct{ Content string } `json:"delta"`
						} `json:"choices"`
					}
					if err := json.Unmarshal([]byte(data), &c); err != nil {
						t.Fatalf("bad event %q: %v", data, err)
					}
					for _, ch := range c.Choices {
						text.WriteString(ch.Delta.Content)
					}
				}
				if text.String() != Content {
					t.Errorf("streamed content = %q, want %q", text.String(), Content)
				}
			}
			d, ok := u.Timing(i)
			if !ok || d < 2*time.Millisecond {
				t.Errorf("Timing(%d) = %s, %v; want at least the 2ms latency", i, d, ok)
			}
		})
	}
	if _, ok := u.Timing(99); ok {
		t.Error("Timing of a sequence number never sent is set")
	}
	if status, _ := post(t, u, `not json`, 100); status != http.StatusBadRequest {
		t.Errorf("invalid JSON: status %d, want 400", status)
	}
}

func TestUpstreamErrorRate(t *testing.T) {
	tests := []struct {
		rate float64
	}{{0}, {0.25}, {1}}
	for _, tt := range tests {
		t.Run(strconv.FormatFloat(tt.rate, 'f', -1, 64), func(t *testing.T) {
			u := NewUpstream(UpstreamOptions{ErrorRate: tt.rate, Seed: 7})
			defer u.Close()
			const n = 400
			failed := 0
			for i := range n {
				status, body := post(t, u, `{"model":"m"}`, i)
				switch status {
				case http.StatusOK:
				case http.StatusInternalServerError:
					if !strings.Contains(body, `"type":"server_error"`) {
						t.Fatalf("injected error body = %s", body)
					}
					failed++
				default:
					t.Fatalf("status %d", status)
				}
			}
			if u.Requests() != n || u.Injected() != int64(failed) {
				t.Fatalf("Requests %d, Injected %d; want %d and %d", u.Requests(), u.Injected(), n, failed)
			}
			// Four standard deviations of the binomial.
			if want := tt.rate * n; math.Abs(float64(failed)-want) > 4*math.Sqrt(n*tt.rate*(1-tt.rate))+0.5 {
				t.Errorf("%d of %d failed, want about %.0f", failed, n, want)
			}
		})
	}
}

func TestOpenLoop(t *testing.T) {
	t.Run("fires every due request once", func(t *testing.T) {
		var fired atomic.Int64
		seen := make([]atomic.Bool, 100)
		start := time.Now()
		n := OpenLoop(context.Background(), 1000, start, start.Add(50*time.Millisecond), func(i int, due time.Time) {
			if want := start.Add(time.Duration(i) * time.Millisecond); !due.Equal(want) {
				t.Errorf("request %d due %s, want %s", i, due.Sub(start), want.Sub(start))
			}
			if seen[i].Swap(true) {
				t.Errorf("request %d fired twice", i)
			}
			fired.Add(1)
		})
		if n != 50 || fired.Load() != 50 {
			t.Fatalf("OpenLoop fired %d (returned %d), want 50", fired.Load(), n)
		}
	})
	t.Run("a slow fire does not slow the arrivals", func(t *testing.T) {
		release := make(chan struct{})
		var started atomic.Int64
		start := time.Now()
		done := make(chan int)
		go func() {
			done <- OpenLoop(context.Background(), 1000, start, start.Add(20*time.Millisecond), func(int, time.Time) {
				if started.Add(1) == 20 {
					close(release)
				}
				<-release
			})
		}()
		if n := <-done; n != 20 {
			t.Fatalf("fired %d, want 20 while every earlier fire was still blocked", n)
		}
	})
	t.Run("a canceled context stops it", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if n := OpenLoop(ctx, 10, start.Add(time.Second), start.Add(time.Hour), func(int, time.Time) {}); n != 0 {
			t.Fatalf("fired %d after cancel, want 0", n)
		}
	})
}

func TestPercentileAndHost(t *testing.T) {
	sorted := []time.Duration{1, 2, 3, 4}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{0.25, 1}, {0.5, 2}, {0.99, 4}, {1, 4}} {
		if got := Percentile(sorted, tc.p); got != tc.want {
			t.Errorf("Percentile(%v) = %d, want %d", tc.p, got, tc.want)
		}
	}
	if Percentile(nil, 0.5) != 0 {
		t.Error("Percentile of nothing is not 0")
	}
	h := DescribeHost(context.Background())
	if h.Cores < 1 || h.GoVersion == "" || !strings.Contains(h.String(), "cores") {
		t.Errorf("DescribeHost = %+v (%s)", h, h)
	}
	if LoadAverage(context.Background()) == "" {
		t.Error("LoadAverage is empty")
	}
}

// TestCollector: the collector counts the spans of every protobuf export
// and rejects what is not one.
func TestCollector(t *testing.T) {
	c := NewCollector()
	defer c.Close()
	export := func(spans ...int) []byte {
		td := &tracepb.TracesData{}
		for _, n := range spans {
			ss := &tracepb.ScopeSpans{}
			for range n {
				ss.Spans = append(ss.Spans, &tracepb.Span{Name: "s"})
			}
			td.ResourceSpans = append(td.ResourceSpans, &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{ss}})
		}
		b, err := proto.Marshal(td)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct {
		name, contentType string
		body              []byte
		wantStatus        int
	}{
		{"one resource", "application/x-protobuf", export(3), http.StatusOK},
		{"two resources", "application/x-protobuf", export(2, 4), http.StatusOK},
		{"not protobuf", "application/json", []byte(`{}`), http.StatusBadRequest},
		{"garbage", "application/x-protobuf", []byte{0xff, 0xff, 0xff}, http.StatusBadRequest},
	}
	for _, tt := range tests {
		resp, err := http.Post(c.Endpoint()+"/v1/traces", tt.contentType, bytes.NewReader(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tt.wantStatus {
			t.Errorf("%s: status %d, want %d", tt.name, resp.StatusCode, tt.wantStatus)
		}
	}
	if c.Spans() != 9 || c.Exports() != 2 || c.Rejected() != 2 {
		t.Errorf("spans %d, exports %d, rejected %d; want 9, 2, 2", c.Spans(), c.Exports(), c.Rejected())
	}
}
