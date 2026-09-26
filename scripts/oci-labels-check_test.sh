#!/bin/sh
# oci-labels-check_test.sh: oci-labels-check.sh passes on the repository and
# fails on each kind of drift, run against a copy it edits.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fresh() {
	rm -rf "$tmp/r"
	mkdir -p "$tmp/r/.github/workflows"
	cp "$here/Dockerfile" "$tmp/r/Dockerfile"
	cp "$here/.github/workflows/release.yml" "$tmp/r/.github/workflows/release.yml"
}
expect() { # expect pass|fail description
	if "$here/scripts/oci-labels-check.sh" "$tmp/r" >/dev/null 2>&1; then got=pass; else got=fail; fi
	[ "$got" = "$1" ] || { echo "FAIL: $2: got $got, want $1" >&2; exit 1; }
	echo "ok: $2"
}
fresh
expect pass "the repository as it is"
fresh
sed -i 's|index:org.opencontainers.image.licenses=.*|index:org.opencontainers.image.licenses=Apache-2.0|' "$tmp/r/.github/workflows/release.yml"
expect fail "an annotation with a different value"
fresh
sed -i '/index:org.opencontainers.image.url=/d' "$tmp/r/.github/workflows/release.yml"
expect fail "an annotation missing"
fresh
sed -i '/index:org.opencontainers.image.created=/d' "$tmp/r/.github/workflows/release.yml"
expect fail "a per-build annotation missing"
fresh
sed -i 's|^FROM alpine:3.24$|FROM alpine:3.25|' "$tmp/r/Dockerfile"
expect fail "the runtime image bumped without base.name"
