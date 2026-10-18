# Onward

## Putting it together

This program counts the words in a text and prints the five most common, using most of the tour:

- a map with a declared capacity
- views into a constant string, so nothing is copied
- a vector of entries
- `sort.slice` with a named comparison function
- a buffer the caller provides, for the output

Run it and look at the memory map: the main task's proven stack holds the map, the vector
and the buffer. There's nothing else.

```squark editor
program

use os, sort, strconv, strings

const text = "a reference may not outlive the memory it points to and the memory a program " +
	"may use is known before it runs so the program can say how much memory it needs and " +
	"the build can refuse a program that may need more memory than it may have in memory"

struct Entry {
	word  str
	count int
}

fn by_count(a, b Entry) -> bool {
	if a.count != b.count {
		return a.count > b.count
	}
	return a.word < b.word
}

fn main() {
	var counts int[str; 128]
	rest := text
	while len(rest) > 0 {
		word, after, _ := strings.cut(rest, " ")
		counts[word]++ // word is a view of text: static, so the map may keep it
		rest = after
	}

	var entries Entry[..128]
	for w, n in counts {
		entries.push(Entry{w, n})
	}
	sort.slice(entries[:], by_count)

	var buf u8[256]
	out := buf[:0]
	for e in entries[:5] {
		out = strings.append_pad_right(out, e.word, 10, ' ')
		out = strconv.append_int(out, e.count)
		out = append(out, '\n')
	}
	os.stdout.write(out)
	println(len(counts), "distinct words")
}
```

## Where next

That's the tour. You've seen squark's main idea: memory you can see. Values live in static memory,
on a proven stack, or in an arena that you name, and the compiler checks every reference against
the memory it points to.

To go further:

- **The language reference**, `docs/spec.md`, is precise and short. Its table of errors maps each
  compiler message to the usual rewrite.
- **The standard library**, `docs/stdlib.md`, lists every function with its provenance. Run
  `squark doc std` for the same list.
- **The examples**: `examples/notes` is an HTTP server with SQLite, `examples/pipeline` is a worker
  pool, and `examples/shop` is two services with a Dockerfile and Kubernetes manifests.
- **Your own program**: `squark run dir` builds and runs it, and `squark build -v` prints the same
  memory map you've seen here.

The editor on the right is yours. Write anything you like, and the compiler is still running
behind it.

```squark editor
program

fn main() {
	println("bye for now")
}
```
