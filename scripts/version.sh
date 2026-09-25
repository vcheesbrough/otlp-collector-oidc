#!/bin/sh
# Prints the semantic version of the checked-out commit, from git tags alone
# (there is no version file):
#   - a commit tagged vX.Y.Z             → X.Y.Z
#   - any other commit, last tag vX.Y.Z  → X.(Y+1).0-dev+<sha>
#   - no tag yet                         → 0.1.0-dev+<sha>
# Uncommitted changes add ".dirty" to the build metadata of a dev version.
set -eu

pattern='v[0-9]*.[0-9]*.[0-9]*'
sha=$(git rev-parse --short=12 HEAD)
dirty=""
if ! git diff --quiet HEAD -- 2>/dev/null; then
	dirty=".dirty"
fi

if [ -z "$dirty" ] && tag=$(git describe --tags --exact-match --match "$pattern" HEAD 2>/dev/null); then
	echo "${tag#v}"
	exit 0
fi

last=$(git describe --tags --abbrev=0 --match "$pattern" HEAD 2>/dev/null || echo v0.0.0)
last=${last#v}
major=${last%%.*}
rest=${last#*.}
minor=${rest%%.*}
echo "${major}.$((minor + 1)).0-dev+${sha}${dirty}"
