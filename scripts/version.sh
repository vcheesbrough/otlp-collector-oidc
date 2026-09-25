#!/bin/sh
# The one place a version is read from git tags (there is no version file).
#
#   scripts/version.sh                  the version of the checked-out commit:
#     - a commit tagged vX.Y.Z            → X.Y.Z (the highest, if several)
#     - any other commit                  → M.(I+1).0-dev+<sha>, M the major of
#                                           the highest release, I the highest
#                                           iteration released (the minor
#                                           carries on across the MVP)
#     - no release yet                    → 0.1.0-dev+<sha>
#     Uncommitted changes add ".dirty" to the build metadata of a dev version.
#   scripts/version.sh --last           the highest release reachable, X.Y.Z
#                                       (0.0.0 if none)
#   scripts/version.sh --last-iteration the highest minor of any reachable
#                                       release (0 if none)
set -eu

pattern='v[0-9]*.[0-9]*.[0-9]*'

# Tags can share a commit (the MVP's v1.0.0 sits on an iteration's v0.N.0),
# so every lookup takes the highest version, never the nearest tag.
releases() {
	git tag "$@" --list "$pattern" | sed 's/^v//' | sort -V
}

last() {
	v=$(releases --merged HEAD | tail -n 1)
	echo "${v:-0.0.0}"
}

last_iteration() {
	i=$(releases --merged HEAD | cut -d. -f2 | sort -n | tail -n 1)
	echo "${i:-0}"
}

case "${1:-}" in
--last)
	last
	exit 0
	;;
--last-iteration)
	last_iteration
	exit 0
	;;
"") ;;
*)
	echo "usage: $0 [--last | --last-iteration]" >&2
	exit 2
	;;
esac

sha=$(git rev-parse --short=12 HEAD)
dirty=""
if ! git diff --quiet HEAD -- 2>/dev/null; then
	dirty=".dirty"
fi

if [ -z "$dirty" ]; then
	exact=$(releases --points-at HEAD | tail -n 1)
	if [ -n "$exact" ]; then
		echo "$exact"
		exit 0
	fi
fi

major=$(last | cut -d. -f1)
echo "${major}.$(($(last_iteration) + 1)).0-dev+${sha}${dirty}"
