package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"switchyard/internal/auth"
	"switchyard/internal/config"
)

// defaultKeyTenant is the tenant id keys generate uses without --tenant.
const defaultKeyTenant = "default"

// idRule describes a valid tenant or key id. An invalid value is not echoed,
// in case a key was pasted into the wrong flag.
const idRule = "is invalid: use 1 to 63 characters from a-z, 0-9, _ and -, starting with a letter or digit"

// keysUsage is printed for keys usage errors and keys generate --help.
const keysUsage = `Usage: switchyard keys generate [--tenant ID] [--key-id ID]

Creates a gateway API key, prints it once, and prints the config snippet
that stores its SHA-256 hash.

Flags:
  --tenant ID    tenant that owns the key (default "default")
  --key-id ID    id of the key in logs and reports (default "key-" and the
                 first 6 hex characters of the key's hash)
`

// runKeys implements `switchyard keys`.
func runKeys(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		fmt.Fprintln(stderr, "error: keys: missing subcommand")
	case args[0] != "generate":
		fmt.Fprintf(stderr, "error: keys: unknown subcommand %q\n", args[0])
	default:
		return runKeysGenerate(args[1:], stdout, stderr)
	}
	fmt.Fprint(stderr, keysUsage)
	return exitUsage
}

// runKeysGenerate implements `switchyard keys generate`. It writes the key
// and the snippet to stdout as plain text, and nothing else: the key must
// not end up anywhere but in front of the operator.
func runKeysGenerate(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("keys generate", flag.ContinueOnError)
	fset.SetOutput(stderr)
	tenant := fset.String("tenant", defaultKeyTenant, "tenant `ID`")
	keyID := fset.String("key-id", "", "key `ID`")
	fset.Usage = func() { fmt.Fprint(stderr, keysUsage) }
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fset.NArg() > 0 {
		fmt.Fprintln(stderr, "error: keys generate: takes no arguments, only flags")
		fset.Usage()
		return exitUsage
	}
	if !config.ValidID(*tenant) {
		fmt.Fprintln(stderr, "error: keys generate: --tenant "+idRule)
		return exitUsage
	}
	if *keyID != "" && !config.ValidID(*keyID) {
		fmt.Fprintln(stderr, "error: keys generate: --key-id "+idRule)
		return exitUsage
	}

	key, hash, err := auth.GenerateKey()
	if err != nil {
		fmt.Fprintf(stderr, "error: keys generate: %v\n", err)
		return exitError
	}
	if *keyID == "" {
		*keyID = "key-" + strings.TrimPrefix(hash, "sha256:")[:6]
	}

	fmt.Fprintf(stdout, `New API key for tenant %[1]s, key id %[2]s:

    %[3]s

Store it now; it cannot be shown again. Switchyard keeps only its hash.
Clients send it as "Authorization: Bearer <key>".

Add the key to the tenants section of your config. If tenant %[1]s
already exists, add only the entry under its keys:

tenants:
  - id: %[1]s
    keys:
      - id: %[2]s
        hash: %[4]s
`, *tenant, *keyID, key, hash)
	return exitOK
}
