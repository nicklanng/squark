# Loop kernels: main vs working tree

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-17T00:07:10+01:00, best of 3

| kernel | main | working tree | speedup |
|---|---:|---:|---:|
| saxpy | 0.46s | 44.1ms | 10.4× |
| dot-int32 | 44.7ms | 47.4ms | 0.9× |
| sum-float64 | 0.38s | 0.41s | 0.9× |
| sum-float64/simd | 0.38s | 64.8ms | 5.9× |
| dot-float32 | 0.39s | 0.39s | 1.0× |
| dot-float32/simd | 0.39s | 58.2ms | 6.8× |
| count-byte | 97.0ms | 92.2ms | 1.1× |
| max-int32 | 28.8ms | 28.9ms | 1.0× |
| index-byte | 0.20s | 0.20s | 1.0× |
| index-byte/simd | 0.20s | 29.7ms | 6.9× |
| strings-index | 0.23s | 29.8ms | 7.7× |
| json-string | 0.57s | 34.7ms | 16.3× |
| xor-bytes | 0.54s | 12.8ms | 42.1× |
| add-rows | 0.55s | 0.24s | 2.3× |
| brighten | 0.35s | 0.15s | 2.3× |
