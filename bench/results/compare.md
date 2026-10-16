# Loop kernels: main vs working tree

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-17T00:28:01+01:00, best of 3

| kernel | main | working tree | speedup |
|---|---:|---:|---:|
| saxpy | 0.35s | 34.9ms | 10.0× |
| method-axpy | 0.35s | 34.0ms | 10.2× |
| dot-int32 | 33.8ms | 34.0ms | 1.0× |
| sum-float64 | 0.36s | 0.36s | 1.0× |
| sum-float64/simd | 0.36s | 59.1ms | 6.0× |
| dot-float32 | 0.36s | 0.36s | 1.0× |
| dot-float32/simd | 0.36s | 52.0ms | 6.9× |
| count-byte | 83.9ms | 84.1ms | 1.0× |
| max-int32 | 23.6ms | 23.6ms | 1.0× |
| index-byte | 0.17s | 0.17s | 1.0× |
| index-byte/simd | 0.17s | 24.4ms | 6.9× |
| strings-index | 0.19s | 24.4ms | 7.8× |
| json-string | 0.50s | 28.3ms | 17.7× |
| xor-bytes | 0.46s | 11.3ms | 40.8× |
| add-rows | 0.46s | 0.20s | 2.3× |
| brighten | 0.30s | 0.13s | 2.3× |
