#!/usr/bin/env bash
# bench-compare.sh runs the same benchmarks on a base commit and on the
# working tree, and compares them with benchstat (a tool of the root module).
#
#   scripts/bench-compare.sh                      # origin/main vs the working tree, every benchmark
#   BASE=HEAD~3 BENCH=Fanout PKGS=./apron/server COUNT=10 scripts/bench-compare.sh
#
# BASE is any commit. The base runs in a temporary git worktree, so the
# working tree, uncommitted changes included, is what is compared against it.
# Benchmarks that exist on only one side are listed by benchstat without a
# comparison. Results go to OUT (build/bench by default): base.txt, head.txt,
# and compare.txt.
#
# Timing is only as quiet as the machine: run nothing else meanwhile, and
# trust allocs/op and B/op over sec/op when the machine is shared.
set -euo pipefail

BASE=${BASE:-origin/main}
BENCH=${BENCH:-.}
PKGS=${PKGS:-./... ./apron/...}
COUNT=${COUNT:-6}
BENCHFLAGS=${BENCHFLAGS:-}
root=$(git rev-parse --show-toplevel)
OUT=${OUT:-$root/build/bench}
mkdir -p "$OUT"

run() { # dir file
	# shellcheck disable=SC2086 # PKGS and BENCHFLAGS are lists.
	(cd "$1" && go test -run '^$' -bench "$BENCH" -benchmem -count "$COUNT" $BENCHFLAGS $PKGS) | tee "$2"
}

base=$(mktemp -d)
trap 'git -C "$root" worktree remove --force "$base"' EXIT
git -C "$root" worktree add --quiet --detach "$base" "$BASE"
# Packages whose tests read the submodules find them at their pinned commits.
for sub in testdata/apron .apron-web; do
	if [ -e "$root/$sub/.git" ] && [ ! -e "$base/$sub/.git" ]; then
		rmdir "$base/$sub" 2>/dev/null || true
		ln -s "$root/$sub" "$base/$sub"
	fi
done

echo "== base: $BASE ($(git -C "$base" rev-parse --short HEAD))" >&2
run "$base" "$OUT/base.txt"
echo "== head: working tree ($(git -C "$root" rev-parse --short HEAD)$(git -C "$root" diff --quiet || echo ', with changes'))" >&2
run "$root" "$OUT/head.txt"

# Run from OUT, so that benchstat labels the columns base.txt and head.txt.
benchstat=$(cd "$root" && go tool -n benchstat)
(cd "$OUT" && "$benchstat" base.txt head.txt) | tee "$OUT/compare.txt"
