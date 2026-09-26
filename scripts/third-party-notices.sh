#!/bin/sh
# third-party-notices.sh OUT: copy the licence and notice files of every
# module linked into the binary into OUT/<module path>/, and list them, with
# their versions, in OUT/MODULES. Run from the repository root with the
# modules downloaded; the image build puts OUT at
# /usr/share/licenses/otlp-collector-oidc/third-party. Fails if a module
# carries no licence file, so a new dependency cannot ship without one.
set -eu
out=$1
mkdir -p "$out"
mods=$(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/otlp-collector-oidc | sort -u)
: >"$out/MODULES"
echo "$mods" | while read -r path version dir; do
	[ -n "$path" ] || continue
	dest="$out/$path"
	mkdir -p "$dest"
	found=0
	for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/License* "$dir"/NOTICE* "$dir"/COPYING*; do
		if [ -f "$f" ]; then
			cp "$f" "$dest/"
			found=1
		fi
	done
	if [ "$found" = 0 ]; then
		echo "third-party-notices: no licence file in $path@$version ($dir)" >&2
		exit 1
	fi
	echo "$path $version" >>"$out/MODULES"
done
echo "third-party-notices: $(wc -l <"$out/MODULES") modules in $out" >&2
