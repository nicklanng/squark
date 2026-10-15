# The Linux test environment for scripts/test-linux.sh: Go, clang with its sanitizer runtimes, gcc,
# binutils (nm, size) and SQLite. The repo is mounted at /src read-only and copied, so nothing here
# depends on it.
FROM golang:1.27
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
	clang libclang-rt-dev gcc binutils libsqlite3-dev procps curl >/dev/null && rm -rf /var/lib/apt/lists/*
