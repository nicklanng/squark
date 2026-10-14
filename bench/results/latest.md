# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-14T23:16:09+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| sieve 50M | 0.15s | 0.14s | 52.5 MiB | 49.0 MiB |
| nbody 50M | 1.52s | 1.13s | 4.2 MiB | 1.3 MiB |
| wordfreq 100MiB | 0.46s | 0.33s | 207.6 MiB | 7.7 MiB |
| json encode 2M | 2.66s | 0.56s | 4.9 MiB | 1.3 MiB |

## Notes server

| server | scenario | req/s | p50 | p99 | server CPU | peak RSS | threads |
|---|---|---:|---:|---:|---:|---:|---:|
| squark | GET /health | 223593 | 280µs | 567µs | 9.5s | 6.9 MiB | 4 |
| squark | GET /notes/1 | 195483 | 323µs | 466µs | 9.9s | 6.9 MiB | 4 |
| squark | mix 80/10/10 | 143759 | 435µs | 621µs | 9.8s | 8.8 MiB | 4 |
| go | GET /health | 177021 | 331µs | 1126µs | 43.7s | 21.2 MiB | 20 |
| go | GET /notes/1 | 107955 | 179µs | 2318µs | 42.9s | 21.4 MiB | 20 |
| go | mix 80/10/10 | 82110 | 1018µs | 2318µs | 36.2s | 22.9 MiB | 20 |
| go GOMAXPROCS=1 | GET /health | 152025 | 417µs | 788µs | 10.0s | 15.1 MiB | 4 |
| go GOMAXPROCS=1 | GET /notes/1 | 116089 | 547µs | 981µs | 10.0s | 15.2 MiB | 4 |
| go GOMAXPROCS=1 | mix 80/10/10 | 79716 | 793µs | 1479µs | 9.9s | 16.4 MiB | 4 |

| server | startup | idle RSS | RSS with 5000 idle conns | per conn |
|---|---:|---:|---:|---:|
| squark | 3.1ms | 2.9 MiB | 4.8 MiB | -0.8 KiB |
| go | 5.6ms | 9.1 MiB | 111.1 MiB | 18.1 KiB |
| go GOMAXPROCS=1 | 5.5ms | 8.7 MiB | 107.3 MiB | 18.6 KiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 128 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 91 KiB |
| gocompute | 3.7 MiB |
