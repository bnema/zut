#!/usr/bin/env bash
# Print the release tag for HEAD, or nothing when its unreleased commits do not
# require a release. A tag already pointing to HEAD is emitted again so a
# failed GoReleaser job can be retried without minting a second tag.
set -euo pipefail

latest=$(git tag --list 'v*.*.*' --sort=-v:refname | head -n1)
head=$(git rev-parse HEAD)
if [ -n "$latest" ] && [ "$(git rev-parse "$latest^{}")" = "$head" ]; then
	printf '%s\n' "$latest"
	exit 0
fi

if [ -n "$latest" ]; then
	range="${latest}..HEAD"
else
	range="HEAD"
fi

minor=false
patch=false
while IFS= read -r commit; do
	subject=$(git log -1 --format=%s "$commit")
	body=$(git log -1 --format=%b "$commit")

	# A breaking change is a v0.x minor release.
	if grep -Eq '^[[:alpha:]]+(\([^)]+\))?!:' <<<"$subject" || grep -Eiq '^BREAKING[ -]CHANGE:' <<<"$body"; then
		minor=true
		continue
	fi

	type=${subject%%(*}
	type=${type%%:*}
	type=${type%%!*}
	type=$(tr '[:upper:]' '[:lower:]' <<<"$type")
	case "$type" in
		feat) minor=true ;;
		fix|perf|refactor|build|chore|ci) patch=true ;;
		docs|test|style) ;;
		*) ;;
	esac
done < <(git rev-list "$range")

if [ "$minor" != true ] && [ "$patch" != true ]; then
	exit 0
fi

# The renamed project starts a new public release line at v0.1.0.
if [ -z "$latest" ]; then
	printf '%s\n' 'v0.1.0'
	exit 0
fi

version=${latest#v}
IFS=. read -r major minor_version patch_version <<<"$version"
if [ "$minor" = true ]; then
	printf 'v%s.%s.0\n' "$major" "$((minor_version + 1))"
else
	printf 'v%s.%s.%s\n' "$major" "$minor_version" "$((patch_version + 1))"
fi
