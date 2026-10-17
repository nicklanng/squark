# Builds shop as a static binary and puts it in an empty image. From the repository root:
#
#    docker build -f examples/shop/Dockerfile -t squark-shop .
#
# The image holds one file, the ~1 MB binary: no libc, no shell. Docker and Kubernetes give a
# container its /etc/hosts and /etc/resolv.conf, which is all net needs to resolve service names.
FROM golang:1.27 AS build
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends clang libsqlite3-dev >/dev/null
WORKDIR /src
COPY . .
RUN go build -o bin/squark ./cmd/squark && bin/squark build -static -o /shop examples/shop

FROM scratch
COPY --from=build /shop /shop
USER 65534:65534
ENTRYPOINT ["/shop"]
