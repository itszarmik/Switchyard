package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/config"
	"switchyard/internal/gateway"
)

// maxExplainBody bounds the request a route explanation reads, as the
// gateway's default body limit does.
const maxExplainBody = 10 << 20

// headerFlags collects repeated --header 'Name: value' flags.
type headerFlags struct{ h http.Header }

func (f *headerFlags) String() string { return "" }

// Set parses one 'Name: value' header.
func (f *headerFlags) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" || strings.ContainsAny(name, " \t") {
		return errors.New("want 'Name: value'")
	}
	f.h.Add(name, strings.TrimSpace(value))
	return nil
}

// runRoute implements `switchyard route SUBCOMMAND`.
func runRoute(args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	if len(args) == 0 || args[0] != "explain" {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "error: route: missing subcommand")
		} else {
			fmt.Fprintf(stderr, "error: route: unknown subcommand %q\n", args[0])
		}
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	return runRouteExplain(args[1:], stdin, stdout, stderr, lookupEnv)
}

// runRouteExplain implements `switchyard route explain`: it reads a chat
// completions request body from FILE, or from stdin for "-" or no FILE,
// and prints the routing explanation the explain endpoint returns for it,
// offline: every deployment counts as healthy, latency estimates come from
// the catalog's hints, and every budget is untouched, with its whole limit
// left and no soft profile in force, as on a fresh gateway. The config is
// loaded without its secrets (API keys, Redis URLs, the ledger DSN), which
// it never uses. A request the gateway would reject prints the error and
// exits 1.
func runRouteExplain(args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	f := loadFlags{secretPlaceholders: true}
	var tenant, key string
	headers := &headerFlags{h: http.Header{}}
	fset := flag.NewFlagSet("route explain", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.StringVar(&f.config, "c", "", "config `file`")
	fset.StringVar(&f.config, "config", "", "config `file` (same as -c)")
	fset.StringVar(&tenant, "tenant", "", "tenant `id` whose policy applies (required)")
	fset.StringVar(&key, "key", "", "API key `id` of the tenant, for key policies, rules and budgets")
	fset.Var(headers, "header", "request header 'Name: value', repeatable")
	fset.Usage = func() {
		fmt.Fprintln(stderr, "Usage: switchyard route explain -c FILE --tenant ID [--key ID] [--header 'Name: value']... [REQUEST.json|-]")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if tenant == "" {
		fmt.Fprintln(stderr, "error: route explain: --tenant is required")
		fset.Usage()
		return exitUsage
	}
	if fset.NArg() > 1 {
		fmt.Fprintf(stderr, "error: route explain: unexpected argument %q\n", fset.Arg(1))
		fset.Usage()
		return exitUsage
	}

	snap, _, ok := load(f, stderr, lookupEnv)
	if !ok {
		return exitError
	}
	cfg := snap.Config()
	i := slices.IndexFunc(cfg.Tenants, func(t config.Tenant) bool { return t.ID == tenant })
	if i < 0 {
		fmt.Fprintf(stderr, "error: route explain: unknown tenant %q\n", tenant)
		return exitError
	}
	if key != "" && !slices.ContainsFunc(cfg.Tenants[i].Keys, func(k config.APIKey) bool { return k.ID == key }) {
		fmt.Fprintf(stderr, "error: route explain: tenant %q has no key %q\n", tenant, key)
		return exitError
	}

	body, err := readRequest(fset.Arg(0), stdin)
	if err != nil {
		fmt.Fprintf(stderr, "error: route explain: %v\n", err)
		return exitError
	}
	gw, err := gateway.New(gateway.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), Registry: prometheus.NewRegistry(),
		ConfigVersion: snap.Version(), Offline: true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: route explain: %v\n", err)
		return exitError
	}
	out, gerr := gw.ExplainRequest(context.Background(), tenant, key, headers.h, body)
	if gerr != nil {
		fmt.Fprintf(stderr, "error: %d %s: %s\n", gerr.Status, gerr.Code, gerr.Message)
		return exitError
	}
	if _, err := stdout.Write(out); err != nil {
		return exitError
	}
	return exitOK
}

// readRequest reads the request body from path, or from stdin when path
// is "" or "-", up to maxExplainBody bytes.
func readRequest(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "" && path != "-" {
		f, err := os.Open(path) // #nosec G304 -- the operator names the file
		if err != nil {
			return nil, fmt.Errorf("read the request: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxExplainBody+1))
	if err != nil {
		return nil, fmt.Errorf("read the request: %w", err)
	}
	if len(data) > maxExplainBody {
		return nil, fmt.Errorf("the request is larger than %d bytes", maxExplainBody)
	}
	return data, nil
}
