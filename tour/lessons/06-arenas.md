# Arenas

Fixed capacities cover a lot, but sometimes a size is only known at run time: a request body, a
list of words, a tree. An **arena** is a block of memory that you name. Allocations come out of it
with a pointer bump, and all of them are freed together when the arena goes away. There is no
garbage collector, no `free` and no leak.

## with arena

`with arena scratch 64K { ... }` reserves 64 KiB in the current stack frame for the length of the
block. Inside the block, `x in scratch` says where an expression's allocations come from:

- `new(T) in a` gives a zeroed `*T`
- `make(T[], n) in a` takes a size known only at run time
- `a + b in a` concatenates strings
- `str(b) in a` copies bytes into a string that owns them

Outside an arena, each of these is a compile error that suggests adding one. Run the program, then
change `64K` to `256K`. The arena is part of `report`'s frame, so the memory map shows the main
task's stack grow with it.

```squark editor
program

fn report(name str, n int) {
	with arena scratch 64K {
		squares := make(int[], n) in scratch // n is only known at run time
		for i in 0..n {
			squares[i] = i * i
		}
		title := "report for " + name in scratch
		println(title, "-", len(squares), "squares, the last is", squares[n-1])
		println("arena:", scratch.used(), "bytes used of", scratch.cap())
	} // everything allocated in scratch is gone here
}

fn main() {
	report("squark", 1000)
}
```

## Arenas as parameters

A function can take an `arena` and allocate in it. Its results then live as long as the caller's
arena does, which is how a function returns a new data structure in squark.

This builds a binary tree. squark doesn't allow recursion, because recursion would make stack
sizes unprovable, so `build` and `count` walk the tree with an explicit stack: a vector of
pointers.

```squark editor
program

struct Node {
	depth int
	kids  *Node[]
}

// build allocates a full binary tree of the given depth in the caller's arena.
fn build(a arena, depth int) -> *Node {
	root := new(Node) in a
	var todo (*Node)[..64]
	todo.push(root)
	while len(todo) > 0 {
		n := todo.pop()
		if n.depth == depth {
			continue
		}
		for _ in 0..2 {
			kid := new(Node) in a
			kid.depth = n.depth + 1
			n.kids = append(n.kids, kid) in a
			todo.push(kid)
		}
	}
	return root
}

fn count(root *Node) -> int {
	total := 0
	var todo (*Node)[..64]
	todo.push(root)
	while len(todo) > 0 {
		n := todo.pop()
		total++
		for k in n.kids {
			todo.push(k)
		}
	}
	return total
}

fn main() {
	with arena tree 64K {
		root := build(tree, 6)
		println("nodes:", count(root), "arena used:", tree.used(), "bytes")
	}
}
```

## When an arena is full
goal: prints "kept 256 of 1000"

A full arena panics and names itself, so an undersized arena fails loudly. When running out is
expected, for example with input from the network, write the allocation as
`p, ok := new(T) in a`. Then `ok` is false when the arena can't hold it, and nothing panics.

This program tries to keep 1000 readings in a 4 KiB arena. Run it to see the panic, then use the
`ok` form to keep as many readings as fit.

```squark editor
program

struct Reading {
	value int
	next  *Reading
}

fn main() {
	with arena a 4K {
		var head *Reading
		kept := 0
		for i in 0..1000 {
			r := new(Reading) in a
			r.value = i
			r.next = head
			head = r
			kept++
		}
		println("kept", kept, "of 1000")
	}
}
```

```squark solution
program

struct Reading {
	value int
	next  *Reading
}

fn main() {
	with arena a 4K {
		var head *Reading
		kept := 0
		for i in 0..1000 {
			r, ok := new(Reading) in a
			if not ok {
				break
			}
			r.value = i
			r.next = head
			head = r
			kept++
		}
		println("kept", kept, "of 1000")
	}
}
```

## Package arenas
goal: prints "longest: squark"

Package variables live for the whole program, so they may only point to memory that does too:
constants, other package variables, or a **package arena**. `arena names 4K` at the top level is
static memory that only ever grows. It is useful for data that is kept forever, such as interned
strings or configuration.

`consider` keeps the longest word it has seen in `longest`, but the word is a view of `main`'s
`buf`, which is overwritten on every iteration. Copy the word into `names` so it can be kept.

```squark editor
program

use strings

thread longest str // a package variable: it lives as long as the program
arena names 4K     // a package arena: static memory, never freed

fn consider(word str) {
	w := strings.trim_space(word)
	if len(w) > len(longest) {
		longest = w
	}
}

fn main() {
	var buf u8[64]
	for word in str[]{"  arena ", " heap", "  squark  ", "no"} {
		n := copy(buf[:], word)
		consider(str(buf[:n]))
	}
	println("longest:", longest)
}
```

```squark solution
program

use strings

thread longest str
arena names 4K

fn consider(word str) {
	w := strings.trim_space(word)
	if len(w) > len(longest) {
		longest = str(w) in names // a copy that lives forever
	}
}

fn main() {
	var buf u8[64]
	for word in str[]{"  arena ", " heap", "  squark  ", "no"} {
		n := copy(buf[:], word)
		consider(str(buf[:n]))
	}
	println("longest:", longest)
}
```

> `thread` before a package variable means each scheduler thread has its own copy. In a program
> with one thread, which is the default, it's simply a package variable. The Memory budgets chapter
> covers threads.
