# shop: services under resource limits

Two services from one program: `stock` keeps item counts, and `orders` takes an item from stock
for each order, calling it by name. It is small on purpose; what it shows is how a squark service
behaves in a container.

## What to look at

- **Proven limits.** `squark build -v -manifest k8s examples/shop` prints the worst case (stacks,
  static data, and the kernel's buffers for at most 512 sockets of 16 KiB) and the pod resources
  that fit it: 68 Mi and one CPU. Start it with less (`docker run --memory 32m ...`) and it refuses
  to start, naming both numbers, instead of being killed later at its busiest.
- **Calls with a deadline.** orders calls stock under `with deadline 300ms`, name lookup included;
  `{"item": "slow"}` makes stock take 5 seconds and orders answer 504 at 300 ms.
- **Stops that drop nothing.** SIGTERM (a rolling update, `docker stop`) makes `/readyz` say
  `stopping`, lets requests in progress finish (`shutdown 20s`), and exits 0.
- **Probes and metrics on their own port.** `:9090/healthz`, `/readyz` and `/metrics` are served
  from a pool of their own, so a full main pool can't fail liveness. `/metrics` has
  `shop_orders_total{result}`, `http_requests_total{code}` and the runtime's pools and sockets.
- **JSON logs** on stdout, one object per line.
- **A one-file image.** The Dockerfile builds `-static` and copies the binary into `FROM scratch`.

## Running it

```sh
squark run examples/shop          # the self-test: both roles in one process, a scripted client

# containers (from the repository root)
docker compose -f examples/shop/docker-compose.yml up --build
curl -X POST localhost:8080/orders -d '{"item":"apple"}'
curl localhost:9090/metrics

# Kubernetes (minikube)
minikube image build -t squark-shop -f examples/shop/Dockerfile .
kubectl apply -f examples/shop/k8s.yaml
kubectl port-forward svc/orders 8080:8080
```

Each stock replica keeps its own counts: it's a demo, not a database.
