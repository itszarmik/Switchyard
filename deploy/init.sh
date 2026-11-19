#!/bin/sh
# Writes deploy/.env for deploy/compose.yaml: a generated admin token, the
# first gateway API key (only its hash is given to the gateway), and the
# provider keys found in the environment. Run it once, before the first
# `docker compose up`:
#
#   export OPENAI_API_KEY=...      # or ANTHROPIC_API_KEY (or both)
#   ./init.sh
#
# The gateway API key is printed once and kept in .env as SWITCHYARD_API_KEY
# for the quickstart's curl commands; the gateway never reads that line.
# .env holds secrets: it is created with mode 0600 and is gitignored.
#
# It generates keys with `switchyard keys generate`, run from the image
# (`docker compose run`, which builds the image if needed), or from
# SWITCHYARD_BIN when that names a switchyard binary, as `make compose-up`
# does. An existing .env is never overwritten; pass --force to replace it,
# which invalidates the old gateway key and admin token.
set -eu

cd "$(dirname "$0")"
env_file=.env

force=0
for arg in "$@"; do
	case "$arg" in
	--force) force=1 ;;
	-h | --help)
		sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "init.sh: unknown argument $arg (try --help)" >&2
		exit 2
		;;
	esac
done

if [ -e "$env_file" ] && [ "$force" -eq 0 ]; then
	echo "init.sh: $PWD/$env_file already exists; keeping it (pass --force to replace it)."
	exit 0
fi

# keys_generate runs `switchyard keys generate` with the given flags.
keys_generate() {
	if [ -n "${SWITCHYARD_BIN:-}" ]; then
		"$SWITCHYARD_BIN" keys generate "$@"
		return
	fi
	if [ -z "${COMPOSE:-}" ]; then
		if docker compose version >/dev/null 2>&1; then
			COMPOSE="docker compose"
		else
			COMPOSE=docker-compose
		fi
	fi
	# --no-deps: generating a key needs no Redis or Postgres. The empty
	# environment keeps compose quiet about unset variables.
	# shellcheck disable=SC2086 # COMPOSE is a command with arguments.
	$COMPOSE run --rm --no-deps -T --entrypoint /usr/local/bin/switchyard gateway keys generate "$@"
}

# The printed key is the only indented line starting with sy_, and the hash
# is on the "hash: sha256:..." line of the config snippet.
api_out=$(keys_generate --tenant default --key-id quickstart)
api_key=$(printf '%s\n' "$api_out" | awk '/^ +sy_/ { print $1; exit }')
api_hash=$(printf '%s\n' "$api_out" | awk '$1 == "hash:" { print $2; exit }')
admin_out=$(keys_generate --tenant default --key-id admin)
admin_token=$(printf '%s\n' "$admin_out" | awk '/^ +sy_/ { print $1; exit }')
if [ -z "$api_key" ] || [ -z "$api_hash" ] || [ -z "$admin_token" ]; then
	echo "init.sh: could not read the output of switchyard keys generate" >&2
	exit 1
fi

# Route "auto" only to the provider with a key when exactly one is given.
allow='*'
count=0
for pair in OPENAI_API_KEY:openai ANTHROPIC_API_KEY:anthropic GEMINI_API_KEY:google TOGETHER_API_KEY:together; do
	var=${pair%%:*}
	eval "val=\${$var:-}"
	if [ -n "$val" ]; then
		count=$((count + 1))
		allow="*@${pair#*:}"
	fi
done
if [ "$count" -ne 1 ]; then
	allow='*'
fi
if [ "$count" -eq 0 ]; then
	echo "init.sh: warning: no provider key in the environment; add OPENAI_API_KEY or ANTHROPIC_API_KEY to $env_file" >&2
fi

umask 077
tmp=$(mktemp "$env_file.XXXXXX")
trap 'rm -f "$tmp"' EXIT
{
	echo "# Written by deploy/init.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ). Holds secrets; never commit it."
	echo
	echo "# Provider keys. Set at least one; restart the gateway after a change."
	for var in OPENAI_API_KEY ANTHROPIC_API_KEY GEMINI_API_KEY TOGETHER_API_KEY; do
		eval "val=\${$var:-}"
		if [ -n "$val" ]; then
			printf '%s=%s\n' "$var" "$val"
		else
			printf '#%s=\n' "$var"
		fi
	done
	echo
	echo "# Deployments \"auto\" may use (a glob over model@provider)."
	printf 'SWITCHYARD_ALLOW=%s\n' "$allow"
	echo
	echo "# Admin token for /admin/v1/* (Authorization: Bearer ...)."
	printf 'SWITCHYARD_ADMIN_TOKEN=%s\n' "$admin_token"
	echo
	echo "# The gateway key \"quickstart\": the gateway reads only its hash."
	printf 'SWITCHYARD_API_KEY_HASH=%s\n' "$api_hash"
	echo "# The key itself, for your client. Not read by the gateway; delete it"
	echo "# once you have stored the key elsewhere."
	printf 'SWITCHYARD_API_KEY=%s\n' "$api_key"
	echo
	echo "# Optional settings, with their defaults:"
	echo "#SWITCHYARD_PORT=8080                 # public API, on SWITCHYARD_BIND"
	echo "#SWITCHYARD_BIND=127.0.0.1            # 0.0.0.0 to serve other hosts"
	echo "#SWITCHYARD_ADMIN_PORT=9090           # admin listener, loopback only"
	echo "#SWITCHYARD_LOG_LEVEL=info"
	echo "#SWITCHYARD_MONTHLY_BUDGET_USD=100"
	echo "#SWITCHYARD_TRACING_ENABLED=false     # true with --profile observability"
	echo "#SWITCHYARD_TRACING_SAMPLE_RATIO=1"
	echo "#SWITCHYARD_SEMANTIC_CACHE=false      # true needs SWITCHYARD_EMBEDDING_MODEL"
	echo "#SWITCHYARD_EMBEDDING_MODEL=          # bge-small-en-v1.5 (--profile tei) or text-embedding-3-small"
	echo "#POSTGRES_PASSWORD=switchyard         # set before the first start; see docs/quickstart.md"
	echo "#GRAFANA_ADMIN_PASSWORD=admin"
} >"$tmp"
mv "$tmp" "$env_file"
trap - EXIT

cat <<EOF
Wrote $PWD/$env_file (mode 0600).

Your gateway API key (also in .env as SWITCHYARD_API_KEY):

    $api_key

Next:
    docker compose up -d --wait
    curl -s localhost:8080/v1/chat/completions \\
      -H "Authorization: Bearer $api_key" -H 'Content-Type: application/json' \\
      -d '{"model":"auto","messages":[{"role":"user","content":"Say hello"}]}'
EOF
