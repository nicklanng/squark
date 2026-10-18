# Storage and views

squark has no heap, so anything that holds a variable number of things has its capacity in its
type. A size in the brackets means **storage**: the elements themselves, inline, wherever the
variable lives. Empty brackets mean a **view**: a reference to storage somewhere else.

| storage (the elements, inline) | view (a reference) |
|---|---|
| `T[N]`, an array | `T[]`, a slice |
| `T[..N]`, a vector: up to N, with a length | `T[]` |
| `V[K; N]`, a map with room for N | `V[K]` |
| `T chan[N]`, a channel with an N-element buffer | `T chan` |

## Arrays

`T[N]` is an array of N elements. The array is storage: declaring it is the allocation, its zero
value is ready to use, and assigning it copies every element.

`T[...]{...}` counts the elements for you.

```squark editor
program

fn main() {
	var a int[4]
	a[0] = 7
	b := int[...]{1, 2, 3}
	c := a // copies all four ints
	c[0] = 100
	println(a[0], c[0], len(b))
	for i, x in b {
		println(i, x)
	}
}
```

## Slices are views

`T[]` is a slice: a view of storage that lives somewhere else, made of a pointer, a length and a
capacity. Slicing an array (`a[1:3]`) makes a view of it, and writes through the view change the
array.

`append` writes into the view's spare capacity. **It never allocates.** An `append` past the
capacity panics, because there is nowhere to grow into (the Arenas chapter shows one way to give it
room).

```squark editor
program

fn main() {
	var a int[6]
	s := a[1:3]
	s[0] = 42
	println("a[1] is", a[1], "len", len(s), "cap", cap(s))

	t := a[:0]
	for i in 0..6 {
		t = append(t, i*i) // fills a's storage, in place
	}
	println(a[5], len(t))
}
```

## Vectors

`T[..N]` is a **vector**: storage for up to N elements, plus a length. It's the squark answer to
"a slice I keep appending to", with its capacity in the type.

- `v.push(x)` adds an element, and `v.pop()` removes the last one
- `v[:]` is a view of the elements in use
- `len(v)` changes and `cap(v)` is always N

A vector holds its elements inline, so this one is 8 × 8 bytes on `main`'s stack.

```squark editor
program

fn sum(xs int[]) -> int {
	total := 0
	for x in xs {
		total += x
	}
	return total
}

fn main() {
	var v int[..8]
	for i in 1..6 {
		v.push(i * 10)
	}
	last := v.pop()
	println("popped", last, "len", len(v), "cap", cap(v))
	println("sum", sum(v[:]))
}
```

## Maps

`V[K; N]` is a map from K to V with room for N entries. Declaring it is the allocation, so there
is no `make`. A function that takes a map takes the view `V[K]`, which works with a map of any
capacity.

Map storage can't be copied, because it points into itself, so pass the view.

```squark editor
program

fn tally(counts int[str], words str[]) {
	for w in words {
		counts[w]++
	}
}

fn main() {
	var counts int[str; 16]
	tally(counts, str[]{"to", "be", "or", "not", "to", "be"})
	println("distinct:", len(counts), "of", cap(counts))
	println("to:", counts["to"], "be:", counts["be"], "or:", counts["or"])
	if _, ok := counts["question"]; not ok {
		println("no question")
	}
}
```

## When storage is full
goal: prints "kept 4 dropped 2"

Capacity in the type is a promise the program has to keep. Pushing a fifth element onto an
`int[..4]` panics. Run this to see how: the panic names the problem, and the exit status is 2.

Sometimes dropping data is right: a queue that sheds load, or a sample that keeps the first N
values. `v.try_push(x)` returns `false` when the vector is full and pushes nothing. Use it so the
program keeps 4 readings and counts the 2 it drops.

```squark editor
program

fn main() {
	var readings int[..4]
	dropped := 0
	for r in int[]{12, 15, 11, 19, 14, 13} {
		readings.push(r)
	}
	println("kept", len(readings), "dropped", dropped)
}
```

```squark solution
program

fn main() {
	var readings int[..4]
	dropped := 0
	for r in int[]{12, 15, 11, 19, 14, 13} {
		if not readings.try_push(r) {
			dropped++
		}
	}
	println("kept", len(readings), "dropped", dropped)
}
```

## Strings are views too

A `str` is an immutable view of bytes. Slicing a string copies nothing, and neither do the
`strings` functions: `strings.cut` returns views into its argument.

`str(b)` turns bytes into a string **without copying**, so the string reads whatever is in the
buffer. Here a change to the buffer shows up in the string. That is the price of not copying, and
the next chapter shows how the compiler keeps it safe.

```squark editor
program

use strings

fn main() {
	key, value, ok := strings.cut("name=squark", "=")
	println(key, value, ok)

	var buf u8[5]
	copy(buf[:], "hello")
	s := str(buf[:])
	println(s)
	buf[0] = 'j'
	println(s) // the same view, of changed bytes
}
```

## Callers provide buffers

A squark function can't create a new slice to return, because there is nowhere to put it. The usual
pattern is for the caller to pass storage in and the function to return a view of it. The standard
library works this way: `strconv.append_int(dst, n)` writes digits into `dst`.

`-> int[] in out` says that the result points into `out`. You don't need to write it, because the
compiler works it out, but written down it's a contract the body is checked against.

```squark editor
program

use os, strconv

fn squares(n int, out int[]) -> int[] in out {
	out = out[:0]
	for i in 1..n+1 {
		out = append(out, i*i)
	}
	return out
}

fn main() {
	var storage int[10]
	sq := squares(10, storage[:])

	var buf u8[128]
	line := buf[:0]
	for x in sq {
		line = strconv.append_int(line, x)
		line = append(line, ' ')
	}
	line = append(line, '\n')
	os.stdout.write(line)
}
```
