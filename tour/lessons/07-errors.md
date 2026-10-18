# Errors

An `error` in squark is a value: an integer code and a constant message. It can't be formatted with
text that changes at run time, because that text would need memory from somewhere. So errors are
cheap to create, compare and return.

## Faults

`fault Name code "message"` declares an error constant. Faults compare with `==`, and a
`switch` can pick them apart. `err.code()` and `err.message()` read them.

A function whose results end in `!` also returns an `error`: `-> int !` means `-> (int, error)`.

```squark editor
program

fault NotFound 404 "not found"
fault SoldOut 410 "sold out"

thread stock int[str; 8]

fn lookup(item str) -> int ! {
	n, ok := stock[item]
	if not ok {
		return 0, NotFound
	}
	if n == 0 {
		return 0, SoldOut
	}
	return n, nil
}

fn main() {
	stock["apples"] = 3
	stock["pears"] = 0
	for item in str[]{"apples", "pears", "plums"} {
		n, err := lookup(item)
		switch err {
		case nil:
			println(item, n)
		case NotFound:
			println(item, "->", err.code(), err.message())
		default:
			println(item, "->", err.message())
		}
	}
}
```

## Passing errors on with !

Most error handling just passes the error up. A `!` after a call does that: if the call returned
an error, the enclosing function returns it at once, with zero values for its other results.
Otherwise the expression is the call's other results.

`!` only works in a function that can return an error itself, so `main` checks the error the
usual way.

```squark editor
program

use strconv

fault Negative 1 "negative number"

fn parse_positive(s str) -> int ! {
	n := strconv.atoi(s)!
	if n < 0 {
		return 0, Negative
	}
	return n, nil
}

fn total(xs str[]) -> int ! {
	sum := 0
	for x in xs {
		sum += parse_positive(x)!
	}
	return sum, nil
}

fn show(xs str[]) {
	sum, err := total(xs)
	if err != nil {
		println("error:", err.message())
		return
	}
	println("sum:", sum)
}

fn main() {
	show(str[]{"10", "20", "30"})
	show(str[]{"10", "-4"})
	show(str[]{"10", "ten"})
}
```

## Panics
exit: 2

A panic is for a bug: an index out of range, a full vector, a nil pointer. It prints a message
with the task and the line, then ends the program at once with status 2. There is no
`recover`, and deferred calls don't run.

Run it. The panic names the index, the length and the line.

```squark editor
program

fn main() {
	xs := int[]{1, 2, 3}
	i := len(xs)
	println("about to read index", i)
	println(xs[i])
	println("never printed")
}
```

> A server can't let one bad request end the whole program. A task marked `isolate` turns a panic
> into the end of that task only: its deferred calls run, and the rest of the program carries
> on. The HTTP server's connection task is `isolate`.
