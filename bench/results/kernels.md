# squark vs Go

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-16T23:32:01+01:00
HTTP: 64 keep-alive connections, 10s per scenario; load generator (sq-load) on the same machine.

## Compute (best of 3)

| benchmark | Go time | squark time | Go peak RSS | squark peak RSS |
|---|---:|---:|---:|---:|
| kernel saxpy | 0.24s | 44.4ms | 4.5 MiB | 1.7 MiB |
| kernel dot-int32 | 0.27s | 43.8ms | 4.6 MiB | 1.7 MiB |
| kernel sum-float64 | 0.45s | 0.38s | 4.6 MiB | 1.7 MiB |
| kernel sum-float64/simd | 0.45s | 65.1ms | 4.7 MiB | 1.7 MiB |
| kernel dot-float32 | 0.60s | 0.40s | 4.6 MiB | 1.7 MiB |
| kernel dot-float32/simd | 0.62s | 60.3ms | 4.6 MiB | 1.7 MiB |
| kernel count-byte | 0.20s | 0.10s | 4.6 MiB | 1.7 MiB |
| kernel max-int32 | 0.21s | 29.7ms | 4.6 MiB | 1.7 MiB |
| kernel index-byte | 0.20s | 0.20s | 4.6 MiB | 1.7 MiB |
| kernel index-byte/simd | 0.21s | 29.7ms | 4.5 MiB | 1.7 MiB |
| kernel xor-bytes | 0.30s | 14.2ms | 4.5 MiB | 1.7 MiB |
| kernel add-rows | 0.44s | 0.26s | 4.5 MiB | 1.7 MiB |
| kernel brighten | 0.28s | 0.16s | 4.5 MiB | 1.7 MiB |

## Binary sizes

| binary | size |
|---|---:|
| notes | 128 KiB |
| gonotes | 9.1 MiB |
| sq-sieve | 70 KiB |
| gocompute | 3.7 MiB |
