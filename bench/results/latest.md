# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-14T14:26:41+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| sieve 50M | 0.15s | 0.14s | 52.6 MiB | 48.9 MiB |
| nbody 50M | 1.52s | 1.08s | 4.2 MiB | 1.2 MiB |
| wordfreq 100MiB | 0.46s | 0.31s | 205.8 MiB | 7.5 MiB |
| json encode 2M | 2.66s | 0.53s | 4.8 MiB | 1.2 MiB |

## Notes server

| server | scenario | req/s | p50 | p99 | server CPU | peak RSS | threads |
|---|---|---:|---:|---:|---:|---:|---:|
| squark | GET /health | 220855 | 282µs | 629µs | 9.4s | 6.8 MiB | 1 |
| squark | GET /notes/1 | 202159 | 310µs | 441µs | 9.8s | 6.8 MiB | 1 |
| squark | mix 80/10/10 | 153095 | 409µs | 555µs | 9.8s | 8.8 MiB | 1 |
| go | GET /health | 175780 | 325µs | 1241µs | 40.8s | 21.2 MiB | 20 |
| go | GET /notes/1 | 109534 | 175µs | 2302µs | 41.6s | 21.7 MiB | 20 |
| go | mix 80/10/10 | 74617 | 1047µs | 2564µs | 33.4s | 23.0 MiB | 20 |
| go GOMAXPROCS=1 | GET /health | 135834 | 439µs | 1520µs | 9.7s | 15.0 MiB | 4 |
| go GOMAXPROCS=1 | GET /notes/1 | 116422 | 547µs | 989µs | 10.0s | 15.1 MiB | 4 |
| go GOMAXPROCS=1 | mix 80/10/10 | 79026 | 797µs | 1511µs | 9.9s | 16.2 MiB | 4 |

| server | startup | idle RSS | RSS with 5000 idle conns | per conn |
|---|---:|---:|---:|---:|
| squark | 3.3ms | 2.9 MiB | 4.9 MiB | -0.8 KiB |
| go | 5.4ms | 9.0 MiB | 111.4 MiB | 18.1 KiB |
| go GOMAXPROCS=1 | 12.4ms | 8.8 MiB | 105.9 MiB | 18.4 KiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 107 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 71 KiB |
| gocompute | 3.7 MiB |
