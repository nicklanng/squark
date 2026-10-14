#!/bin/sh
# Compares the notes server as one process, as `workers 3` (processes), as `threads 3` (scheduler
# threads in one process), and Go with GOMAXPROCS=3, on Linux in Docker (macOS doesn't spread
# SO_REUSEPORT connections across processes). The load generator
# shares the machine, as in run.sh. Needs Docker; the repo is mounted read-only.
#
#   bench/workers-linux.sh
set -e
cd "$(dirname "$0")/.."
docker run --rm -v "$PWD":/src:ro golang:1.27 sh -c '
set -e
apt-get update -qq >/dev/null && apt-get install -y -qq clang binutils libsqlite3-dev procps >/dev/null 2>&1
cp -r /src /w && cd /w
go build -o bin/squark ./cmd/squark
go build -o /tmp/load ./bench/load
(cd bench && go build -o /tmp/gonotes ./gonotes)
bin/squark build -o /tmp/notes1 examples/notes >/dev/null
cp -r examples/notes /tmp/notesw && sed -i '"'"'/^budget /d'"'"' /tmp/notesw/main.sq && printf '"'"'package main\n\nworkers 3\n'"'"' > /tmp/notesw/workers.sq
bin/squark build -o /tmp/notes3 /tmp/notesw >/dev/null
cp -r examples/notes /tmp/notest && sed -i '"'"'/^budget /d'"'"' /tmp/notest/main.sq && printf '"'"'package main\n\nthreads 3\n'"'"' > /tmp/notest/threads.sq
bin/squark build -o /tmp/notest3 /tmp/notest >/dev/null
cpu() { # total utime+stime (seconds) of a process and its children
	t=0
	for p in $1 $(pgrep -P $1); do
		set -- $(cut -d'"'"' '"'"' -f14,15 /proc/$p/stat)
		t=$((t + $1 + $2))
	done
	echo $t
}
run() { # name, command...
	name=$1; shift
	rm -f /tmp/b.db*
	"$@" & P=$!
	sleep 0.6
	curl -s -X POST -d '"'"'{"title":"hello","body":"the quick brown fox"}'"'"' http://127.0.0.1:8080/notes >/dev/null
	for sc in health note mix; do
		case $sc in
		health) args="-url http://127.0.0.1:8080/health";;
		note) args="-url http://127.0.0.1:8080/notes/1";;
		mix) args="-url http://127.0.0.1:8080/notes -mix -seed 200";;
		esac
		c0=$(cpu $P)
		out=$(GOMAXPROCS=3 /tmp/load -c 64 -d 5s -json $args 2>/dev/null | tail -1)
		c1=$(cpu $P)
		hz=$(getconf CLK_TCK)
		rps=$(echo "$out" | grep -o '"'"'"rps":[0-9.]*'"'"' | cut -d: -f2 | cut -d. -f1)
		p50=$(echo "$out" | grep -o '"'"'"p50":[0-9.]*'"'"' | head -1 | cut -d: -f2 | cut -d. -f1)
		p99=$(echo "$out" | grep -o '"'"'"p99":[0-9.]*'"'"' | head -1 | cut -d: -f2 | cut -d. -f1)
		printf '"'"'%-20s %-7s %8s req/s  p50 %6sus  p99 %6sus  server cpu %ss of 5s\n'"'"' "$name" "$sc" "$rps" "$p50" "$p99" "$(( (c1 - c0) / hz ))"
	done
	kill $P; wait $P 2>/dev/null || true
}
run "squark (1 process)" /tmp/notes1 -port 8080 -db /tmp/b.db
run "squark (workers 3)" /tmp/notes3 -port 8080 -db /tmp/b.db
run "squark (threads 3)" /tmp/notest3 -port 8080 -db /tmp/b.db
run "go (GOMAXPROCS=3)" env GOMAXPROCS=3 /tmp/gonotes -port 8080 -db /tmp/b.db
'
