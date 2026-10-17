# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-17T11:59:41+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator (sq-load) on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| sieve 50M | 0.16s | 0.15s | 52.6 MiB | 49.0 MiB |
| nbody 50M | 1.55s | 1.13s | 4.2 MiB | 1.3 MiB |
| wordfreq 100MiB | 0.47s | 0.32s | 205.9 MiB | 7.7 MiB |
| json encode 2M | 2.65s | 0.57s | 4.8 MiB | 1.3 MiB |
| kernel saxpy | 0.17s | 34.3ms | 4.7 MiB | 1.7 MiB |
| kernel method-axpy | 0.25s | 33.6ms | 4.6 MiB | 1.7 MiB |
| kernel dot-int32 | 0.20s | 34.0ms | 4.7 MiB | 1.7 MiB |
| kernel sum-float64 | 0.41s | 0.36s | 4.6 MiB | 1.7 MiB |
| kernel sum-float64/simd | 0.41s | 63.6ms | 4.6 MiB | 1.7 MiB |
| kernel dot-float32 | 0.57s | 0.42s | 4.6 MiB | 1.7 MiB |
| kernel dot-float32/simd | 0.56s | 53.9ms | 4.7 MiB | 1.7 MiB |
| kernel count-byte | 0.18s | 95.4ms | 4.5 MiB | 1.7 MiB |
| kernel max-int32 | 0.20s | 25.4ms | 4.9 MiB | 1.9 MiB |
| kernel index-byte | 0.18s | 0.17s | 4.6 MiB | 1.7 MiB |
| kernel index-byte/simd | 0.17s | 25.8ms | 4.7 MiB | 1.8 MiB |
| kernel hex | 0.34s | 0.22s | 4.6 MiB | 1.7 MiB |
| kernel hex/simd | 0.34s | 60.8ms | 4.6 MiB | 1.8 MiB |
| kernel strings-index | 0.25s | 24.5ms | 10.8 MiB | 1.7 MiB |
| kernel json-string | 0.31s | 28.0ms | 11.8 MiB | 1.7 MiB |
| kernel json-decode | 1.46s | 40.6ms | 11.1 MiB | 1.7 MiB |
| kernel xor-bytes | 0.25s | 11.1ms | 4.6 MiB | 1.7 MiB |
| kernel add-rows | 0.36s | 0.20s | 4.7 MiB | 1.7 MiB |
| kernel brighten | 0.22s | 0.13s | 4.7 MiB | 1.7 MiB |

## Notes server

| server | scenario | req/s | p50 | p99 | server CPU | peak RSS | threads |
|---|---|---:|---:|---:|---:|---:|---:|
| squark | GET /health | 219616 | 181µs | 1135µs | 9.8s | 6.9 MiB | 4 |
| squark | GET /notes/1 | 188148 | 329µs | 715µs | 9.8s | 6.9 MiB | 4 |
| squark | mix 80/10/10 | 128201 | 476µs | 1053µs | 9.8s | 8.7 MiB | 4 |
| go | GET /health | 183799 | 290µs | 1085µs | 49.9s | 20.8 MiB | 17 |
| go | GET /notes/1 | 106796 | 168µs | 2499µs | 45.5s | 21.0 MiB | 18 |
| go | mix 80/10/10 | 72201 | 1110µs | 2548µs | 36.7s | 22.3 MiB | 18 |
| go GOMAXPROCS=1 | GET /health | 121500 | 501µs | 1266µs | 9.8s | 15.0 MiB | 4 |
| go GOMAXPROCS=1 | GET /notes/1 | 82541 | 735µs | 2060µs | 9.7s | 15.1 MiB | 4 |
| go GOMAXPROCS=1 | mix 80/10/10 | 65409 | 961µs | 1946µs | 9.9s | 16.1 MiB | 4 |

| server | startup | idle RSS | RSS with 5000 idle conns | per conn |
|---|---:|---:|---:|---:|
| squark | 3.0ms | 3.0 MiB | 4.8 MiB | -0.8 KiB |
| go | 5.3ms | 9.2 MiB | 110.2 MiB | 18.0 KiB |
| go GOMAXPROCS=1 | 6.0ms | 8.7 MiB | 105.4 MiB | 18.3 KiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 168 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 70 KiB |
| gocompute | 3.7 MiB |
