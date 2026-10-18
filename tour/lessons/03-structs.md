# Structs and methods

Data and behaviour in squark: structs, methods in `impl` blocks, pointers, named types and
functions as values.

## Structs

`struct Name { ... }` declares a struct. Fields are listed one per line, and several fields of one
type can share a line. A literal names its fields or lists them in order.

Structs are values: assigning one copies it.

```squark editor
program

struct Point {
	x, y int
}

fn main() {
	p := Point{x: 1, y: 2}
	q := Point{3, 4}
	p.x = 10
	println(p.x, p.y, q.x, q.y)

	r := p // a copy
	r.y = 99
	println("p.y is still", p.y, "and r.y is", r.y)
}
```

## Methods
goal: prints "count 3"

Methods go in an `impl` block. A method's first parameter is its receiver, and the receiver comes in
two forms:

- `self` is a **copy** of the value
- `&self` is a **pointer** to it, so the method can change it

This counter never counts. Run it to see `count 0`, then make `inc` change the counter it was
called on.

```squark editor
program

struct Counter {
	name str
	n    int
}

impl Counter {
	fn inc(self) {
		self.n++
	}

	fn get(self) -> int {
		return self.n
	}
}

fn main() {
	c := Counter{name: "count"}
	c.inc()
	c.inc()
	c.inc()
	println(c.name, c.get())
}
```

```squark solution
program

struct Counter {
	name str
	n    int
}

impl Counter {
	fn inc(&self) {
		self.n++
	}

	fn get(self) -> int {
		return self.n
	}
}

fn main() {
	c := Counter{name: "count"}
	c.inc()
	c.inc()
	c.inc()
	println(c.name, c.get())
}
```

## Pointers

`&x` takes an address and `*p` follows it. A field reached through a pointer needs no `*`:
`b.next.val` follows `next` for you. The zero value of a pointer is `nil`.

As in C, a pointer type puts `*` first: `*Node` is a pointer to a `Node`.

```squark editor
program

struct Node {
	val  int
	next *Node
}

fn double(x *int) {
	*x *= 2
}

fn main() {
	n := 21
	double(&n)
	println(n)

	a := Node{val: 1}
	b := Node{val: 2, next: &a}
	println(b.val, "->", b.next.val)

	var p *Node
	println(p == nil)
}
```

> Every pointer here points at a variable in `main`'s frame, and nothing outlives `main`, so all of
> this is fine. The Lifetimes chapter shows what happens when it isn't.

## Named types

`Name: T` gives an existing type a new name. The new type is a type of its own: a `Cents` doesn't
mix with an `int` without a conversion, and it can have methods.

Uncomment the last line in `main` to see the compiler refuse to add dollars to cents.

```squark editor
program

Cents: int

impl Cents {
	fn dollars(self) -> int {
		return int(self) / 100
	}

	fn rest(self) -> int {
		return int(self) % 100
	}
}

fn main() {
	price := Cents(1999)
	total := price * 3 // an untyped constant takes the type it meets
	println(total.dollars(), "dollars and", total.rest(), "cents")
	tip := 5
	_ = tip
	// total = total + tip
}
```

## Functions as values

A function type is written `fn(int) -> bool`. A value of that type can only be a **named
top-level function**. There are no closures, because a closure keeps captured variables alive
somewhere, and squark has no heap to put them in.

To give a function some context, pass the context in as an argument, or keep it next to the
function in a struct.

```squark editor
program

Op: fn(a, b int) -> int

struct Named {
	name str
	f    Op
}

fn add(a, b int) -> int { return a + b }
fn mul(a, b int) -> int { return a * b }
fn pow(a, b int) -> int {
	r := 1
	for _ in 0..b {
		r *= a
	}
	return r
}

fn main() {
	ops := Named[]{{"add", add}, {"mul", mul}, {"pow", pow}}
	for o in ops {
		println(o.name, o.f(2, 10))
	}
}
```
