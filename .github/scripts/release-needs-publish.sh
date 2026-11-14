#!/usr/bin/env bash
# Print true when GoReleaser must publish a tag, false when its release is
# already published. Arguments: the HTTP status and response body file of
# GET /repos/{owner}/{repo}/releases/tags/{tag}.
#
# GoReleaser keeps a release as a draft until every artifact is uploaded, so a
# published release is complete; a missing or draft release needs a retry. Any
# other status or an unreadable body fails rather than guessing.
set -euo pipefail

status=$1
body=$2

case "$status" in
	404)
		echo true
		;;
	200)
		draft=$(jq -r '.draft | if type == "boolean" then tostring else "invalid" end' "$body" 2>/dev/null) || draft=
		case "$draft" in
			true) echo true ;;
			false) echo false ;;
			*)
				echo "::error::Release lookup returned an unreadable body." >&2
				exit 1
				;;
		esac
		;;
	*)
		echo "::error::Release lookup failed (HTTP $status)." >&2
		head -c 500 "$body" >&2 || true
		exit 1
		;;
esac
