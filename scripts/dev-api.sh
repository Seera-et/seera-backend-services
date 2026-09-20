#!/usr/bin/env bash
#
# Build and run the API on the host with .env loaded.
#
#   bash scripts/dev-api.sh
#
# Two things this does that `go run ./cmd/api` does not:
#
#   1. Loads .env. The binary has no godotenv dependency — it reads the process
#      environment, which docker-compose fills from env_file. On the host that
#      job is nobody's until here.
#   2. Builds a binary and execs it. `go run` compiles to a temporary file and
#      runs it as a *child*, so Ctrl-C or a kill aimed at `go run` can leave the
#      real server alive, still holding the port and still serving the code it
#      was built from.

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ ! -f .env ]]; then
	echo "no .env here — start from .env.example" >&2
	exit 1
fi

# Read rather than `source`: values are not shell-quoted, and a password
# containing & or ( would be a syntax error at best.
while IFS= read -r line || [[ -n "$line" ]]; do
	line="${line%$'\r'}"
	case "$line" in '' | '#'*) continue ;; esac
	[[ "$line" == *=* ]] || continue
	export "${line%%=*}=${line#*=}"
done <.env

BIN=tmp/api
[[ "${OS:-}" == "Windows_NT" ]] && BIN=tmp/api.exe

mkdir -p tmp
go build -o "$BIN" ./cmd/api
exec "./$BIN"
