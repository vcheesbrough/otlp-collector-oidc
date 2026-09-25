#!/bin/sh
# Tests scripts/release-plan.sh against throwaway git repositories.
set -eu

plan=$(cd "$(dirname "$0")" && pwd)/release-plan.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

# repo NAME [TAG...]: a fresh repository with one commit, tagged as given.
repo() {
	dir="$work/$1"
	shift
	mkdir -p "$dir/scripts"
	cp "$(dirname "$plan")/version.sh" "$(dirname "$plan")/release-plan.sh" "$dir/scripts/"
	git -C "$dir" init -q
	git -C "$dir" -c user.name=t -c user.email=t@t commit -q --allow-empty -m first
	for t in "$@"; do git -C "$dir" -c user.name=t -c user.email=t@t tag -a "$t" -m "$t"; done
	# Leave HEAD on a commit after the tags, as a merge to main would be.
	git -C "$dir" -c user.name=t -c user.email=t@t commit -q --allow-empty -m merge
	echo "$dir"
}

# check NAME DIR REF PR_BRANCH WANT: WANT is the expected output, or "FAIL".
check() {
	name=$1 dir=$2 ref=$3 branch=$4 want=$5
	if got=$(cd "$dir" && IMAGE=img REF=$ref PR_BRANCH=$branch scripts/release-plan.sh 2>/dev/null); then :; else got=FAIL; fi
	got=$(printf '%s' "$got" | sed 's/-dev+[0-9a-f]*/-dev+SHA/')
	if [ "$got" = "$want" ]; then
		echo "ok   $name"
	else
		echo "FAIL $name"
		echo "  want: $want" | tr '\n' ' '
		echo
		echo "  got:  $got" | tr '\n' ' '
		echo
		failures=$((failures + 1))
	fi
}

nl='
'
r=$(repo first)
check "first iteration, no tags yet" "$r" main feat/iteration-1-scaffold \
	"version=0.1.0${nl}tag=v0.1.0${nl}tags=img:edge,img:0.1.0"
r=$(repo second v0.1.0)
check "next iteration" "$r" main feat/iteration-2-auth \
	"version=0.2.0${nl}tag=v0.2.0${nl}tags=img:edge,img:0.2.0"
check "non-iteration merge publishes edge only" "$r" main ci/auto-tag-iterations \
	"version=0.2.0-dev+SHA${nl}tag=${nl}tags=img:edge"
check "direct push publishes edge only" "$r" main "" \
	"version=0.2.0-dev+SHA${nl}tag=${nl}tags=img:edge"
check "an iteration number already released fails" "$r" main feat/iteration-1-again FAIL
check "an iteration number that skips ahead fails" "$r" main feat/iteration-20-typo FAIL
r=$(repo post-mvp v0.6.0 v1.0.0)
check "post-MVP iteration keeps the major, minor carries on" "$r" main feat/iteration-7-thing \
	"version=1.7.0${nl}tag=v1.7.0${nl}tags=img:edge,img:1.7.0,img:latest"
check "post-MVP edge build names the next iteration" "$r" main ci/x \
	"version=1.7.0-dev+SHA${nl}tag=${nl}tags=img:edge"
check "post-MVP iteration restarting the minor fails" "$r" main feat/iteration-1-thing FAIL
r=$(repo mvp v0.6.0)
git -C "$r" -c user.name=t -c user.email=t@t tag -a v0.7.0 -m v0.7.0
git -C "$r" -c user.name=t -c user.email=t@t tag -a v1.0.0 -m v1.0.0
check "the MVP tag pushed beside an iteration tag publishes 1.0.0" "$r" v1.0.0 "" \
	"version=1.0.0${nl}tag=${nl}tags=img:1.0.0,img:latest"

r=$(repo rerun v0.1.0)
git -C "$r" -c user.name=t -c user.email=t@t tag -a v0.2.0 -m v0.2.0
check "rerun after tagging this commit does not tag again" "$r" main feat/iteration-2-auth \
	"version=0.2.0${nl}tag=${nl}tags=img:edge,img:0.2.0"
r=$(repo taken v0.1.0 v0.2.0)
check "an iteration tag on another commit fails" "$r" main feat/iteration-2-auth FAIL

r=$(repo tagpush v0.1.0)
git -C "$r" -c user.name=t -c user.email=t@t tag -a v0.1.1 -m v0.1.1
check "pushed patch tag publishes that version" "$r" v0.1.1 "" \
	"version=0.1.1${nl}tag=${nl}tags=img:0.1.1"
check "pushed tag not on this commit fails" "$r" v0.1.0 "" FAIL
r=$(repo stable v1.0.0)
git -C "$r" -c user.name=t -c user.email=t@t tag -a v1.0.1 -m v1.0.1
check "a stable pushed tag also publishes latest" "$r" v1.0.1 "" \
	"version=1.0.1${nl}tag=${nl}tags=img:1.0.1,img:latest"
check "an unknown ref fails" "$r" feature "" FAIL

[ "$failures" -eq 0 ] || { echo "$failures failed" >&2; exit 1; }
