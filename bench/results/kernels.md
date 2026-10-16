# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-16T22:46:12+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator (sq-load) on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| kernel saxpy | 0.17s | 34.0ms | 4.6 MiB | 1.7 MiB |
| kernel dot-int32 | 0.20s | 33.6ms | 4.6 MiB | 1.6 MiB |
| kernel sum-float64 | 0.39s | 0.34s | 4.5 MiB | 1.7 MiB |
| kernel count-byte | 0.17s | 83.0ms | 4.5 MiB | 1.7 MiB |
| kernel max-int32 | 0.17s | 23.5ms | 4.5 MiB | 1.6 MiB |
| kernel xor-bytes | 0.25s | 11.0ms | 4.6 MiB | 1.6 MiB |
| kernel add-rows | 0.36s | 0.20s | 4.5 MiB | 1.7 MiB |
| kernel brighten | 0.22s | 0.13s | 4.6 MiB | 1.7 MiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 128 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 70 KiB |
| gocompute | 3.7 MiB |
