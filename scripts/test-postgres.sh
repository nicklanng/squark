#!/bin/sh
# Runs TestPostgres (internal/driver) against a throwaway PostgreSQL in Docker, with SCRAM-SHA-256
# password authentication (the default since Postgres 14).
#
#   scripts/test-postgres.sh            Postgres 16
#   PG=postgres:17-alpine scripts/test-postgres.sh
set -e
cd "$(dirname "$0")/.."
PG=${PG:-postgres:16-alpine}
NAME=squark-test-postgres-$$
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=s3cret -e POSTGRES_USER=squark \
	-e POSTGRES_HOST_AUTH_METHOD=scram-sha-256 -e POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 \
	-p 127.0.0.1::5432 "$PG" >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1' EXIT
PORT=$(docker port "$NAME" 5432/tcp | head -1 | sed 's/.*://')
for i in $(seq 1 60); do
	docker exec "$NAME" pg_isready -U squark -h 127.0.0.1 >/dev/null 2>&1 && break
	sleep 0.5
done
SQUARK_PG_URL="postgres://squark:s3cret@127.0.0.1:$PORT/squark" go test ./internal/driver -run TestPostgres -count=1 -v "$@"
