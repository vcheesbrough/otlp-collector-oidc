#!/bin/sh
# oci-labels-check.sh: every static OCI label the Dockerfile sets is also an
# index annotation in release.yml, with the same value, and the reverse; the
# per-build ones (version, revision, created) must appear in both. The two
# lists are written twice because GHCR reads a multi-arch image's labels from
# its index, which docker build does not annotate.
# Also: base.name is the runtime stage's FROM image. The repository root
# is $1, the current directory by default (the test points it at a copy).
set -eu
root=${1:-.}
labels=$(sed -n 's/^.*\(org\.opencontainers\.image\.[a-z.]*\)="\([^"]*\)".*$/\1=\2/p' "$root/Dockerfile" | sort)
annotations=$(sed -n 's/^ *index:\(org\.opencontainers\.image\.[a-z.]*=.*\)$/\1/p' "$root/.github/workflows/release.yml" | sort)
base=$(sed -n 's/^FROM \([^ ]*\)\( AS .*\)\{0,1\}$/\1/p' "$root/Dockerfile" | tail -n 1)
case "$base" in
*/*) ;;
*) base="docker.io/library/$base" ;;
esac
if ! printf '%s\n' "$labels" | grep -qx "org.opencontainers.image.base.name=$base"; then
	echo "oci-labels-check: base.name is not the runtime image, $base" >&2
	exit 1
fi
static() { grep -v -E '^org\.opencontainers\.image\.(version|revision|created)='; }
keys() { sed 's/=.*//'; }
if [ "$(printf '%s\n' "$labels" | static)" != "$(printf '%s\n' "$annotations" | static)" ]; then
	echo "oci-labels-check: the Dockerfile labels and release.yml index annotations differ" >&2
	tmp=$(mktemp -d)
	printf '%s\n' "$labels" | static >"$tmp/Dockerfile"
	printf '%s\n' "$annotations" | static >"$tmp/release.yml"
	diff "$tmp/Dockerfile" "$tmp/release.yml" >&2 || true
	rm -rf "$tmp"
	exit 1
fi
if [ "$(printf '%s\n' "$labels" | keys)" != "$(printf '%s\n' "$annotations" | keys)" ]; then
	echo "oci-labels-check: the label keys differ" >&2
	exit 1
fi
echo "oci-labels-check: $(printf '%s\n' "$labels" | wc -l) labels match"
