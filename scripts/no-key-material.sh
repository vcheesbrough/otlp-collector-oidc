#!/bin/sh
# Fails if any tracked file holds a PEM block. Test keys and certificates are
# generated at run time; the image generates its own at build.
set -eu
if git grep -nE -e '-{5}BEGIN [A-Z ]*(PRIVATE KEY|CERTIFICATE)-{5}' -- .; then
	echo "key or certificate material is committed" >&2
	exit 1
fi
