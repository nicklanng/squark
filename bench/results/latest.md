# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-14T23:51:02+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| sieve 50M | 0.15s | 0.14s | 52.5 MiB | 49.0 MiB |
| nbody 50M | 1.52s | 1.12s | 4.2 MiB | 1.3 MiB |
| wordfreq 100MiB | 0.46s | 0.32s | 205.8 MiB | 7.7 MiB |
| json encode 2M | 2.68s | 0.55s | 4.7 MiB | 1.3 MiB |

## Notes server

| server | scenario | req/s | p50 | p99 | server CPU | peak RSS | threads |
|---|---|---:|---:|---:|---:|---:|---:|
| squark | GET /health | 223823 | 280µs | 612µs | 9.5s | 6.8 MiB | 4 |
| squark | GET /notes/1 | 196938 | 318µs | 543µs | 9.8s | 6.8 MiB | 4 |
| squark | mix 80/10/10 | 143586 | 435µs | 629µs | 9.8s | 8.7 MiB | 4 |
| go | GET /health | 178301 | 331µs | 1094µs | 43.9s | 21.7 MiB | 20 |
| go | GET /notes/1 | 108610 | 177µs | 2286µs | 42.9s | 21.8 MiB | 20 |
| go | mix 80/10/10 | 82904 | 1014µs | 2302µs | 36.1s | 23.3 MiB | 20 |
| go GOMAXPROCS=1 | GET /health | 154180 | 411µs | 772µs | 10.0s | 14.7 MiB | 4 |
| go GOMAXPROCS=1 | GET /notes/1 | 118899 | 535µs | 961µs | 10.0s | 14.8 MiB | 4 |
| go GOMAXPROCS=1 | mix 80/10/10 | 80458 | 784µs | 1470µs | 9.9s | 16.0 MiB | 4 |

| server | startup | idle RSS | RSS with 5000 idle conns | per conn |
|---|---:|---:|---:|---:|
| squark | 3.2ms | 2.9 MiB | 4.7 MiB | -0.8 KiB |
| go | 5.9ms | 9.3 MiB | 111.6 MiB | 18.1 KiB |
| go GOMAXPROCS=1 | 5.7ms | 8.4 MiB | 107.4 MiB | 18.7 KiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 109 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 70 KiB |
| gocompute | 3.7 MiB |
