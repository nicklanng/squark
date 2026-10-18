# Lifetimes

This chapter covers squark's central rule: **a reference may not outlive the memory it points
to**. A language with a heap can move a variable there when a pointer to it might outlive it.
squark has no heap to move it to, so the compiler proves every reference safe or rejects the
program, and the error says where the memory should come from instead.

## Where a heap would come in
goal: rejects "cannot return reference to variable"

`greet` writes into a buffer that its **caller** owns, and returns a view of what it wrote. That's
the squark way. With a heap, `greet` could declare the buffer itself and return it, and the
buffer would move to the heap.

Write it that way: declare `var buf u8[64]` inside `greet`, drop the `out` parameter, and return a
view of `buf`. The checker rejects it before you press Run, and its message tells you what to do
instead.

```squark editor
program

use os

// greet writes a greeting into the caller's buffer and returns the part it wrote.
fn greet(name str, out u8[]) -> u8[] in out {
	out = append(out[:0], "hello, "...)
	out = append(out, name...)
	out = append(out, '\n')
	return out
}

fn main() {
	var buf u8[64]
	os.stdout.write(greet("squark", buf[:]))
}
```

```squark solution
program

use os

fn greet(name str) -> u8[] {
	var buf u8[64]
	out := append(buf[:0], "hello, "...)
	out = append(out, name...)
	out = append(out, '\n')
	return out
}

fn main() {
	os.stdout.write(greet("squark"))
}
```

## Storage must outlive its views
goal: prints "event 2"

The checker follows references through function calls as well. It works out what every function
does with its arguments. Here it knows that `log.add` keeps its argument, so the string passed in
must live as long as `log` does.

Each `buf` lives for one iteration of the loop, but `log` keeps a view of it for longer. Read the
error, then fix it. Every line needs storage of its own that outlives `log`. If you reuse a single
buffer instead, every line in the log becomes a view of the same bytes.

```squark editor
program

use strconv

struct Log {
	lines str[..8]
}

impl Log {
	fn add(&self, s str) {
		self.lines.push(s)
	}
}

fn main() {
	var log Log
	for i in 0..3 {
		var buf u8[32]
		line := strconv.append_int(append(buf[:0], "event "...), i)
		log.add(str(line))
	}
	for l in log.lines {
		println(l)
	}
}
```

```squark solution
program

use strconv

struct Log {
	lines str[..8]
}

impl Log {
	fn add(&self, s str) {
		self.lines.push(s)
	}
}

fn main() {
	var bufs u8[32][3] // three buffers of 32 bytes, living as long as main
	var log Log
	for i in 0..3 {
		line := strconv.append_int(append(bufs[i][:0], "event "...), i)
		log.add(str(line))
	}
	for l in log.lines {
		println(l)
	}
}
```

## Provenance you can write down
goal: rejects "its signature does not say so"

The compiler infers, for every function, what its results point into and which arguments it
stores where. That's how the last lesson's error knew what `log.add` does. You can also write it
down: `-> str in a, b` says that the result may point into `a` or into `b`.

A declared contract is checked. If you later change the body so a reference escapes some other way,
the build fails instead of quietly widening what callers must guarantee. Remove `, b` from the
signature and see.

```squark editor
program

// longer returns the longer of a and b: a view of one of them, never a copy.
fn longer(a, b str) -> str in a, b {
	if len(b) > len(a) {
		return b
	}
	return a
}

fn main() {
	println(longer("heap", "no heap"))
}
```

```squark solution
program

fn longer(a, b str) -> str in a {
	if len(b) > len(a) {
		return b
	}
	return a
}

fn main() {
	println(longer("heap", "no heap"))
}
```

> `squark doc` prints every function with the provenance it inferred, so you can read a library's
> contracts without reading its bodies.
