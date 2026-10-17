# Loop kernels: main vs working tree

Apple M4, 10 cores, 16 GiB, darwin/arm64, 2026-10-17T11:52:25+01:00, best of 3

| kernel | main | working tree | speedup |
|---|---:|---:|---:|
| saxpy | 34.0ms | 34.4ms | 1.0× |
| method-axpy | 33.4ms | 33.4ms | 1.0× |
| dot-int32 | 33.5ms | 33.5ms | 1.0× |
| sum-float64 | 0.35s | 0.36s | 1.0× |
| sum-float64/simd | 0.35s | 59.2ms | 6.0× |
| dot-float32 | 0.36s | 0.36s | 1.0× |
| dot-float32/simd | 0.36s | 52.2ms | 6.9× |
| count-byte | 84.0ms | 83.8ms | 1.0× |
| max-int32 | 23.5ms | 23.6ms | 1.0× |
| index-byte | 0.17s | 0.17s | 1.0× |
| index-byte/simd | 0.17s | 24.2ms | 7.0× |
| hex | 0.41s | 0.21s | 1.9× |
| hex/simd | 0.41s | 60.5ms | 6.8× |
| strings-index | 24.3ms | 24.5ms | 1.0× |
| json-string | 27.9ms | 27.7ms | 1.0× |
| json-decode | 40.6ms | 40.4ms | 1.0× |
| xor-bytes | 11.0ms | 11.2ms | 1.0× |
| add-rows | 0.20s | 0.20s | 1.0× |
| brighten | 0.13s | 0.13s | 1.0× |
