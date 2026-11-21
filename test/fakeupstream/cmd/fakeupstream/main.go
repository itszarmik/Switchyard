// Command fakeupstream serves the fake OpenAI-wire upstream of package
// fakeupstream as a standalone process, for tests that run the gateway
// somewhere a Go test cannot start an in-process fake: the Helm chart's
// kind test runs it as a Deployment (test/helm/Dockerfile.fakeupstream).
//
//	fakeupstream -listen :8080 -slow-listen :8081 -slow-chunk-delay 4s
//
// It serves two fakes. The one on -listen answers at once. The one on
// -slow-listen pauses -slow-chunk-delay before every stream event after
// the first, so a stream of the canned answer (11 events) lasts about ten
// delays: a long stream to keep in flight across a rolling upgrade. Point
// two gateway providers at them. Non-streaming answers are immediate on
// both.
//
// Both require "Authorization: Bearer <key>" when -api-key, or the
// FAKEUPSTREAM_API_KEY environment variable, is set; the variable keeps
// the key out of the process's arguments. Requests are not recorded, so
// memory stays flat however long it runs. SIGTERM or SIGINT stops it,
// releasing any request still in flight.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"switchyard/test/fakeupstream"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakeupstream:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("fakeupstream", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "address of the immediate fake")
	slowListen := fs.String("slow-listen", ":8081", `address of the slow-streaming fake; "" disables it`)
	chunkDelay := fs.Duration("slow-chunk-delay", 4*time.Second, "pause between the slow fake's stream events")
	apiKey := fs.String("api-key", os.Getenv("FAKEUPSTREAM_API_KEY"), "required bearer key (default $FAKEUPSTREAM_API_KEY; empty accepts any)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if *chunkDelay < 0 {
		return errors.New("-slow-chunk-delay must not be negative")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	fast, err := serve(ctx, *listen, *apiKey, fakeupstream.Behavior{})
	if err != nil {
		return err
	}
	defer fast.Close()
	log.Info("serving", "fake", "immediate", "url", fast.URL(), "auth", *apiKey != "")
	if *slowListen != "" {
		slow, err := serve(ctx, *slowListen, *apiKey, fakeupstream.Behavior{ChunkDelay: *chunkDelay})
		if err != nil {
			return err
		}
		defer slow.Close()
		log.Info("serving", "fake", "slow", "url", slow.URL(), "chunk_delay", chunkDelay.String())
	}

	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

// serve starts a fake on addr that answers every request with b.
func serve(ctx context.Context, addr, apiKey string, b fakeupstream.Behavior) (*fakeupstream.Server, error) {
	l, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	s := fakeupstream.New(fakeupstream.Options{APIKey: apiKey, DisableRecording: true, Listener: l})
	s.Script(b)
	return s, nil
}
