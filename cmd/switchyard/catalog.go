package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"switchyard/internal/catalog"
)

// runCatalog implements `switchyard catalog SUBCOMMAND`. now is the clock
// lint compares pricing.checked dates against.
func runCatalog(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), now func() time.Time) int {
	if len(args) == 0 || args[0] != "lint" {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "error: catalog: missing subcommand")
		} else {
			fmt.Fprintf(stderr, "error: catalog: unknown subcommand %q\n", args[0])
		}
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	return runCatalogLint(args[1:], stdout, stderr, lookupEnv, now)
}

// runCatalogLint implements `switchyard catalog lint`. It loads the config
// as `config validate` does, then prints every catalog finding and a
// summary. It exits 1 if the config is invalid or there is any warning;
// notices alone do not fail it.
func runCatalogLint(args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), now func() time.Time) int {
	var f loadFlags
	fset := flag.NewFlagSet("catalog lint", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.StringVar(&f.config, "c", "", "config `file`")
	fset.StringVar(&f.config, "config", "", "config `file` (same as -c)")
	fset.Usage = func() {
		fmt.Fprintln(stderr, "Usage: switchyard catalog lint [-c FILE]")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(stderr, "error: catalog lint: unexpected argument %q\n", fset.Arg(0))
		fset.Usage()
		return exitUsage
	}

	snap, path, ok := load(f, stderr, lookupEnv)
	if !ok {
		return exitError
	}
	findings := catalog.Lint(snap, now())
	var warnings, notices int
	for _, fd := range findings {
		fmt.Fprintln(stdout, fd.String())
		if fd.Level == catalog.LevelNotice {
			notices++
		} else {
			warnings++
		}
	}
	status := "OK"
	if warnings > 0 {
		status = "FAILED"
	}
	fmt.Fprintf(stdout, "%s: catalog lint %s (%s, %s; %s, %s)\n", path, status,
		plural(len(snap.Config().Models), "model"), aliasCount(len(snap.Config().Aliases)),
		plural(warnings, "warning"), plural(notices, "notice"))
	if warnings > 0 {
		return exitError
	}
	return exitOK
}

// aliasCount formats a number of aliases.
func aliasCount(n int) string {
	if n == 1 {
		return "1 alias"
	}
	return fmt.Sprintf("%d aliases", n)
}
