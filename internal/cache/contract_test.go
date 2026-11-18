package cache

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// created is a fixed entry creation time, with nanoseconds, in UTC.
var created = time.Date(2126, 11, 17, 9, 30, 0, 123456789, time.UTC)

// testStoreContract checks the behavior every Store shares. newStore
// returns an empty store; keys are unique per call, so implementations
// over a shared server need no cleanup between subtests.
func testStoreContract(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()
	entry := &Entry{
		Body:       []byte(`{"id":"c1","object":"chat.completion","choices":[]}`),
		Deployment: "gpt-4o@openai", Model: "gpt-4o", Created: created,
	}

	t.Run("miss", func(t *testing.T) {
		e, err := newStore(t).Get(ctx, "sy:x:v1:t:a:0:absent")
		if e != nil || err != nil {
			t.Errorf("Get(absent) = %v, %v; want nil, nil", e, err)
		}
	})

	t.Run("set then get", func(t *testing.T) {
		s := newStore(t)
		if err := s.Set(ctx, "k", entry, time.Hour); err != nil {
			t.Fatalf("Set: %v", err)
		}
		got, err := s.Get(ctx, "k")
		if err != nil || got == nil {
			t.Fatalf("Get = %v, %v", got, err)
		}
		if !bytes.Equal(got.Body, entry.Body) || got.Deployment != entry.Deployment || got.Model != entry.Model || !got.Created.Equal(entry.Created) {
			t.Errorf("Get = %+v, want %+v", got, entry)
		}
	})

	t.Run("replace", func(t *testing.T) {
		s := newStore(t)
		_ = s.Set(ctx, "k", entry, time.Hour)
		next := &Entry{Body: []byte("second"), Deployment: "d2", Model: "m2"}
		if err := s.Set(ctx, "k", next, time.Hour); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "k")
		if err != nil || got == nil || string(got.Body) != "second" || got.Deployment != "d2" || !got.Created.IsZero() {
			t.Errorf("Get after replace = %+v, %v", got, err)
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		s := newStore(t)
		_ = s.Set(ctx, "sy:x:v1:t:a:0:h", entry, time.Hour)
		for _, k := range []string{"sy:x:v1:t:b:0:h", "sy:x:v1:t:a:1:h", "sy:x:v1:t:a:0:"} {
			if e, err := s.Get(ctx, k); e != nil || err != nil {
				t.Errorf("Get(%q) = %v, %v; want a miss", k, e, err)
			}
		}
	})

	t.Run("binary body", func(t *testing.T) {
		s := newStore(t)
		body := make([]byte, 256*1024)
		for i := range body {
			body[i] = byte(i * 7)
		}
		if err := s.Set(ctx, "k", &Entry{Body: body}, time.Hour); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, "k")
		if err != nil || got == nil || !bytes.Equal(got.Body, body) {
			t.Errorf("binary body did not round-trip: %v", err)
		}
	})

	t.Run("caller may reuse its buffer", func(t *testing.T) {
		s := newStore(t)
		buf := []byte("original")
		_ = s.Set(ctx, "k", &Entry{Body: buf}, time.Hour)
		copy(buf, "XXXXXXXX")
		got, _ := s.Get(ctx, "k")
		if got == nil || string(got.Body) != "original" {
			t.Errorf("stored body changed with the caller's buffer: %v", got)
		}
	})

	t.Run("ttl must be positive", func(t *testing.T) {
		s := newStore(t)
		for _, ttl := range []time.Duration{0, -time.Second} {
			if err := s.Set(ctx, "k", entry, ttl); err == nil {
				t.Errorf("Set(ttl %s) succeeded", ttl)
			}
		}
	})

	t.Run("concurrent use", func(t *testing.T) {
		s := newStore(t)
		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				k := fmt.Sprintf("k%d", i%4)
				_ = s.Set(ctx, k, entry, time.Hour)
				if _, err := s.Get(ctx, k); err != nil {
					t.Errorf("Get: %v", err)
				}
			})
		}
		wg.Wait()
	})
}

// testNamespacesContract checks the behavior every Namespaces shares.
// newNamespaces returns a value where the tenants named in the test have
// never been bumped.
func testNamespacesContract(t *testing.T, newNamespaces func(t *testing.T) Namespaces) {
	ctx := context.Background()

	t.Run("version starts at 0", func(t *testing.T) {
		v, err := newNamespaces(t).Version(ctx, "acme")
		if v != 0 || err != nil {
			t.Errorf("Version = %d, %v; want 0, nil", v, err)
		}
	})

	t.Run("bump increments one tenant only", func(t *testing.T) {
		n := newNamespaces(t)
		for want := int64(1); want <= 3; want++ {
			v, err := n.Bump(ctx, "acme")
			if v != want || err != nil {
				t.Fatalf("Bump = %d, %v; want %d", v, err, want)
			}
		}
		if v, err := n.Version(ctx, "acme"); v != 3 || err != nil {
			t.Errorf("Version(acme) = %d, %v; want 3", v, err)
		}
		if v, err := n.Version(ctx, "globex"); v != 0 || err != nil {
			t.Errorf("Version(globex) = %d, %v; want 0", v, err)
		}
	})

	t.Run("concurrent bumps are not lost", func(t *testing.T) {
		n := newNamespaces(t)
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if _, err := n.Bump(ctx, "acme"); err != nil {
					t.Errorf("Bump: %v", err)
				}
			})
		}
		wg.Wait()
		if v, _ := n.Version(ctx, "acme"); v != 20 {
			t.Errorf("Version after 20 bumps = %d", v)
		}
	})
}
