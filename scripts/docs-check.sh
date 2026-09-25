#!/bin/sh
# Fails unless the README's Configuration table lists exactly the environment
# variables the shipped configuration reads. The generated reference replaces
# this with the renderer; until then the README is the reference.
set -eu

config=config/collector.yaml
readme=README.md

used=$(grep -oE '\$\{env:[A-Z0-9_]+' "$config" | sed 's/.*env://' | sort -u)
# Rows of the Configuration section's table: "| `VAR` | ...".
documented=$(awk '/^## Configuration/{on=1; next} /^## /{on=0} on' "$readme" |
	grep -oE '^\| `[A-Z0-9_]+`' | sed 's/^| `//; s/`$//' | sort -u)

missing=$(printf '%s\n' "$used" | grep -vxF "$documented" || true)
stale=$(printf '%s\n' "$documented" | grep -vxF "$used" || true)

status=0
if [ -n "$missing" ]; then
	echo "used by $config but not documented in $readme:" >&2
	printf '  %s\n' $missing >&2
	status=1
fi
if [ -n "$stale" ]; then
	echo "documented in $readme but not used by $config:" >&2
	printf '  %s\n' $stale >&2
	status=1
fi
exit $status
