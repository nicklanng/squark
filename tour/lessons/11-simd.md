# SIMD

clang already vectorizes plain loops over slices: squark emits a second copy of a simple counting
loop without bounds checks, which clang can turn into SIMD instructions. Some things clang can't
vectorize, such as searches that stop early. For those, squark has SIMD types.

## Lanes

`i32x4` is four `i32`s that operators work on all at once: NEON on arm64, SSE or AVX on x86-64.
There's one such type for every number type, 128 and 256 bits wide (`u8x16`, `f32x8`, `f64x4`, …).

- `i32x4(7)` puts 7 in every lane, and `i32x4(xs[i:])` loads four elements
- comparisons give a **mask** (-1 where true, 0 where false), which you can test with `.any()`,
  `.all()` and `.first()`, or use to pick lanes with `.select(a, b)`
- `.sum()`, `.min()` and `.max()` work across the lanes

```squark editor
program

fn main() {
	a := i32x4{1, 2, 3, 4}
	b := i32x4(10)
	c := a*b + 1
	println(c)

	big := c > i32x4(25)
	println(big, big.any(), big.all(), big.first())
	println(big.select(c, i32x4(0)))
	println(c.sum(), c.max())
	println(a.swizzle(3, 2, 1, 0))
}
```

## A search that stops early

Here is a byte search that tests 16 bytes per step: load them, compare all 16 with the byte you're
looking for, and stop at the first block where any lane matches. The scalar loop at the end
handles the last few bytes.

`strings.index_byte` in the standard library is written this way.

```squark editor
program

// index_of returns the index of the first c in s, or -1, testing 16 bytes at a time.
fn index_of(s str, c u8) -> int {
	needle := u8x16(c)
	i := 0
	for ; i+16 <= len(s); i += 16 {
		hits := u8x16(s[i:]) == needle
		if hits.any() {
			return i + hits.first()
		}
	}
	for ; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

fn main() {
	text := "every byte a squark program can use is accounted for at compile time"
	println(index_of(text, 'q'))
	println(index_of(text, '!'))
	println(index_of(text, 'l'))
}
```
