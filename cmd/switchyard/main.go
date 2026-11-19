// Command switchyard is the Switchyard LLM gateway.
//
// Usage:
//
//	switchyard serve [-c FILE] [--log-level LEVEL]
//	switchyard config validate [-c FILE] [--log-level LEVEL]
//	switchyard catalog lint [-c FILE]
//	switchyard route explain [-c FILE] --tenant ID [--key ID] [--header 'Name: value']... [FILE|-]
//	switchyard keys generate [--tenant ID] [--key-id ID]
//	switchyard eval run|fit|report|fit-weights [flags]
//	switchyard healthcheck [--url URL] [--timeout DURATION]
//	switchyard version
//	switchyard help
//
// The config file is the -c/--config flag, else $SWITCHYARD_CONFIG, else
// ./switchyard.yaml. SWITCHYARD_LISTEN, SWITCHYARD_ADMIN_LISTEN and
// SWITCHYARD_LOG_LEVEL override the file, and --log-level overrides them all.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"syscall"
	"time"

	"switchyard/internal/app"
	"switchyard/internal/cache/semantic"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/eval"
	"switchyard/internal/version"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// defaultConfigPath is used when neither the flag nor SWITCHYARD_CONFIG is set.
const defaultConfigPath = "switchyard.yaml"

const usage = `Switchyard is a cost-aware, OpenAI-compatible LLM gateway.

Usage:
  switchyard serve [flags]             run the gateway
  switchyard config validate [flags]   check a config file and exit
  switchyard catalog lint [-c FILE]    check catalog prices and sources; exit 1 on warnings
  switchyard route explain [flags]     explain how a chat request would be routed, offline
  switchyard keys generate [flags]     create a gateway API key
  switchyard eval SUBCOMMAND [flags]   measure model quality: run, fit, report, fit-weights (see eval help)
  switchyard healthcheck [flags]       GET a health URL; exit 0 on 200, 1 otherwise (container healthchecks)
  switchyard version                   print the build metadata
  switchyard help                      print this help

Flags for serve and config validate:
  -c, --config FILE    config file (default $SWITCHYARD_CONFIG, else ./switchyard.yaml)
  --log-level LEVEL    debug, info, warn or error; overrides the config file

Flags for route explain (the request body is FILE, or stdin for - or none):
  -c, --config FILE        config file, as for serve
  --tenant ID              tenant whose policy applies (required)
  --key ID                 the tenant's API key, for key policies and rules
  --header 'Name: value'   a request header, such as X-Switchyard-Profile; repeatable

Flags for healthcheck:
  --url URL            URL to GET (default http://127.0.0.1:8080/healthz)
  --timeout DURATION   overall timeout (default 2s)

Flags for keys generate:
  --tenant ID          tenant that owns the key (default "default")
  --key-id ID          key id (default "key-" and 6 hex characters of its hash)

Environment:
  SWITCHYARD_CONFIG        config file, when -c is not given
  SWITCHYARD_LISTEN        overrides server.listen
  SWITCHYARD_ADMIN_LISTEN  overrides server.admin_listen
  SWITCHYARD_LOG_LEVEL     overrides logging.level
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv))
}

// run executes the command line args and returns the process exit code.
func run(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		return runServe(rest, stdout, stderr, lookupEnv)
	case "config":
		if len(rest) == 0 || rest[0] != "validate" {
			if len(rest) == 0 {
				fmt.Fprintln(stderr, "error: config: missing subcommand")
			} else {
				fmt.Fprintf(stderr, "error: config: unknown subcommand %q\n", rest[0])
			}
			fmt.Fprint(stderr, usage)
			return exitUsage
		}
		return runValidate(rest[1:], stdout, stderr, lookupEnv)
	case "catalog":
		return runCatalog(rest, stdout, stderr, lookupEnv, time.Now)
	case "route":
		return runRoute(rest, os.Stdin, stdout, stderr, lookupEnv)
	case "keys":
		return runKeys(rest, stdout, stderr)
	case "eval":
		return runEval(rest, stdout, stderr, lookupEnv)
	case "healthcheck":
		return runHealthcheck(rest, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, version.Get().String())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n", cmd)
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
}

// loadFlags are the flags shared by serve and config validate.
type loadFlags struct {
	config   string
	logLevel string
	// secretPlaceholders loads without the config's secrets (see
	// config.LoadOptions.SecretPlaceholders), for commands that never
	// call an upstream or a store.
	secretPlaceholders bool
}

// parseLoadFlags parses args for the named subcommand. When it returns
// ok == false, the caller exits with code.
func parseLoadFlags(name string, args []string, stderr io.Writer) (f loadFlags, code int, ok bool) {
	fset := flag.NewFlagSet(name, flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.StringVar(&f.config, "c", "", "config `file`")
	fset.StringVar(&f.config, "config", "", "config `file` (same as -c)")
	fset.StringVar(&f.logLevel, "log-level", "", "log `level`: debug, info, warn or error")
	fset.Usage = func() {
		fmt.Fprintf(stderr, "Usage: switchyard %s [-c FILE] [--log-level LEVEL]\n", name)
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return f, exitOK, false
		}
		return f, exitUsage, false
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(stderr, "error: %s: unexpected argument %q\n", name, fset.Arg(0))
		fset.Usage()
		return f, exitUsage, false
	}
	return f, exitOK, true
}

// load resolves the config path and overrides and loads the snapshot. On
// failure it prints the problems to stderr. It returns the path it used.
func load(f loadFlags, stderr io.Writer, lookupEnv func(string) (string, bool)) (*config.Snapshot, string, bool) {
	snap, path, err := loadSnapshot(f, lookupEnv)
	if err == nil {
		return snap, path, true
	}
	var verr *config.ValidationError
	switch {
	case errors.As(err, &verr):
		// Problems name settings and references, never expanded values, so
		// they are safe to print.
		fmt.Fprintln(stderr, verr.Error())
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stderr, "error: %s: no such file\n", path)
	default:
		fmt.Fprintf(stderr, "error: %v\n", err)
	}
	return nil, path, false
}

// loadSnapshot resolves the config path and overrides, as load does, and
// loads the snapshot. A reload calls it again with the same flags.
func loadSnapshot(f loadFlags, lookupEnv func(string) (string, bool)) (*config.Snapshot, string, error) {
	path := f.config
	if path == "" {
		if v, ok := lookupEnv("SWITCHYARD_CONFIG"); ok && v != "" {
			path = v
		} else {
			path = defaultConfigPath
		}
	}

	// The operational environment overrides (DESIGN §16.1) and their
	// config.LoadOptions.Overrides keys.
	envOverrides := [...]struct{ env, key string }{
		{"SWITCHYARD_LISTEN", "server.listen"},
		{"SWITCHYARD_ADMIN_LISTEN", "server.admin_listen"},
		{"SWITCHYARD_LOG_LEVEL", "logging.level"},
	}
	overrides := make(map[string]string)
	for _, o := range envOverrides {
		if v, ok := lookupEnv(o.env); ok && v != "" {
			overrides[o.key] = v
		}
	}
	if f.logLevel != "" {
		overrides["logging.level"] = f.logLevel
	}

	snap, err := config.Load(path, config.LoadOptions{LookupEnv: lookupEnv, Overrides: overrides, SecretPlaceholders: f.secretPlaceholders})
	if errors.Is(err, fs.ErrNotExist) {
		err = fmt.Errorf("%s: no such file: %w", path, err)
	}
	return snap, path, err
}

// runValidate implements `switchyard config validate`.
func runValidate(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	f, code, ok := parseLoadFlags("config validate", args, stderr)
	if !ok {
		return code
	}
	snap, path, ok := load(f, stderr, lookupEnv)
	if !ok {
		return exitError
	}
	cfg := snap.Config()
	// The checks that need the catalog: the deployment shadow sampling
	// pins the judge to must pass every sampling policy, and the one the
	// semantic cache embeds with every policy that enables it.
	cat, err := catalog.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	if _, err := eval.ShadowJudge(cat, cfg); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", path, err)
		return exitError
	}
	if semantic.Enabled(cfg) {
		adapters, err := eval.Adapters(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitError
		}
		if _, _, err := semantic.ConfiguredDeployment(cfg, cat, adapters); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", path, err)
			return exitError
		}
	}
	warnings := snap.Warnings()
	for _, w := range warnings {
		fmt.Fprintln(stdout, w.String())
	}
	fmt.Fprintf(stdout, "%s: OK (config version %s; %s, %s; %s)\n", path, snap.Version(),
		plural(len(cfg.Providers), "provider"), plural(len(cfg.Models), "model"), plural(len(warnings), "warning"))
	return exitOK
}

// runServe implements `switchyard serve`. The first SIGINT or SIGTERM starts
// a graceful shutdown; a second one aborts the requests still in flight.
// SIGHUP reloads the configuration, from the same file, environment and
// flags, and never stops the process.
func runServe(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	f, code, ok := parseLoadFlags("serve", args, stderr)
	if !ok {
		return code
	}
	// Catch SIGHUP before anything else, so that one sent during startup
	// does not end the process, as its default action would; it is
	// handled once the service runs.
	hups := make(chan os.Signal, 1)
	signal.Notify(hups, syscall.SIGHUP)
	defer signal.Stop(hups)
	snap, _, ok := load(f, stderr, lookupEnv)
	if !ok {
		return exitError
	}
	reload := func() (*config.Snapshot, error) {
		snap, _, err := loadSnapshot(f, lookupEnv)
		return snap, err
	}
	a, err := app.New(snap, stdout, reload)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitError
	}
	log := a.Logger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	forceCtx, force := context.WithCancel(context.Background())
	defer force()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-hups:
				log.Info("signal received; reloading the configuration", "signal", syscall.SIGHUP.String())
				a.ReloadOnSignal()
			case <-done:
				return
			}
		}
	}()
	go func() {
		select {
		case sig := <-sigs:
			log.Info("signal received; shutting down", "signal", sig.String())
			cancel()
		case <-done:
			return
		}
		select {
		case sig := <-sigs:
			log.Warn("second signal received; aborting in-flight requests", "signal", sig.String())
			force()
		case <-done:
		}
	}()

	if err := a.Run(ctx, forceCtx); err != nil {
		log.Error("switchyard stopped with an error", "error", err)
		return exitError
	}
	return exitOK
}

// plural formats n with noun, adding "s" unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
