#!/bin/sh
# Compares the loop kernels (bench/squark/kernels) built by another revision's compiler with
# the working tree's: the same kernel sources, two compilers. The other compiler builds them
# without simd.sq (it may not have SIMD types), so a SIMD kernel (sum-float64/simd) is compared
# with its scalar kernel (sum-float64) built by the other compiler.
#
#   bench/compare.sh          against main
#   bench/compare.sh REF      against any git revision
#   RUNS=5 bench/compare.sh   best of 5 (default 3)
set -e
cd "$(dirname "$0")"
BENCH=$(pwd)
REF=${1:-main}
mkdir -p bin results
tmp=$(mktemp -d)
trap 'git -C "$BENCH/.." worktree remove --force "$tmp/src" >/dev/null 2>&1; rm -rf "$tmp"' EXIT

echo "building $REF..." >&2
git -C "$BENCH/.." worktree add -q --detach "$tmp/src" "$REF"
(cd "$tmp/src" && go build -o "$tmp/squark" ./cmd/squark)
mkdir "$tmp/kernels"
cp squark/kernels/main.sq "$tmp/kernels/"
cat > "$tmp/kernels/simd.sq" <<'STUB'
package main

func simdKernel(name string, reps int) (int64, bool) { return 0, false }
STUB
"$tmp/squark" build -o bin/sq-kernels-base "$tmp/kernels"

echo "building the working tree..." >&2
(cd .. && go build -o bin/squark ./cmd/squark)
../bin/squark build -o bin/sq-kernels squark/kernels
go build -o bin/harness ./harness

ONLY=compare BASE="$REF" ./bin/harness | tee results/compare.md
