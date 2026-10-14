# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-14T17:39:45+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| sieve 50M | 0.15s | 0.14s | 52.5 MiB | 48.9 MiB |
| nbody 50M | 1.52s | 1.12s | 4.2 MiB | 1.2 MiB |
| wordfreq 100MiB | 0.45s | 0.31s | 205.9 MiB | 7.5 MiB |
| json encode 2M | 2.66s | 0.56s | 4.7 MiB | 1.2 MiB |

## Notes server

| server | scenario | req/s | p50 | p99 | server CPU | peak RSS | threads |
|---|---|---:|---:|---:|---:|---:|---:|
| squark | GET /health | 226577 | 275µs | 543µs | 9.6s | 7.0 MiB | 1 |
| squark | GET /notes/1 | 201890 | 310µs | 466µs | 9.8s | 7.0 MiB | 1 |
| squark | mix 80/10/10 | 152594 | 411µs | 625µs | 9.8s | 9.0 MiB | 1 |
| go | GET /health | 179443 | 318µs | 1200µs | 41.2s | 21.7 MiB | 31 |
| go | GET /notes/1 | 109820 | 173µs | 2302µs | 42.2s | 22.3 MiB | 31 |
| go | mix 80/10/10 | 81992 | 1010µs | 2367µs | 35.9s | 23.7 MiB | 31 |
| go GOMAXPROCS=1 | GET /health | 155831 | 407µs | 764µs | 10.0s | 14.7 MiB | 4 |
| go GOMAXPROCS=1 | GET /notes/1 | 118858 | 535µs | 965µs | 10.0s | 14.8 MiB | 4 |
| go GOMAXPROCS=1 | mix 80/10/10 | 79712 | 793µs | 1495µs | 9.9s | 16.0 MiB | 4 |

| server | startup | idle RSS | RSS with 5000 idle conns | per conn |
|---|---:|---:|---:|---:|
| squark | 3.3ms | 3.1 MiB | 5.0 MiB | -0.8 KiB |
| go | 6.9ms | 9.3 MiB | 112.3 MiB | 18.1 KiB |
| go GOMAXPROCS=1 | 5.6ms | 8.6 MiB | 105.8 MiB | 18.4 KiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 108 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 71 KiB |
| gocompute | 3.7 MiB |
