# Loop kernels: main vs working tree

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-17T00:30:18+01:00, best of 3

| kernel | main | working tree | speedup |
|---|---:|---:|---:|
| saxpy | 0.35s | 35.1ms | 9.9× |
| method-axpy | 0.35s | 33.8ms | 10.3× |
| dot-int32 | 33.6ms | 33.9ms | 1.0× |
| sum-float64 | 0.35s | 0.35s | 1.0× |
| sum-float64/simd | 0.35s | 59.6ms | 5.9× |
| dot-float32 | 0.36s | 0.36s | 1.0× |
| dot-float32/simd | 0.36s | 52.4ms | 6.9× |
| count-byte | 83.7ms | 83.9ms | 1.0× |
| max-int32 | 23.5ms | 23.6ms | 1.0× |
| index-byte | 0.17s | 0.17s | 1.0× |
| index-byte/simd | 0.17s | 24.2ms | 7.0× |
| strings-index | 0.19s | 24.4ms | 7.8× |
| json-string | 0.50s | 28.0ms | 17.9× |
| json-decode | 0.92s | 40.5ms | 22.6× |
| xor-bytes | 0.46s | 11.1ms | 41.8× |
| add-rows | 0.46s | 0.20s | 2.3× |
| brighten | 0.34s | 0.13s | 2.6× |
