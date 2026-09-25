#!/bin/sh
# Decides what one release run publishes, for the commit checked out in the
# current directory. Inputs (environment):
#   IMAGE      image name, e.g. ghcr.io/vcheesbrough/otlp-collector-oidc
#   REF        what CI ran on: "main" or a tag "vX.Y.Z"
#   PR_BRANCH  head branch of the PR merged into main ("" for a direct push)
# Prints key=value lines for $GITHUB_OUTPUT:
#   version    the version to link into the image
#   tag        a git tag to create and push first, or empty
#   tags       comma-separated image tags to publish
#
# A merge of an iteration PR (branch feat/iteration-N-...) releases vM.N.0,
# M being the current major: the iteration number is the minor (AGENTS.md
# "Versioning"). N must be exactly the next iteration, so a mistyped branch
# fails the release instead of publishing a version nobody can take back.
# Any other merge to main publishes :edge only. A pushed v* tag publishes
# exactly that version. :latest only from 1.0.0 on. Every tag lookup is
# scripts/version.sh's.
set -eu

: "${IMAGE:?}" "${REF:?}"
PR_BRANCH=${PR_BRANCH:-}
here=$(dirname "$0")

fail() {
	echo "release-plan: $*" >&2
	exit 1
}

stable() {
	case "$1" in 0.* | *-*) return 1 ;; *) return 0 ;; esac
}

tag=""
case "$REF" in
main)
	iteration=$(printf '%s\n' "$PR_BRANCH" | sed -n 's|^feat/iteration-\([0-9][0-9]*\)-.*|\1|p')
	if [ -z "$iteration" ]; then
		version=$("$here/version.sh")
		tags="$IMAGE:edge"
	else
		last=$("$here/version.sh" --last)
		major=${last%%.*}
		next=$(($("$here/version.sh" --last-iteration) + 1))
		version="$major.$iteration.0"
		tag="v$version"
		existing=$(git rev-parse -q --verify "refs/tags/$tag^{commit}" || true)
		if [ -n "$existing" ]; then
			# A rerun of a release that already tagged this commit.
			[ "$existing" = "$(git rev-parse HEAD)" ] || fail "$tag already tags another commit"
			tag=""
		elif [ "$iteration" -ne "$next" ]; then
			fail "branch says iteration $iteration, but the next iteration is $next (last release $last)"
		fi
		tags="$IMAGE:edge,$IMAGE:$version"
		if stable "$version"; then tags="$tags,$IMAGE:latest"; fi
	fi
	;;
v*)
	version=$("$here/version.sh")
	[ "v$version" = "$REF" ] || fail "tag $REF does not match the checked-out version $version"
	tags="$IMAGE:$version"
	if stable "$version"; then tags="$tags,$IMAGE:latest"; fi
	;;
*)
	fail "not a release ref: $REF"
	;;
esac

echo "version=$version"
echo "tag=$tag"
echo "tags=$tags"
