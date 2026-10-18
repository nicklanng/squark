# Doing without

Some things you'd reach for in other languages don't exist in squark, each for a reason. This
chapter shows what to write instead.

## No recursion
goal: prints "region size 30"

The compiler proves the size of every task's stack, so the call graph can't have cycles: a
recursive function could go arbitrarily deep. Any recursion can become a loop with an explicit
stack. Pick a fixed capacity for that stack, and its memory shows up on the map like everything
else.

This flood fill counts the open cells reachable from one corner. Read the error, then rewrite `fill`
with a loop and a `Pos[..256]` stack of cells still to visit.

```squark editor
program

const w = 10
const h = 5
const grid = str[h]{
	"..#.......",
	".##..#.##.",
	"#...##..#.",
	"..#.....#.",
	"..#..####.",
}

struct Pos {
	x, y int
}

thread seen bool[w][h]

// fill counts the open cells reachable from (x, y).
fn fill(x, y int) -> int {
	if x < 0 or y < 0 or x >= w or y >= h {
		return 0
	}
	if seen[y][x] or grid[y][x] == '#' {
		return 0
	}
	seen[y][x] = true
	return 1 + fill(x+1, y) + fill(x-1, y) + fill(x, y+1) + fill(x, y-1)
}

fn main() {
	println("region size", fill(9, 4))
}
```

```squark solution
program

const w = 10
const h = 5
const grid = str[h]{
	"..#.......",
	".##..#.##.",
	"#...##..#.",
	"..#.....#.",
	"..#..####.",
}

struct Pos {
	x, y int
}

thread seen bool[w][h]

// fill counts the open cells reachable from (x, y).
fn fill(x, y int) -> int {
	var todo Pos[..256] // cells still to visit
	todo.push(Pos{x, y})
	count := 0
	while len(todo) > 0 {
		p := todo.pop()
		if p.x < 0 or p.y < 0 or p.x >= w or p.y >= h {
			continue
		}
		if seen[p.y][p.x] or grid[p.y][p.x] == '#' {
			continue
		}
		seen[p.y][p.x] = true
		count++
		todo.push(Pos{p.x + 1, p.y})
		todo.push(Pos{p.x - 1, p.y})
		todo.push(Pos{p.x, p.y + 1})
		todo.push(Pos{p.x, p.y - 1})
	}
	return count
}

fn main() {
	println("region size", fill(9, 4))
}
```

## anytype instead of generics

squark has no interfaces and no generic types. A parameter can be `anytype`, though: the compiler
makes a copy of the function for each set of argument types it's called with, as Zig does. Nothing
is boxed and nothing happens at run time. Each copy is checked and compiled like code written by
hand.

`sort.slice` works this way. It sorts any slice by any `less` function that fits.

```squark editor
program

use sort

struct Entry {
	word  str
	count int
}

fn by_count(a, b Entry) -> bool {
	return a.count > b.count
}

// count_if works for any slice and any predicate on its elements.
fn count_if(xs anytype, keep anytype) -> int {
	n := 0
	for x in xs {
		if keep(x) {
			n++
		}
	}
	return n
}

fn is_long(s str) -> bool { return len(s) > 4 }
fn is_even(n int) -> bool { return n%2 == 0 }

fn main() {
	entries := Entry[]{{"stack", 3}, {"arena", 7}, {"heap", 0}, {"static", 5}}
	sort.slice(entries, by_count)
	for e in entries {
		println(e.word, e.count)
	}
	println("long words:", count_if(str[]{"no", "heap", "arena", "static"}, is_long))
	println("even numbers:", count_if(int[]{1, 2, 3, 4, 6}, is_even))
}
```

> If you pass a type that doesn't fit, such as an `int[]` with `is_long`, the error points at the
> line in `count_if` that fails and names the instance: "in main.count_if with xs int[] …".
