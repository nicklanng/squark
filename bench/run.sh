#!/bin/sh
# Builds every benchmark program (squark and Go) and runs the comparison harness.
#
#   ./run.sh                     full run (10s per HTTP scenario, 3 runs per compute benchmark)
#   DURATION=3s RUNS=1 ./run.sh  quick run
#   ONLY=http ./run.sh           just the HTTP server benchmarks (or ONLY=compute)
#   CONC=256 IDLE=8000 ./run.sh  more load connections / idle connections
#   LOADGEN=load ./run.sh        drive the servers with the Go load generator (bench/load) instead of
#                                the squark one (bench/squark/load); the Go one also has -startup,
#                                which launches a server process (squark can't start processes)
set -e
cd "$(dirname "$0")"
ROOT=..
mkdir -p bin results

echo "building squark compiler..." >&2
(cd $ROOT && go build -o bin/squark ./cmd/squark)
SQ=$ROOT/bin/squark

echo "building squark programs..." >&2
$SQ build -o bin/notes $ROOT/examples/notes
for p in sieve nbody wordfreq jsonenc load; do
	$SQ build -o bin/sq-$p squark/$p
done

echo "building Go programs..." >&2
go build -o bin/gonotes ./gonotes
go build -o bin/gocompute ./gocompute
go build -o bin/load ./load
go build -o bin/procstat ./procstat
go build -o bin/harness ./harness

./bin/harness | tee results/latest.md
