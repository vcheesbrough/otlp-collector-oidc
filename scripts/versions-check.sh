#!/bin/sh
# Fails unless every module builder.yaml pins is required at the same version
# in go.mod: the collector core, contrib and ocb move in lockstep.
set -eu

status=0
# "  - gomod: <module> <version>" lines; v0.0.0 is this repository itself.
grep -E '^[[:space:]]*- gomod:' builder.yaml | while read -r _ _ module version; do
	[ "$version" = "v0.0.0" ] && continue
	actual=$(go list -m -f '{{.Version}}' "$module")
	if [ "$actual" != "$version" ]; then
		echo "$module: builder.yaml pins $version, go.mod has $actual" >&2
		exit 1
	fi
done || status=1

# ocb and mdatagen are go.mod tools; they must be the builder's release too.
line=$(grep -E 'processor/batchprocessor v' builder.yaml | awk '{print $NF}')
for tool in go.opentelemetry.io/collector/cmd/builder go.opentelemetry.io/collector/cmd/mdatagen; do
	actual=$(go list -m -f '{{.Version}}' "$tool")
	if [ "$actual" != "$line" ]; then
		echo "$tool: go.mod has $actual, the pinned collector release is $line" >&2
		status=1
	fi
done
exit $status
