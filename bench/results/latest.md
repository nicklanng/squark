# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-14T23:36:08+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| sieve 50M | 0.15s | 0.14s | 52.6 MiB | 49.0 MiB |
| nbody 50M | 1.52s | 1.13s | 4.2 MiB | 1.3 MiB |
| wordfreq 100MiB | 0.46s | 0.32s | 205.9 MiB | 7.7 MiB |
| json encode 2M | 2.64s | 0.55s | 4.8 MiB | 1.3 MiB |

## Notes server

| server | scenario | req/s | p50 | p99 | server CPU | peak RSS | threads |
|---|---|---:|---:|---:|---:|---:|---:|
| squark | GET /health | 224107 | 280µs | 608µs | 9.5s | 6.9 MiB | 4 |
| squark | GET /notes/1 | 194146 | 325µs | 447µs | 9.9s | 6.9 MiB | 4 |
| squark | mix 80/10/10 | 140915 | 443µs | 629µs | 9.8s | 8.8 MiB | 4 |
| go | GET /health | 176664 | 333µs | 1118µs | 43.8s | 21.7 MiB | 19 |
| go | GET /notes/1 | 107277 | 176µs | 2335µs | 42.9s | 21.8 MiB | 19 |
| go | mix 80/10/10 | 81440 | 1022µs | 2318µs | 35.9s | 23.3 MiB | 19 |
| go GOMAXPROCS=1 | GET /health | 150836 | 419µs | 793µs | 10.0s | 15.1 MiB | 4 |
| go GOMAXPROCS=1 | GET /notes/1 | 116890 | 543µs | 977µs | 10.0s | 15.2 MiB | 4 |
| go GOMAXPROCS=1 | mix 80/10/10 | 79403 | 793µs | 1511µs | 9.9s | 16.3 MiB | 4 |

| server | startup | idle RSS | RSS with 5000 idle conns | per conn |
|---|---:|---:|---:|---:|
| squark | 3.5ms | 2.9 MiB | 4.8 MiB | -0.8 KiB |
| go | 5.6ms | 9.2 MiB | 113.2 MiB | 18.4 KiB |
| go GOMAXPROCS=1 | 5.8ms | 8.4 MiB | 106.4 MiB | 18.5 KiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 129 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 91 KiB |
| gocompute | 3.7 MiB |
