// Package config loads, validates and snapshots the Switchyard configuration.
//
// Load reads the root YAML file, merges the files named by its include globs,
// expands ${VAR}, ${VAR:-default} and ${file:PATH} references at the YAML
// node level, rejects unknown fields, applies defaults and operational
// overrides, and validates the result. Every problem it can find is collected
// and returned together in a *ValidationError, each with its file, line and
// config path, so an operator can fix a broken config in one pass.
//
// A successful load yields an immutable *Snapshot whose Version is a short
// content hash of the effective configuration.
//
// Secret values (API keys, DSNs, URLs that may embed passwords) are held in
// the Secret type, which never reveals its contents when printed, logged or
// serialized. Problems never echo a value that came from a reference, and
// never echo a secret at all.
package config
