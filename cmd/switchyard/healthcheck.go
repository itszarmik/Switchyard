package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Defaults of switchyard healthcheck: the image's public listener.
const (
	defaultHealthURL     = "http://127.0.0.1:8080/healthz"
	defaultHealthTimeout = 2 * time.Second
	// maxHealthBody bounds how much of the answer is read, so the
	// connection can be reused; the body is otherwise ignored.
	maxHealthBody = 4 << 10
)

// runHealthcheck implements `switchyard healthcheck`: one GET of --url,
// within --timeout, for container healthchecks (the distroless image has
// no shell or curl). It exits 0, printing nothing, when the answer is
// 200, and 1 otherwise, with the reason on stderr; bad flags exit 1 too,
// never 2, which Docker reserves for healthchecks. Redirects are not
// followed and proxy settings are ignored: the probe is local.
func runHealthcheck(args []string, stderr io.Writer) int {
	fset := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fset.SetOutput(stderr)
	rawURL := fset.String("url", defaultHealthURL, "`URL` to GET; 200 is healthy")
	timeout := fset.Duration("timeout", defaultHealthTimeout, "overall `timeout`")
	fset.Usage = func() {
		fmt.Fprintln(stderr, "Usage: switchyard healthcheck [--url URL] [--timeout DURATION]")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(stderr, "error: healthcheck: unexpected argument %q\n", fset.Arg(0))
		return exitError
	}
	u, err := url.Parse(*rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintln(stderr, "error: healthcheck: --url must be an http or https URL")
		return exitError
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "error: healthcheck: --timeout must be positive")
		return exitError
	}
	if err := probe(u.String(), *timeout); err != nil {
		fmt.Fprintf(stderr, "switchyard healthcheck: %v\n", err)
		return exitError
	}
	return exitOK
}

// probe GETs target and returns nil when the answer is 200.
func probe(target string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	req.Header.Set("User-Agent", "switchyard-healthcheck")
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", req.URL.Redacted(), err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHealthBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d, want 200", req.URL.Redacted(), resp.StatusCode)
	}
	return nil
}
