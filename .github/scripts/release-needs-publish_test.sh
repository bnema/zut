#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
script="$repo_root/.github/scripts/release-needs-publish.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

expect() {
	local want=$1 status=$2 body=$3
	local got
	printf '%s' "$body" >"$tmp/body"
	if ! got=$("$script" "$status" "$tmp/body" 2>/dev/null); then
		got=error
	fi
	if [ "$got" != "$want" ]; then
		printf 'release-needs-publish %s %q = %q, want %q\n' "$status" "$body" "$got" "$want" >&2
		exit 1
	fi
}

expect false 200 '{"draft":false,"assets":[{"name":"checksums.txt"}]}'
expect true 200 '{"draft":true,"assets":[]}'
expect true 404 '{"message":"Not Found"}'
expect error 200 '{"assets":[]}'
expect error 200 'not json'
expect error 200 ''
expect error 200 '{"draft":false}{"draft":true}'
expect error 200 '{"draft":"false"}'
expect error 401 '{"message":"Bad credentials"}'
expect error 500 ''
