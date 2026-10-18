# Basics

The first chapter covers the everyday parts of the language: functions, variables, loops and
decisions.

## Packages and use

Every file starts with a header. A program's files start with `program`, and a library's start
with `module name`. Then come the imports: `use` takes package paths, unquoted.

Here `strings` and `strconv` come from the standard library. Names are `snake_case` and types are
`PascalCase`. Spelling doesn't decide visibility: `pub` does, and you'll see it later.

```squark editor
program

use strings, strconv

fn main() {
	println(strings.has_prefix("squark", "sq"))
	n, err := strconv.atoi("1024")
	if err != nil {
		println("not a number:", err.message())
		return
	}
	println(n * 2)
}
```

> An `error` is a value: a code and a constant message. The Errors chapter shows a shorter way to
> pass one on.

## Functions

`fn` declares a function. Each parameter's type comes after its name, and the results come after
`->`. A function can return several results. Results have types but no names, so a `return`
always lists its values.

```squark editor
program

fn add(a, b int) -> int {
	return a + b
}

fn divmod(a, b int) -> (int, int) {
	return a / b, a % b
}

fn split(total int) -> (int, int) {
	x := total * 4 / 9
	return x, total - x
}

fn main() {
	println(add(42, 13))
	q, r := divmod(17, 5)
	println(q, r)
	x, y := split(17)
	println(x, y)
}
```

## Variables and types

A variable is declared with `var x T`, `var x = e`, or `x := e` inside a function. Types are written
the way they read, and sizes come after them, so `u8[4]` means four `u8`s.

- integers: `int` (64-bit), `i8` to `i64`, `uint`, `u8` to `u64`
- `f32`, `f64`, `bool`, `str` (immutable bytes), `rune` (an `i32`)
- conversions are explicit: `f64(n)`, `int(x)`

```squark editor
program

const greeting = "hi"
const page = 4K // a size literal: 4096

fn main() {
	var count int // zero value: 0
	var ready = true
	ratio := 2.5
	var b u8 = 200
	b += 100 // u8 arithmetic wraps: 300 - 256
	n := int(ratio * 4)
	println(count, ready, ratio, b, n, greeting, page)
}
```

## Loops

squark has more than one loop keyword, so each loop says what it does:

- `for x in xs`: each element, or `for i, x in xs` with the index too
- `for i in 0..n`: a range of numbers
- `for i := 0; i < n; i++`: start, condition and step
- `while cond`: loop while a condition holds
- `loop`: loop until `break` or `return`

Labels let `break` and `continue` reach an outer loop.

```squark editor
program

fn main() {
	for i in 0..4 {
		print(i, " ")
	}
	println()

	for i, x in int[]{10, 20, 30} {
		print(i, "=", x, " ")
	}
	println()

	n := 1
	while n < 100 {
		n *= 2
	}
	println("first power of two over 100:", n)

	tries := 0
	loop {
		tries++
		if tries == 3 {
			break
		}
	}
	println("tries:", tries)

	outer: for y in 1..4 {
		for x in 1..4 {
			if x*y == 6 {
				println("found", x, "*", y)
				break outer
			}
		}
	}
}
```

## if and switch

Conditions use the words `and`, `or` and `not`. `if` can start with a short statement, whose variables live for the `if` and its `else`.
`switch` doesn't fall through, a `case` can list several values, and a `switch` with no value
picks the first case that is true.

```squark editor
program

fn classify(n int) -> str {
	switch {
	case n < 0:
		return "negative"
	case n == 0:
		return "zero"
	case n%2 == 0:
		return "even"
	}
	return "odd"
}

fn main() {
	for n in int[]{-3, 0, 4, 7} {
		println(n, classify(n))
	}

	x := 7
	if v := x * 3; v > 20 and not (x == 3) {
		println("v is", v)
	}

	switch x {
	case 1, 2, 3:
		println("small")
	default:
		println("big")
	}
}
```

## Enums

An `enum` is a named set of numbers. Its members belong to the type
(`Dir.north`). Where the context already says which enum it expects, write just `.north`: in a
`case`, an argument, a comparison or a map key.

An enum is a type of its own, so it can have methods, and it doesn't mix with plain integers
without a conversion.

```squark editor
program

enum Dir { north, east, south, west }

impl Dir {
	fn right(self) -> Dir {
		return Dir((int(self) + 1) % 4)
	}

	fn name(self) -> str {
		switch self {
		case .north:
			return "north"
		case .east:
			return "east"
		case .south:
			return "south"
		}
		return "west"
	}
}

fn main() {
	d := Dir.north
	for _ in 0..5 {
		print(d.name(), " ")
		d = d.right()
	}
	println()
	if d == .east {
		println("facing east again")
	}
}
```

## Habits from other languages
goal: prints "15 FizzBuzz"

This FizzBuzz has four habits from other languages in it. The compiler knows each one and names the squark
spelling: the result type goes after `->`, the logical operators are words, a slice type puts its
brackets after the element, and a loop on a condition is a `while`.

Fix them one at a time. The bar above the editor shows the next error as soon as you fix the one
before it.

```squark editor
program

fn fizzbuzz(n int) str {
	if n%3 == 0 && n%5 == 0 {
		return "FizzBuzz"
	} else if n%3 == 0 {
		return "Fizz"
	} else if n%5 == 0 {
		return "Buzz"
	}
	return ""
}

fn main() {
	var hits []int
	i := 1
	for i <= 15 {
		println(i, fizzbuzz(i))
		i++
	}
	_ = hits
}
```

```squark solution
program

fn fizzbuzz(n int) -> str {
	if n%3 == 0 and n%5 == 0 {
		return "FizzBuzz"
	} else if n%3 == 0 {
		return "Fizz"
	} else if n%5 == 0 {
		return "Buzz"
	}
	return ""
}

fn main() {
	var hits int[]
	i := 1
	while i <= 15 {
		println(i, fizzbuzz(i))
		i++
	}
	_ = hits
}
```
