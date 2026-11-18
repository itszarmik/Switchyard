package semantic

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/redis/go-redis/v9"

	"switchyard/internal/cache"
)

func TestIndexName(t *testing.T) {
	tests := []struct {
		model string
		dims  int
		want  string
	}{
		{"text-embedding-3-small@openai", 1536, "sy:s:v1:text-embedding-3-small_aopenai:1536"},
		{"BAAI/bge-small-en-v1.5@tei_local", 384, "sy:s:v1:BAAI_sbge-small-en-v1.5_atei__local:384"},
		{"nomic:latest@ollama", 768, "sy:s:v1:nomic_clatest_aollama:768"},
		{"a b*", 2, "sy:s:v1:a_x20b_x2a:2"},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			ix := NewRedisIndex(nil, tc.model, tc.dims)
			if ix.Name() != tc.want || ix.prefix != tc.want+":" {
				t.Errorf("name %q prefix %q, want %q", ix.Name(), ix.prefix, tc.want)
			}
		})
	}
}

// slugChars are the characters a slug may contain.
const slugChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

// TestSlugInjective checks that model IDs differing only in characters the
// slug escapes never share an index.
func TestSlugInjective(t *testing.T) {
	ids := []string{
		"a_b", "a__b", "a_cb", "a:b", "a/b", "a@b", "a_sb", "a_ab", "a_x3ab", "a.b", "a-b",
		"A@b", "a@B", "m@p_q", "m_ap@q", "m@p", "m_a", "m_ap",
	}
	seen := map[string]string{}
	for _, id := range ids {
		s := slug(id)
		if prev, ok := seen[s]; ok {
			t.Errorf("slug(%q) = slug(%q) = %q", id, prev, s)
		}
		seen[s] = id
		for _, c := range s {
			if !strings.ContainsRune(slugChars, c) {
				t.Errorf("slug(%q) = %q has unsafe %q", id, s, c)
			}
		}
	}
}

func TestEscapeTag(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0123abcdef", "0123abcdef"},
		{"a_b", "a_b"},
		{"a-b.c", `a\-b\.c`},
		{`{x}|y\`, `\{x\}\|y\\`},
	}
	for _, tc := range tests {
		if got := escapeTag(tc.in); got != tc.want {
			t.Errorf("escapeTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCheckPartition(t *testing.T) {
	tests := []struct {
		p  string
		ok bool
	}{
		{strings.Repeat("ab", 32), true},
		{`a{b}|c-d.e:f\g`, true},
		{"", false},
		{strings.Repeat("a", maxPartitionBytes+1), false},
		{"a,b", false},
		{"a b", false},
		{"a\tb", false},
		{"é", false},
	}
	for _, tc := range tests {
		if err := checkPartition(tc.p); (err == nil) != tc.ok {
			t.Errorf("checkPartition(%q) = %v, want ok %v", tc.p, err, tc.ok)
		}
	}
}

func TestFirstResult(t *testing.T) {
	tests := []struct {
		name   string
		reply  any
		want   map[string]string
		wantOK bool
		bad    bool
	}{
		{name: "resp2 none", reply: []any{int64(0)}},
		{
			name:  "resp2 one",
			reply: []any{int64(1), "doc", []any{"e", "body", "d", "0.25"}},
			want:  map[string]string{"e": "body", "d": "0.25"}, wantOK: true,
		},
		{name: "resp2 expired", reply: []any{int64(1), "doc", nil}},
		{name: "resp2 bad field", reply: []any{int64(1), "doc", []any{"e", int64(3)}}, bad: true},
		{name: "resp3 none", reply: map[any]any{"total_results": int64(0), "results": []any{}}},
		{
			name: "resp3 one",
			reply: map[any]any{"total_results": int64(1), "results": []any{
				map[any]any{"id": "doc", "extra_attributes": map[any]any{"e": "body", "d": "0.25"}, "values": []any{}},
			}},
			want: map[string]string{"e": "body", "d": "0.25"}, wantOK: true,
		},
		{name: "resp3 expired", reply: map[any]any{"results": []any{map[any]any{"id": "doc"}}}},
		{name: "resp3 no results", reply: map[any]any{"total_results": int64(0)}, bad: true},
		{name: "other", reply: "OK", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := firstResult(tc.reply)
			if (err != nil) != tc.bad || ok != tc.wantOK {
				t.Fatalf("firstResult = %v, %v, %v; want ok %v, error %v", got, ok, err, tc.wantOK, tc.bad)
			}
			if diff := cmp.Diff(tc.want, got); tc.wantOK && diff != "" {
				t.Errorf("fields (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRedisIndexWithoutQueryEngine checks that a Redis without the query
// engine (miniredis has no FT.* commands) fails every call with an error,
// which the cache treats as a miss, and that invalid input fails before
// any Redis call.
func TestRedisIndexWithoutQueryEngine(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })
	ix := NewRedisIndex(client, "m@p", 2)
	ctx := context.Background()
	if _, err := ix.Search(ctx, "p", []float32{1, 0}); err == nil {
		t.Error("Search without the query engine succeeded")
	}
	if err := ix.Add(ctx, "p", []float32{1, 0}, &cache.Entry{}, time.Hour); err == nil {
		t.Error("Add without the query engine succeeded")
	}
	tests := []struct {
		name string
		call func() error
	}{
		{"search dims", func() error { _, err := ix.Search(ctx, "p", []float32{1}); return err }},
		{"search zero", func() error { _, err := ix.Search(ctx, "p", []float32{0, 0}); return err }},
		{"search partition", func() error { _, err := ix.Search(ctx, "", []float32{1, 0}); return err }},
		{"add nil entry", func() error { return ix.Add(ctx, "p", []float32{1, 0}, nil, time.Hour) }},
		{"add ttl", func() error { return ix.Add(ctx, "p", []float32{1, 0}, &cache.Entry{}, time.Microsecond) }},
		{"add partition", func() error { return ix.Add(ctx, "a,b", []float32{1, 0}, &cache.Entry{}, time.Hour) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := mr.CommandCount()
			if err := tc.call(); err == nil {
				t.Error("succeeded")
			}
			if n := mr.CommandCount() - before; n != 0 {
				t.Errorf("%d Redis commands sent for invalid input", n)
			}
		})
	}
}

// TestRedisIndexHungRedis checks that a Redis that accepts connections but
// never answers costs a lookup no more than its context's deadline.
func TestRedisIndexHungRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		// Hold every connection open, unanswered, until the listener
		// closes.
		var conns []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range conns {
					_ = c.Close()
				}
				return
			}
			conns = append(conns, conn)
		}
	}()
	client := redis.NewClient(&redis.Options{
		Addr: ln.Addr().String(), ContextTimeoutEnabled: true, MaxRetries: -1,
		// Without a deadline of its own, a read would wait for these.
		ReadTimeout: time.Hour, WriteTimeout: time.Hour,
	})
	t.Cleanup(func() { _ = client.Close() })
	ix := NewRedisIndex(client, "m@p", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = ix.Search(ctx, "p", []float32{1, 0})
	if err == nil {
		t.Fatal("Search against a hung Redis succeeded")
	}
	var nerr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) || errors.As(err, &nerr) && nerr.Timeout()
	if !timedOut {
		t.Errorf("Search error %v, want a deadline", err)
	}
}
