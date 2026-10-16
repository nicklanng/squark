# Loop kernels: main vs working tree

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-16T23:11:06+01:00, best of 3

| kernel | main | working tree | speedup |
|---|---:|---:|---:|
| saxpy | 0.37s | 35.0ms | 10.5× |
| dot-int32 | 34.5ms | 34.6ms | 1.0× |
| sum-float64 | 0.36s | 0.36s | 1.0× |
| sum-float64/simd | 0.36s | 60.8ms | 5.9× |
| dot-float32 | 0.36s | 0.36s | 1.0× |
| dot-float32/simd | 0.36s | 53.9ms | 6.7× |
| count-byte | 84.9ms | 85.0ms | 1.0× |
| max-int32 | 24.4ms | 24.3ms | 1.0× |
| index-byte | 0.17s | 0.17s | 1.0× |
| index-byte/simd | 0.17s | 24.9ms | 6.9× |
| xor-bytes | 0.48s | 11.6ms | 41.2× |
| add-rows | 0.48s | 0.21s | 2.4× |
| brighten | 0.31s | 0.13s | 2.4× |
