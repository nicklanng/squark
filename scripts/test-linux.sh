#!/bin/sh
# Runs the test suite on Linux in Docker: epoll, SO_REUSEPORT, the stack-clash flags and glibc only
# exist there. The image is built once (scripts/linux.Dockerfile); Go's caches persist in volumes.
#
#   scripts/test-linux.sh                    the full suite (with the ASan run), clang
#   scripts/test-linux.sh -short             skip the slow ASan run
#   scripts/test-linux.sh -run TestHTTP -v   any go test flags
#   CC=gcc scripts/test-linux.sh             build programs with gcc
#   FUZZ=2000 scripts/test-linux.sh -short   also run that many fuzz mutants under ASan
#   PLATFORM=linux/amd64 scripts/test-linux.sh -short   x86-64 (emulated on an arm64 host: slow)
set -e
cd "$(dirname "$0")/.."
PLATFORM=${PLATFORM:-}
TAG=squark-linux-test${PLATFORM:+-$(echo "$PLATFORM" | tr / -)}
docker build -q ${PLATFORM:+--platform "$PLATFORM"} -t "$TAG" -f scripts/linux.Dockerfile scripts >/dev/null
docker run --rm ${PLATFORM:+--platform "$PLATFORM"} -v "$PWD":/src:ro -v "$TAG-go-build":/root/.cache/go-build -v squark-go-mod:/go/pkg/mod \
	-e CC="${CC:-clang}" -e FUZZ="${FUZZ:-0}" "$TAG" sh -c '
set -e
mkdir /w && cd /src && tar --exclude=./.claude --exclude=./bin --exclude=./bench/bin -cf - . | tar -C /w -xf -
cd /w
go build -o bin/squark ./cmd/squark
echo "linux/$(go env GOARCH), CC=$CC ($($CC --version | head -1))"
go test ./internal/... ./cmd/... "$@"
if [ "$FUZZ" -gt 0 ]; then
	SQUARK_FUZZ=$FUZZ go test ./internal/driver -run TestFuzzSoundness -v -timeout 2h | grep -v "^=== RUN"
fi
' sh "$@"
